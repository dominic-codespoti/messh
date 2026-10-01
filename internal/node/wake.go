package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/control"
	"messh/internal/discovery"
	"messh/internal/identity"
	"messh/internal/provider"
	"messh/internal/roster"
	"messh/internal/wol"
)

const (
	wakeInfoPath     = "/v1/wake-info"
	goingToSleepPath = "/v1/going-to-sleep"

	wakeInfoMaxAge  = 10 * time.Minute // refetch a peer's wake info at most this often
	wakeInfoRetry   = time.Minute      // wait after a failed fetch
	defaultWakeWait = 60 * time.Second
	maxWakeWait     = 300 * time.Second
	wakeProbeEvery  = 2 * time.Second  // while waking: try to reach the peer this often
	freshContact    = 5 * time.Second  // verified contact this recent means awake without re-checking
	liveCheckTime   = 2 * time.Second  // budget for re-verifying a quiet "online" peer before waking it
	wakeResendEvery = 15 * time.Second // while waking: repeat the magic packets this often
	maxProbeBackoff = 10 * time.Minute
	sleepNoticeTime = 1500 * time.Millisecond // budget for telling peers before this device suspends
)

// wakeState is the node's Wake-on-LAN bookkeeping.
//
// A peer is asleep when it said so after it was last heard from. The
// authenticated notice (POST /v1/going-to-sleep) is stored in the roster;
// the multicast one is an unauthenticated hint kept only in memory (hinted)
// that pauses background probes and nothing else. heard holds the last
// awake discovery announcement; fails counts background probes failing in
// a row, for backoff.
type wakeState struct {
	announce chan bool // to discovery: true = going to sleep, false = resumed

	mu       sync.Mutex
	heard    map[string]time.Time
	hinted   map[string]time.Time
	fails    map[string]probeFail
	probing  map[string]bool
	fetching map[string]bool
	fetchTry map[string]time.Time

	// Replaceable by tests; guarded by mu. A nil waker means Node.Wake.
	localTargets func() ([]wol.Target, error)
	send         func(context.Context, []wol.Target) (wol.Report, error)
	onProbe      func(id string)
	waker        func(ctx context.Context, id string, wait time.Duration) error
}

type probeFail struct {
	count int
	at    time.Time // last failure
	next  time.Time // no background probe before this
}

func newWakeState() *wakeState {
	return &wakeState{
		announce:     make(chan bool, 1),
		heard:        map[string]time.Time{},
		hinted:       map[string]time.Time{},
		fails:        map[string]probeFail{},
		probing:      map[string]bool{},
		fetching:     map[string]bool{},
		fetchTry:     map[string]time.Time{},
		localTargets: wol.LocalTargets,
		send:         (&wol.Sender{}).Send,
	}
}

// startWake wires Wake-on-LAN: the mesh_wake tool, wake-info fetches after
// successful refreshes, and this device's own sleep announcements. It needs
// n.gateway and n.files.
func (n *Node) startWake() {
	n.roster.OnChange(n.fetchWakeInfoAll)
	n.gateway.AddTool(meshWakeTool(), n.meshWake)
	n.goRun(n.watchSleep)
}

// mountWake serves wake info and sleep notices to paired peers only.
func (n *Node) mountWake(mux *http.ServeMux) {
	mux.Handle("GET "+wakeInfoPath, n.requirePeer(http.HandlerFunc(n.handleWakeInfo)))
	mux.Handle("POST "+goingToSleepPath, n.requirePeer(http.HandlerFunc(n.handleGoingToSleep)))
}

func (n *Node) registerWakeAPI(api *http.ServeMux) {
	api.HandleFunc("POST /v1/wake/{peer}", n.apiWake)
}

// probeBackoff is the pause after fails background probes failed in a row:
// none after one (the normal interval applies), then doubling from
// 2×probeInterval up to maxProbeBackoff.
func probeBackoff(fails int) time.Duration {
	if fails < 2 {
		return 0
	}
	if fails > 10 {
		return maxProbeBackoff
	}
	return min(probeInterval<<(fails-1), maxProbeBackoff)
}

