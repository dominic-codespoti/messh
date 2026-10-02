// Package node is the messh runtime: the mutually authenticated mesh server
// peers call, the loopback endpoint local agents and the CLI call, pairing,
// discovery, and peer connection management.
package node

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/approval"
	"messh/internal/buildinfo"
	"messh/internal/catalog"
	"messh/internal/discovery"
	"messh/internal/gateway"
	"messh/internal/grants"
	"messh/internal/identity"
	"messh/internal/jobs"
	"messh/internal/provider"
	"messh/internal/provider/nodeinfo"
	"messh/internal/roster"
	"messh/internal/state"
)

const (
	DefaultMeshAddr  = ":7519"
	DefaultLocalAddr = "127.0.0.1:7520"
	defaultMeshPort  = 7519

	onlineWindow  = 90 * time.Second // verified contact this recent counts as online
	probeInterval = 45 * time.Second // re-verify peers quieter than this
	toolsMaxAge   = 2 * time.Minute  // refetch peer tool lists older than this (services come and go)
	sightingTTL   = 10 * time.Minute
)

// Options configures a node.
type Options struct {
	Paths           state.Paths
	Name            string // overrides the stored name; empty keeps it (first run: hostname)
	MeshAddr        string // TCP listen address for peers; default DefaultMeshAddr
	LocalAddr       string // loopback listen address for agents and the CLI; default DefaultLocalAddr
	Discovery       bool
	DiscoveryIfaces []string       // nil = automatic interface selection
	DiscoveryGroup  netip.AddrPort // zero = discovery.DefaultGroup
	Logger          *slog.Logger
	ApprovalSurface approval.Surface // how approval prompts are shown; nil = the platform's default
}

// Node is a running messh node.
type Node struct {
	opts        Options
	log         *slog.Logger
	paths       state.Paths
	id          *identity.Identity
	name        string
	build       buildinfo.Info
	started     time.Time
	executable  string
	maintenance maintenanceState

	roster     *roster.Roster
	gateway    *gateway.Gateway
	pairing    *pairing
	peers      *peerSet
	remoteJobs *remoteJobOutbox
	files      *fileService
	grants     *grants.Store
	jobs       *jobs.Provider
	browser    browserControl // the browser provider's control surface (stop, status)

	catalog *catalog.Provider // the service catalogue, nil if it failed to start; see llm.go

	sched *scheduler // stored calls of this node's agents, nil if scheduling is off; see schedule.go

	wakes *wakeState // Wake-on-LAN: peers' sleep state, probe backoff, wake info; see wake.go

	approvals *approval.Engine // gates every non-info tool call; see dispatch

	toolsMu   sync.RWMutex
	providers []provider.Provider
	tools     map[string]localTool // tool name → owner; rebuilt by rebuildTools
	toolOrder []string
	peerSrv   *mcp.Server     // serves tools to paired peers
	peerNames map[string]bool // tool names currently registered on peerSrv

	sightMu   sync.Mutex
	sightings map[string]discovery.Sighting

	controlToken string
	meshLn       net.Listener
	localLn      net.Listener
	meshSrv      *http.Server
	localSrv     *http.Server

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	done   chan struct{}
}