// heardAt is the latest sign of life from a peer: verified contact, a
// remembered successful address, or an awake discovery announcement.
func (n *Node) heardAt(p roster.Peer) time.Time {
	t := p.LastSeen
	if c := n.peers.lastContact(p.ID); c.After(t) {
		t = c
	}
	n.wakes.mu.Lock()
	h := n.wakes.heard[p.ID]
	n.wakes.mu.Unlock()
	if h.After(t) {
		t = h
	}
	return t
}

// sleepState reports whether p is asleep: it said so after it was last
// heard from. confirmed means it said so over the authenticated mesh; an
// unconfirmed (multicast) notice only pauses background probes.
func (n *Node) sleepState(p roster.Peer) (asleep, confirmed bool) {
	heard := n.heardAt(p)
	n.wakes.mu.Lock()
	hint := n.wakes.hinted[p.ID]
	n.wakes.mu.Unlock()
	confirmed = !p.Asleep.IsZero() && p.Asleep.After(heard)
	if !p.Asleep.IsZero() && !confirmed {
		n.clearAsleep(p.ID) // heard from since: drop the stale notice from the file
	}
	return confirmed || hint.After(heard), confirmed
}

// peerState is "asleep", "online" or "offline", as agents and the CLI see it.
func (n *Node) peerState(p roster.Peer) string {
	if asleep, _ := n.sleepState(p); asleep {
		return "asleep"
	}
	if time.Since(n.peers.lastContact(p.ID)) < onlineWindow {
		return "online"
	}
	return "offline"
}

// markAsleep records that a peer is going to sleep, stamped after its last
// sign of life (the notice itself counts as contact).
func (n *Node) markAsleep(id string, confirmed bool) {
	p, ok := n.roster.Get(id)
	if !ok {
		return
	}
	at := time.Now()
	if h := n.heardAt(p); !at.After(h) {
		at = h.Add(time.Nanosecond)
	}
	if confirmed {
		if err := n.roster.SetAsleep(id, at); err != nil {
			n.log.Warn("save peer sleep state", "error", err)
		}
		return
	}
	n.wakes.mu.Lock()
	n.wakes.hinted[id] = at
	n.wakes.mu.Unlock()
}

func (n *Node) clearAsleep(id string) {
	if err := n.roster.SetAsleep(id, time.Time{}); err != nil {
		n.log.Warn("save peer sleep state", "error", err)
	}
}

// noteSighting applies a discovery announcement's sleep flag and reports
// whether the peer says it is awake. Announcements are unauthenticated, so
// a sleeping one only pauses background probes until the next awake
// announcement or contact: it neither fails calls nor touches stored state.
func (n *Node) noteSighting(p roster.Peer, s discovery.Sighting) bool {
	asleep, _ := n.sleepState(p)
	if s.Sleeping {
		if !asleep {
			n.log.Info("peer announced it is going to sleep; pausing probes", "peer", p.Name)
			n.markAsleep(p.ID, false)
		}
		return false
	}
	n.wakes.mu.Lock()
	if s.Seen.After(n.wakes.heard[p.ID]) {
		n.wakes.heard[p.ID] = s.Seen
	}
	if asleep {
		delete(n.wakes.fails, p.ID) // it is back: probe now, not at the backoff deadline
	}
	n.wakes.mu.Unlock()
	if asleep {
		n.log.Info("peer is awake again", "peer", p.Name)
		n.clearAsleep(p.ID)
	}
	return true
}