// Start brings a node up. It stops when ctx is cancelled or Close is called.
func Start(ctx context.Context, opts Options) (*Node, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	if opts.MeshAddr == "" {
		opts.MeshAddr = DefaultMeshAddr
	}
	if opts.LocalAddr == "" {
		opts.LocalAddr = DefaultLocalAddr
	}

	cfg, err := opts.Paths.LoadConfig()
	if err != nil {
		return nil, err
	}
	name := cmp.Or(opts.Name, cfg.Name, defaultName())
	if !ValidName(name) {
		return nil, fmt.Errorf("invalid device name %q: use 1-64 printable characters", name)
	}
	if cfg.Name != name {
		cfg.Name = name
		if err := opts.Paths.SaveConfig(cfg); err != nil {
			return nil, err
		}
	}
	id, err := identity.LoadOrCreate(opts.Paths.IdentityDir(), name)
	if err != nil {
		return nil, err
	}
	ro, err := roster.Load(opts.Paths.RosterFile())
	if err != nil {
		return nil, fmt.Errorf("load peers: %w", err)
	}
	tok, err := opts.Paths.ControlToken()
	if err != nil {
		return nil, err
	}
	approvals, err := newApprovalEngine(opts, log)
	if err != nil {
		return nil, err
	}

	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate running executable: %w", err)
	}
	startupUpdate, err := loadStartupUpdate(opts.Paths, id.ID, executable, time.Now())
	if err != nil {
		return nil, fmt.Errorf("validate startup update: %w", err)
	}

	if err := requireLoopback(opts.LocalAddr); err != nil {
		return nil, err
	}
	meshLn, err := net.Listen("tcp", opts.MeshAddr)
	if err != nil {
		return nil, fmt.Errorf("listen for peers on %s: %w", opts.MeshAddr, err)
	}
	localLn, err := net.Listen("tcp", opts.LocalAddr)
	if err != nil {
		meshLn.Close()
		return nil, fmt.Errorf("listen for agents on %s: %w", opts.LocalAddr, err)
	}

	ctx, cancel := context.WithCancel(ctx)
	n := &Node{
		opts:         opts,
		log:          log,
		paths:        opts.Paths,
		id:           id,
		name:         name,
		build:        buildinfo.Current(),
		started:      time.Now(),
		roster:       ro,
		tools:        map[string]localTool{},
		pairing:      newPairing(),
		sightings:    map[string]discovery.Sighting{},
		controlToken: tok,
		approvals:    approvals,
		meshLn:       lanOnly{Listener: meshLn, log: log},
		localLn:      localLn,
		ctx:          ctx,
		cancel:       cancel,
		done:         make(chan struct{}),
		executable:   executable,
	}
	if startupUpdate != nil {
		n.maintenance.lease = startupUpdate.Lease
		n.maintenance.expires = startupUpdate.Expires
		n.maintenance.paths = opts.Paths
		n.maintenance.startup = startupUpdate
	}
	n.peers = newPeerSet(n)
	n.wakes = newWakeState() // before the gateway: Nodes() reads sleep state
	n.peerSrv = mcp.NewServer(&mcp.Implementation{Name: "messh-node", Version: n.build.Version}, &mcp.ServerOptions{
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
	})
	n.register(&nodeinfo.Provider{DeviceID: id.ID, DeviceName: name, Build: n.build})
	if err := n.startGrants(); err != nil {
		cancel()
		meshLn.Close()
		localLn.Close()
		approvals.Close()
		return nil, fmt.Errorf("load capability grants: %w", err)
	}
	n.remoteJobs, err = newRemoteJobOutbox(n)
	if err != nil {
		cancel()
		meshLn.Close()
		localLn.Close()
		approvals.Close()
		return nil, err
	}
	n.roster.OnChange(n.remoteJobs.pairedChanged)
	n.startJobs()
	n.remoteJobs.start()
	n.startCatalog()
	n.startBrowser()
	n.rebuildTools()
	n.gateway = gateway.New(n, n.build.Version, log.With("component", "gateway"))
	ro.OnChange(n.gateway.Sync)
	if err := n.startFiles(); err != nil {
		cancel()
		meshLn.Close()
		localLn.Close()
		return nil, err
	}
	n.startWake()
	n.startSchedules()

	errLog := slog.NewLogLogger(log.With("component", "http").Handler(), slog.LevelDebug)
	n.meshSrv = &http.Server{
		Handler:           n.meshHandler(),
		TLSConfig:         n.serverTLS(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          errLog,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}
	n.localSrv = &http.Server{
		Handler:           n.localHandler(),
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          errLog,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	n.goRun(func() { n.serve("mesh", n.meshSrv.ServeTLS(n.meshLn, "", "")) })
	n.goRun(func() { n.serve("local", n.localSrv.Serve(n.localLn)) })
	if opts.Discovery {
		n.goRun(n.runDiscovery)
	}
	n.goRun(n.maintain)

	if err := n.paths.SaveRunInfo(state.RunInfo{
		PID: os.Getpid(), ID: id.ID, Name: name,
		Mesh: n.meshLn.Addr().String(), Local: n.localLn.Addr().String(), Started: n.started,
		Executable: n.executable,
	}); err != nil {
		cancel()
		n.shutdown()
		return nil, err
	}
	go func() {
		<-ctx.Done()
		n.shutdown()
		close(n.done)
	}()
	return n, nil
}

func (n *Node) ID() string        { return n.id.ID }
func (n *Node) Name() string      { return n.name }
func (n *Node) MeshAddr() string  { return n.meshLn.Addr().String() }
func (n *Node) LocalAddr() string { return n.localLn.Addr().String() }

// Close stops the node and waits for shutdown to finish.
func (n *Node) Close() {
	n.cancel()
	<-n.done
}

// Done is closed once the node has shut down.
func (n *Node) Done() <-chan struct{} { return n.done }

func (n *Node) goRun(fn func()) {
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		fn()
	}()
}

func (n *Node) serve(which string, err error) {
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		n.log.Error("server stopped", "server", which, "error", err)
		n.cancel()
	}
}

func (n *Node) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	n.approvals.Close() // before the servers: it releases calls blocked on a prompt
	n.meshSrv.Shutdown(ctx)
	n.localSrv.Shutdown(ctx)
	n.wg.Wait()
	n.peers.closeAll()
	if run, err := n.paths.LoadRunInfo(); err == nil && run.PID == os.Getpid() && run.Local == n.LocalAddr() {
		if err := n.paths.RemoveRunInfo(); err != nil {
			n.log.Warn("remove run file", "error", err)
		}
	}
}