// probePeer re-verifies a peer in the background (a tool refresh) unless it is
// asleep or its recent probes failed. Failures back off exponentially up to
// maxProbeBackoff, so a peer that slept without telling anyone is not
// hammered: with wake-on-pattern NICs every probe could wake it or keep it awake.
func (n *Node) probePeer(id string) {
	p, ok := n.roster.Get(id)
	if !ok {
		return
	}
	if asleep, _ := n.sleepState(p); asleep {
		return
	}
	contact := n.peers.lastContact(id)
	ws := n.wakes
	ws.mu.Lock()
	f := ws.fails[id]
	if f.count > 0 && contact.After(f.at) {
		delete(ws.fails, id) // it made contact since: back to normal
		f = probeFail{}
	}
	if ws.probing[id] || time.Now().Before(f.next) {
		ws.mu.Unlock()
		return
	}
	ws.probing[id] = true
	hook := ws.onProbe
	ws.mu.Unlock()
	if hook != nil {
		hook(id)
	}
	go func() {
		ctx, cancel := context.WithTimeout(n.ctx, 10*time.Second)
		err := n.peers.refresh(ctx, id)
		cancel()
		ws.mu.Lock()
		defer ws.mu.Unlock()
		delete(ws.probing, id)
		if n.ctx.Err() != nil {
			return
		}
		if err == nil {
			delete(ws.fails, id)
			return
		}
		f := ws.fails[id]
		f.count++
		f.at = time.Now()
		f.next = f.at.Add(probeBackoff(f.count))
		ws.fails[id] = f
		n.log.Debug("peer probe failed", "peer", identity.Short(id), "failures", f.count, "next_in", probeBackoff(f.count), "error", err)
	}()
}

// beforePeerCall applies sleep state to an explicit tool call. With
// auto-wake on, an asleep or offline peer is woken first; without it, a peer
// that confirmed it is asleep fails fast instead of after dial timeouts.
// nil means go ahead.
func (n *Node) beforePeerCall(ctx context.Context, id string) *mcp.CallToolResult {
	p, ok := n.roster.Get(id)
	if !ok {
		return nil // the call reports it
	}
	_, confirmed := n.sleepState(p)
	if p.AutoWake {
		state := n.peerState(p)
		if state == "online" && time.Since(n.peers.lastContact(id)) < freshContact {
			return nil
		}
		n.wakes.mu.Lock()
		waker := n.wakes.waker
		n.wakes.mu.Unlock()
		if waker == nil {
			waker = n.Wake
		}
		err := waker(ctx, id, defaultWakeWait)
		if err == nil || !confirmed {
			// Merely quiet (say, this node just restarted): the call itself
			// still gets its chance and reports any failure.
			return nil
		}
		return provider.ErrorResult("%s is %s and auto-wake failed: %v", p.Name, state, err)
	}
	if confirmed {
		return provider.ErrorResult("%s is asleep; call mesh_wake first (or turn on auto-wake: messh wake %s --auto on)", p.Name, p.Name)
	}
	return nil
}

// Wake makes sure a paired peer is awake. It returns nil at once if the peer
// is online; otherwise it sends Wake-on-LAN magic packets to every MAC known
// for it and waits up to wait (<= 0: 60 s) for verified contact. It fails
// when no MAC is known or the wait runs out.
func (n *Node) Wake(ctx context.Context, deviceID string, wait time.Duration) error {
	if deviceID == n.id.ID {
		return nil
	}
	_, err := n.wakePeer(ctx, deviceID, wait)
	return err
}

// awakeNow reports whether a peer is verifiably awake right now: verified
// contact within freshContact, or a tool refresh succeeding within
// liveCheckTime. onlineWindow alone is too coarse: a peer that slept without
// announcing it still looks online for that long.
func (n *Node) awakeNow(ctx context.Context, id string) bool {
	if time.Since(n.peers.lastContact(id)) < freshContact {
		return true
	}
	cctx, cancel := context.WithTimeout(ctx, liveCheckTime)
	defer cancel()
	return n.peers.refresh(cctx, id) == nil
}

func (n *Node) wakePeer(ctx context.Context, id string, wait time.Duration) (control.WakeResult, error) {
	p, ok := n.roster.Get(id)
	if !ok {
		return control.WakeResult{ID: id}, fmt.Errorf("device %s is not paired", identity.Short(id))
	}
	res := control.WakeResult{Device: p.Name, ID: p.ID, AutoWake: p.AutoWake, Before: n.peerState(p)}
	if res.Before == "online" {
		if n.awakeNow(ctx, id) {
			res.Awake = true
			return res, nil
		}
		res.Before = "quiet" // within onlineWindow, but not answering now
	}
	if len(p.Wake) == 0 {
		return res, fmt.Errorf("no MAC address known for %s: messh learns it over the authenticated mesh, so %s must have been "+
			"reachable at least once since pairing (running a messh with wake-on-LAN support)", p.Name, p.Name)
	}
	if wait <= 0 {
		wait = defaultWakeWait
	}
	wait = min(wait, maxWakeWait)

	n.wakes.mu.Lock()
	send := n.wakes.send
	n.wakes.mu.Unlock()
	start := time.Now()
	rep, err := send(ctx, p.Wake)
	res.MACs, res.Routes, res.NoSharedSubnet = rep.MACs, rep.Routes, rep.NoSharedSubnet
	res.PacketsSent, res.PacketsFailed = rep.Sent, rep.Failed
	if err != nil {
		return res, fmt.Errorf("send magic packets: %w", err)
	}
	n.log.Info("sent wake-on-lan packets", "peer", p.Name, "macs", strings.Join(rep.MACs, ","), "packets", rep.Sent, "routes", len(rep.Routes))

	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(250 * time.Millisecond)
	defer tick.Stop()
	var probing atomic.Bool
	var lastProbe time.Time
	lastSend := start
	for {
		if n.peers.lastContact(id).After(start) {
			n.wakes.mu.Lock()
			delete(n.wakes.fails, id)
			delete(n.wakes.hinted, id)
			n.wakes.mu.Unlock()
			n.clearAsleep(id)
			res.Awake = true
			res.Seconds = round(time.Since(start).Seconds(), 1)
			n.log.Info("peer is awake", "peer", p.Name, "seconds", res.Seconds)
			return res, nil
		}
		now := time.Now()
		// Explicit probes bypass the sleep and backoff gates: waking is the point.
		if now.Sub(lastProbe) >= wakeProbeEvery && probing.CompareAndSwap(false, true) {
			lastProbe = now
			go func() {
				defer probing.Store(false)
				pctx, cancel := context.WithTimeout(ctx, 4*time.Second)
				defer cancel()
				n.peers.refresh(pctx, id)
			}()
		}
		if now.Sub(lastSend) >= wakeResendEvery {
			lastSend = now
			r, _ := send(ctx, p.Wake)
			res.PacketsSent += r.Sent
			res.PacketsFailed += r.Failed
		}
		select {
		case <-ctx.Done():
			return res, ctx.Err()
		case <-deadline.C:
			return res, fmt.Errorf("%s did not answer within %s after %d magic packets to %s: check that Wake-on-LAN is enabled "+
				"on it and that it sleeps (S3) rather than being off or on another network", p.Name, wait, res.PacketsSent, strings.Join(res.MACs, ", "))
		case <-tick.C:
		}
	}
}

func meshWakeTool() *mcp.Tool {
	return &mcp.Tool{
		Name: "mesh_wake",
		Description: "Wake a sleeping device on the mesh with Wake-on-LAN and wait until it answers. Call it when mesh_nodes " +
			"shows a device as asleep or offline, before using its tools (tool calls to an asleep device fail with " +
			"'call mesh_wake first'). A device that is already online returns at once. Returns its state before " +
			"(online, quiet, asleep or offline; quiet: recently seen but not answering now), whether it is awake now, the MAC addresses used (partly masked), how many " +
			"magic packets were sent on which routes, and the seconds it took to wake. Needs Wake-on-LAN enabled on " +
			"the target, which must have been reachable at least once since pairing so its MACs are known.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"device":{"type":"string","description":"device handle or name from mesh_nodes"},` +
			`"wait_seconds":{"type":"integer","minimum":1,"maximum":300,"description":"how long to wait for the device to answer (default 60, max 300)"}},` +
			`"required":["device"],"additionalProperties":false}`),
		Annotations: &mcp.ToolAnnotations{Title: "Wake a device", IdempotentHint: true, DestructiveHint: new(bool), OpenWorldHint: new(bool)},
	}
}