// Nodes implements gateway.Backend.
func (n *Node) Nodes() []gateway.Node {
	now := time.Now()
	out := []gateway.Node{{
		ID: n.id.ID, Name: n.name, Self: true, Online: true, LastSeen: now,
		Addr: n.MeshAddr(), Tools: n.localDefs(),
	}}
	for _, p := range n.roster.List() {
		contact := n.peers.lastContact(p.ID)
		last := p.LastSeen
		if contact.After(last) {
			last = contact
		}
		var addr string
		if c := n.peers.candidates(p.ID); len(c) > 0 {
			addr = c[0]
		}
		asleep, _ := n.sleepState(p)
		out = append(out, gateway.Node{
			ID: p.ID, Name: p.Name, Online: !asleep && now.Sub(contact) < onlineWindow, Asleep: asleep,
			LastSeen: last, Addr: addr, Tools: p.Tools,
		})
	}
	return out
}

// Call implements gateway.Backend.
func (n *Node) Call(ctx context.Context, deviceID, tool string, args json.RawMessage, agent string) (*mcp.CallToolResult, error) {
	if !n.beginWork() {
		return provider.ErrorResult("node is preparing for an update"), nil
	}
	defer n.endWork()
	if deviceID == n.id.ID {
		return n.dispatch(ctx, tool, args, provider.Caller{DeviceID: n.id.ID, DeviceName: n.name, Agent: agent}), nil
	}
	if strings.HasPrefix(tool, "job_") && n.remoteJobs != nil {
		return n.remoteJobs.call(ctx, deviceID, tool, args, agent)
	}
	if res := n.beforePeerCall(ctx, deviceID); res != nil {
		return res, nil
	}
	return n.peers.call(ctx, deviceID, tool, args, agent)
}

func (n *Node) meshPort() int {
	if a, ok := n.meshLn.Addr().(*net.TCPAddr); ok {
		return a.Port
	}
	return defaultMeshPort
}

func (n *Node) runDiscovery() {
	cfg := discovery.Config{
		Group:      n.opts.DiscoveryGroup,
		Self:       discovery.Announcement{ID: n.id.ID, Name: n.name, Port: n.meshPort()},
		Interfaces: n.opts.DiscoveryIfaces,
		OnSighting: n.onSighting,
		Logger:     n.log, // discovery tags its own records with component=discovery
		Sleep:      n.wakes.announce,
	}
	for {
		err := discovery.Run(n.ctx, cfg)
		if n.ctx.Err() != nil {
			return
		}
		n.log.Warn("discovery stopped; retrying in 30s", "error", err)
		select {
		case <-n.ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}

// onSighting records an announcement. Announcements are unauthenticated, so
// they only suggest addresses; a peer counts as reachable once a pinned TLS
// connection to it succeeds.
func (n *Node) onSighting(s discovery.Sighting) {
	n.sightMu.Lock()
	n.sightings[s.ID] = s
	n.sightMu.Unlock()
	p, ok := n.roster.Get(s.ID)
	if !ok {
		return
	}
	if !n.noteSighting(p, s) {
		return // going to sleep: no probe
	}
	quiet := time.Since(n.peers.lastContact(s.ID)) > probeInterval
	stale := p.ToolsFetched.IsZero() || time.Since(p.ToolsFetched) > toolsMaxAge
	if quiet || stale {
		n.probePeer(s.ID)
	}
}

func (n *Node) sighting(id string) (discovery.Sighting, bool) {
	n.sightMu.Lock()
	defer n.sightMu.Unlock()
	s, ok := n.sightings[id]
	return s, ok
}

func (n *Node) sightingList() []discovery.Sighting {
	n.sightMu.Lock()
	out := make([]discovery.Sighting, 0, len(n.sightings))
	for _, s := range n.sightings {
		out = append(out, s)
	}
	n.sightMu.Unlock()
	slices.SortFunc(out, func(a, b discovery.Sighting) int { return strings.Compare(a.Name, b.Name) })
	return out
}

func (n *Node) maintain() {
	first := time.NewTimer(time.Second)
	defer first.Stop()
	tick := time.NewTicker(probeInterval)
	defer tick.Stop()
	for {
		select {
		case <-n.ctx.Done():
			return
		case <-first.C:
		case <-tick.C:
		}
		now := time.Now()
		for _, p := range n.roster.List() {
			quiet := now.Sub(n.peers.lastContact(p.ID)) > probeInterval
			stale := p.ToolsFetched.IsZero() || now.Sub(p.ToolsFetched) > toolsMaxAge
			if quiet || stale {
				n.probePeer(p.ID) // skips asleep peers and backs off failing ones
			}
		}
		n.pairing.gc(now)
		n.sightMu.Lock()
		for id, s := range n.sightings {
			if now.Sub(s.Seen) > sightingTTL {
				delete(n.sightings, id)
			}
		}
		n.sightMu.Unlock()
	}
}

// ValidName reports whether name is usable as a device name.
func ValidName(name string) bool {
	if name == "" || len(name) > 64 || strings.TrimSpace(name) != name {
		return false
	}
	for _, r := range name {
		if !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func defaultName() string {
	h, err := os.Hostname()
	if err != nil || h == "" {
		return "device"
	}
	h, _, _ = strings.Cut(strings.ToLower(h), ".")
	return h
}

func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("local address %q: %w", addr, err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsLoopback() {
		return fmt.Errorf("local address %q must be a loopback IP; agents and the CLI connect there without TLS", addr)
	}
	return nil
}