type wakeArgs struct {
	Device      string `json:"device"`
	WaitSeconds *int   `json:"wait_seconds"`
}

func (n *Node) meshWake(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a wakeArgs
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &a); err != nil {
			return provider.ErrorResult("mesh_wake: invalid arguments: %v", err), nil
		}
	}
	if strings.TrimSpace(a.Device) == "" {
		return provider.ErrorResult("mesh_wake: device is required (a handle or name from mesh_nodes)"), nil
	}
	id, _, err := n.files.resolveDevice(a.Device)
	if err != nil {
		return provider.ErrorResult("mesh_wake: %v", err), nil
	}
	if id == n.id.ID {
		return provider.JSONResult(control.WakeResult{Device: n.name, ID: id, Before: "online", Awake: true})
	}
	wait := defaultWakeWait
	if a.WaitSeconds != nil {
		wait = time.Duration(min(max(*a.WaitSeconds, 1), int(maxWakeWait/time.Second))) * time.Second
	}
	res, err := n.wakePeer(ctx, id, wait)
	if err != nil {
		res.Error = err.Error()
		out, jerr := provider.JSONResult(res)
		if jerr != nil {
			return nil, jerr
		}
		out.IsError = true
		return out, nil
	}
	return provider.JSONResult(res)
}

func (n *Node) apiWake(w http.ResponseWriter, r *http.Request) {
	var req control.WakeRequest
	if !readJSON(w, r, &req) {
		return
	}
	p, err := resolvePeer(n.roster.List(), r.PathValue("peer"))
	if err != nil {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if req.Auto != nil {
		if _, err := n.roster.SetAutoWake(p.ID, *req.Auto); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		state := n.peerState(p)
		writeJSON(w, http.StatusOK, control.WakeResult{Device: p.Name, ID: p.ID, Before: state, Awake: state == "online", AutoWake: *req.Auto})
		return
	}
	wait := defaultWakeWait
	if req.WaitSeconds > 0 {
		wait = min(time.Duration(req.WaitSeconds)*time.Second, maxWakeWait)
	}
	res, err := n.wakePeer(r.Context(), p.ID, wait)
	if err != nil {
		res.Error = err.Error()
	}
	writeJSON(w, http.StatusOK, res)
}

// wakeInfo is the body of GET /v1/wake-info.
type wakeInfo struct {
	Targets []wol.Target `json:"targets"`
}

func (n *Node) handleWakeInfo(w http.ResponseWriter, _ *http.Request) {
	n.wakes.mu.Lock()
	list := n.wakes.localTargets
	n.wakes.mu.Unlock()
	ts, err := list()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list network interfaces: "+err.Error())
		return
	}
	if ts == nil {
		ts = []wol.Target{}
	}
	writeJSON(w, http.StatusOK, wakeInfo{Targets: ts})
}

func (n *Node) handleGoingToSleep(w http.ResponseWriter, r *http.Request) {
	n.markAsleep(r.Header.Get(hdrPeerID), true)
	n.log.Info("peer is going to sleep; pausing probes until it is heard again", "peer", r.Header.Get(hdrPeerName))
	w.WriteHeader(http.StatusNoContent)
}

// fetchWakeInfoAll runs after roster changes, which every successful tool
// refresh ends in: it fetches wake info from peers in recent contact whose
// copy is older than wakeInfoMaxAge.
func (n *Node) fetchWakeInfoAll() {
	now := time.Now()
	ws := n.wakes
	for _, p := range n.roster.List() {
		if now.Sub(n.peers.lastContact(p.ID)) > onlineWindow || now.Sub(p.WakeFetched) < wakeInfoMaxAge {
			continue
		}
		ws.mu.Lock()
		skip := ws.fetching[p.ID] || now.Sub(ws.fetchTry[p.ID]) < wakeInfoRetry
		if !skip {
			ws.fetching[p.ID] = true
			ws.fetchTry[p.ID] = now
		}
		ws.mu.Unlock()
		if !skip {
			go n.fetchWakeInfo(p.ID)
		}
	}
}

// fetchWakeInfo learns a peer's MACs over its pinned mTLS channel; MACs are
// never taken from unauthenticated discovery packets.
func (n *Node) fetchWakeInfo(id string) {
	defer func() {
		n.wakes.mu.Lock()
		delete(n.wakes.fetching, id)
		n.wakes.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(n.ctx, 10*time.Second)
	defer cancel()
	tr := n.pinnedTransport(id)
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}
	var lastErr error
	for _, addr := range n.peers.candidates(id) {
		var info wakeInfo
		lastErr = getJSON(ctx, client, "https://"+addr+wakeInfoPath, &info)
		if lastErr == nil {
			now := time.Now()
			n.peers.markContact(id, now)
			targets := wol.Sanitize(info.Targets)
			if err := n.roster.SetWake(id, targets, now); err != nil {
				n.log.Warn("save peer wake info", "error", err)
			}
			n.log.Debug("learned wake info", "peer", identity.Short(id), "interfaces", len(targets))
			return
		}
		if ctx.Err() != nil {
			break
		}
	}
	if lastErr != nil && n.ctx.Err() == nil {
		n.log.Debug("fetch wake info failed", "peer", identity.Short(id), "error", lastErr)
	}
}

func getJSON(ctx context.Context, client *http.Client, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(out)
}

// watchPower is the OS sleep notification source; tests replace it so test
// nodes never register for real power events or spawn logind monitors.
var watchPower = wol.WatchSleep

// watchSleep announces this device's own sleep for as long as the node runs.
func (n *Node) watchSleep() {
	err := watchPower(n.ctx, n.log.With("component", "sleep"), n.onPowerEvent)
	if err != nil && n.ctx.Err() == nil {
		n.log.Info("sleep announcements disabled; peers fall back to probe backoff", "error", err)
	}
}

func (n *Node) onPowerEvent(ev wol.PowerEvent) {
	switch ev {
	case wol.Suspending:
		n.log.Info("going to sleep; telling peers")
		n.announceSleep()
	case wol.Resumed:
		n.log.Info("resumed from sleep")
		n.signalDiscovery(false)
		n.wakes.mu.Lock()
		clear(n.wakes.fails) // failures while this device slept say nothing about its peers
		n.wakes.mu.Unlock()
		for _, p := range n.roster.List() {
			n.probePeer(p.ID) // contact also tells each peer this device is back
		}
	}
}

// signalDiscovery hands the sleep flag to discovery; the latest value wins.
func (n *Node) signalDiscovery(sleeping bool) {
	select {
	case <-n.wakes.announce:
	default:
	}
	select {
	case n.wakes.announce <- sleeping:
	default:
	}
}

// announceSleep tells the LAN (multicast hint) and every peer in recent
// contact (authenticated notice) that this device is about to sleep, within
// sleepNoticeTime.
func (n *Node) announceSleep() {
	n.signalDiscovery(true)
	ctx, cancel := context.WithTimeout(n.ctx, sleepNoticeTime)
	defer cancel()
	var wg sync.WaitGroup
	for _, p := range n.roster.List() {
		if time.Since(n.peers.lastContact(p.ID)) > onlineWindow {
			continue
		}
		wg.Go(func() { n.tellGoingToSleep(ctx, p.ID) })
	}
	wg.Wait()
}

func (n *Node) tellGoingToSleep(ctx context.Context, id string) {
	tr := n.pinnedTransport(id)
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr}
	for _, addr := range n.peers.candidates(id) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://"+addr+goingToSleepPath, nil)
		if err != nil {
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			continue
		}
		resp.Body.Close()
		return
	}
}
