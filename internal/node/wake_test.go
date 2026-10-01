package node

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/control"
	"messh/internal/discovery"
	"messh/internal/roster"
	"messh/internal/wol"
)

func init() {
	// Test nodes must not hook into this machine's real suspend/resume.
	watchPower = func(ctx context.Context, _ *slog.Logger, _ func(wol.PowerEvent)) error {
		<-ctx.Done()
		return nil
	}
}

var desktopNICs = []wol.Target{
	{MAC: "a4:bb:6d:12:34:56", Interface: "Ethernet", Subnets: []wol.Subnet{
		{Prefix: netip.MustParsePrefix("192.168.1.20/24"), Broadcast: netip.MustParseAddr("192.168.1.255")},
	}},
	{MAC: "a4:bb:6d:12:34:57", Interface: "Wi-Fi"},
}

// wakePair pairs a desktop that reports nics with a raspi and waits until
// both learned each other's wake info and nothing is in flight, so later
// contact is only what a test causes. Magic packets fail the test unless a
// test installs its own sender.
func wakePair(t *testing.T, nics []wol.Target) (desktop, raspi *Node) {
	t.Helper()
	desktop = startNode(t, "desktop")
	raspi = startNode(t, "raspi")
	for _, n := range []*Node{desktop, raspi} {
		n.wakes.mu.Lock()
		n.wakes.localTargets = func() ([]wol.Target, error) { return nics, nil }
		n.wakes.send = func(context.Context, []wol.Target) (wol.Report, error) {
			t.Error("unexpected magic packet")
			return wol.Report{}, errors.New("sending is not allowed in this test")
		}
		n.wakes.mu.Unlock()
	}
	pair(t, desktop, raspi)
	// Each side fetches wake info only after its own first successful
	// refresh, so this also waits out the acceptor's post-pairing retries.
	waitFor(t, "both sides to learn the other's wake info", func() bool {
		a, _ := raspi.roster.Get(desktop.ID())
		b, _ := desktop.roster.Get(raspi.ID())
		return !a.WakeFetched.IsZero() && !b.WakeFetched.IsZero()
	})
	waitFor(t, "fetches and refreshes to settle", func() bool {
		for _, n := range []*Node{desktop, raspi} {
			n.wakes.mu.Lock()
			busy := len(n.wakes.fetching) + len(n.wakes.probing)
			n.wakes.mu.Unlock()
			n.peers.mu.Lock()
			busy += len(n.peers.busy)
			n.peers.mu.Unlock()
			if busy > 0 {
				return false
			}
		}
		return true
	})
	return desktop, raspi
}

func countProbes(n *Node) *atomic.Int32 {
	var c atomic.Int32
	n.wakes.mu.Lock()
	n.wakes.onProbe = func(string) { c.Add(1) }
	n.wakes.mu.Unlock()
	return &c
}

func sleepStateOf(n *Node, id string) (asleep, confirmed bool) {
	p, _ := n.roster.Get(id)
	return n.sleepState(p)
}

// announcement is what discovery reports when it hears n.
func announcement(n *Node, sleeping bool) discovery.Sighting {
	return discovery.Sighting{
		Announcement: discovery.Announcement{V: 1, ID: n.ID(), Name: n.Name(), Port: n.meshPort(), Sleeping: sleeping},
		Addr:         netip.MustParseAddrPort(n.MeshAddr()),
		Seen:         time.Now(),
	}
}

// putToSleep has the desktop tell the raspi it is going to sleep.
func putToSleep(t *testing.T, desktop, raspi *Node) {
	t.Helper()
	desktop.announceSleep()
	if _, confirmed := sleepStateOf(raspi, desktop.ID()); !confirmed {
		t.Fatal("raspi did not record the desktop's going-to-sleep notice")
	}
}

func toolText(res *mcp.CallToolResult) string {
	var b strings.Builder
	if res == nil {
		return ""
	}
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestWakeInfoLearnedAndPersistedAfterPairing(t *testing.T) {
	desktop, raspi := wakePair(t, desktopNICs)
	p, _ := raspi.roster.Get(desktop.ID())
	if len(p.Wake) != 2 || p.Wake[0].MAC != "a4:bb:6d:12:34:56" || p.Wake[1].Interface != "Wi-Fi" ||
		p.Wake[0].Subnets[0].Broadcast.String() != "192.168.1.255" {
		t.Fatalf("learned wake info = %+v", p.Wake)
	}
	ro, err := roster.Load(raspi.paths.RosterFile())
	if err != nil {
		t.Fatal(err)
	}
	saved, _ := ro.Get(desktop.ID())
	if len(saved.Wake) != 2 || saved.WakeFetched.IsZero() {
		t.Fatalf("wake info on disk = %+v (fetched %v)", saved.Wake, saved.WakeFetched)
	}
}

func TestGoingToSleepSuppressesProbesUntilHeardAgain(t *testing.T) {
	desktop, raspi := wakePair(t, desktopNICs)
	probes := countProbes(raspi)
	putToSleep(t, desktop, raspi)

	ro, _ := roster.Load(raspi.paths.RosterFile())
	if saved, _ := ro.Get(desktop.ID()); saved.Asleep.IsZero() {
		t.Fatal("asleep notice not persisted")
	}
	for _, d := range raspi.Nodes() {
		if d.ID == desktop.ID() && (!d.Asleep || d.Online) {
			t.Fatalf("mesh_nodes shows desktop asleep=%v online=%v, want asleep and not online", d.Asleep, d.Online)
		}
	}

	// Neither the probe loop nor a sleeping announcement may touch it.
	raspi.probePeer(desktop.ID())
	raspi.onSighting(announcement(desktop, true))
	if n := probes.Load(); n != 0 {
		t.Fatalf("%d probes sent to an asleep peer", n)
	}

	// An explicit call fails fast with a pointer to mesh_wake.
	start := time.Now()
	res, err := raspi.Call(t.Context(), desktop.ID(), "node_info", nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(toolText(res), "desktop is asleep; call mesh_wake first") {
		t.Fatalf("call to asleep peer = %q", toolText(res))
	}
	if time.Since(start) > time.Second {
		t.Fatalf("asleep call took %s; it must not dial", time.Since(start))
	}

	// An awake announcement clears it and probing resumes.
	raspi.onSighting(announcement(desktop, false))
	if asleep, _ := sleepStateOf(raspi, desktop.ID()); asleep {
		t.Fatal("still asleep after an awake announcement")
	}
	if p, _ := raspi.roster.Get(desktop.ID()); !p.Asleep.IsZero() {
		t.Fatal("stored asleep notice not cleared")
	}
	raspi.probePeer(desktop.ID())
	if n := probes.Load(); n != 1 {
		t.Fatalf("probes after waking = %d, want 1", n)
	}
	waitFor(t, "probe to finish", func() bool {
		raspi.wakes.mu.Lock()
		defer raspi.wakes.mu.Unlock()
		return len(raspi.wakes.probing) == 0
	})

	// Contact initiated by the peer clears it too.
	putToSleep(t, desktop, raspi)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := desktop.peers.refresh(ctx, raspi.ID()); err != nil {
		t.Fatal(err)
	}
	if asleep, _ := sleepStateOf(raspi, desktop.ID()); asleep {
		t.Fatal("still asleep after the desktop contacted the raspi")
	}
}

func TestSleepingAnnouncementIsOnlyAHint(t *testing.T) {
	desktop, raspi := wakePair(t, desktopNICs)
	probes := countProbes(raspi)
	raspi.onSighting(announcement(desktop, true))
	asleep, confirmed := sleepStateOf(raspi, desktop.ID())
	if !asleep || confirmed {
		t.Fatalf("after a multicast sleep hint: asleep=%v confirmed=%v, want true/false", asleep, confirmed)
	}
	raspi.probePeer(desktop.ID())
	if probes.Load() != 0 {
		t.Fatal("a sleep hint did not pause probes")
	}
	// A spoofable hint must not block explicit calls.
	res, err := raspi.Call(t.Context(), desktop.ID(), "node_info", nil, "test")
	if err != nil || res.IsError {
		t.Fatalf("call after a sleep hint: %v %q", err, toolText(res))
	}
	if p, _ := raspi.roster.Get(desktop.ID()); !p.Asleep.IsZero() {
		t.Fatal("an unauthenticated hint was stored")
	}
}

func TestProbeBackoff(t *testing.T) {
	for _, tc := range []struct {
		fails int
		want  time.Duration
	}{
		{0, 0}, {1, 0}, {2, 90 * time.Second}, {3, 3 * time.Minute}, {4, 6 * time.Minute}, {5, 10 * time.Minute}, {64, 10 * time.Minute},
	} {
		if got := probeBackoff(tc.fails); got != tc.want {
			t.Errorf("probeBackoff(%d) = %s, want %s", tc.fails, got, tc.want)
		}
	}

	desktop, raspi := wakePair(t, desktopNICs)
	probes := countProbes(raspi)
	failedAt := time.Now()
	raspi.wakes.mu.Lock()
	raspi.wakes.fails[desktop.ID()] = probeFail{count: 4, at: failedAt, next: failedAt.Add(time.Hour)}
	raspi.wakes.mu.Unlock()
	raspi.probePeer(desktop.ID())
	if probes.Load() != 0 {
		t.Fatal("probe sent during backoff")
	}
	raspi.peers.markContact(desktop.ID(), failedAt.Add(time.Millisecond))
	raspi.probePeer(desktop.ID())
	if probes.Load() != 1 {
		t.Fatal("contact after the failures did not end the backoff")
	}
}

func TestWakeOnlinePeerReturnsAtOnce(t *testing.T) {
	desktop, raspi := wakePair(t, desktopNICs)
	start := time.Now()
	if err := raspi.Wake(t.Context(), desktop.ID(), time.Minute); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 500*time.Millisecond {
		t.Fatalf("Wake on an online peer took %s", d)
	}
	if err := raspi.Wake(t.Context(), raspi.ID(), time.Minute); err != nil {
		t.Fatalf("Wake on self: %v", err)
	}
}

func TestWakeQuietPeerSendsPackets(t *testing.T) {
	desktop, raspi := wakePair(t, desktopNICs)
	var sends atomic.Int32
	raspi.wakes.mu.Lock()
	raspi.wakes.send = func(context.Context, []wol.Target) (wol.Report, error) {
		sends.Add(1)
		return wol.Report{Sent: 1}, nil
	}
	raspi.wakes.mu.Unlock()

	// Recently contacted and running: awake without packets.
	if err := raspi.Wake(t.Context(), desktop.ID(), time.Minute); err != nil {
		t.Fatal(err)
	}
	// Running but not contacted for a while (still within onlineWindow): the
	// live check succeeds, so still no packets.
	setContact(raspi, desktop.ID(), time.Now().Add(-30*time.Second))
	if err := raspi.Wake(t.Context(), desktop.ID(), time.Minute); err != nil {
		t.Fatal(err)
	}
	if n := sends.Load(); n != 0 {
		t.Fatalf("sent %d magic packet batches to a running peer", n)
	}

	// Stopped without announcing it; its last contact is well within onlineWindow.
	desktop.Close()
	setContact(raspi, desktop.ID(), time.Now().Add(-30*time.Second))
	if p, _ := raspi.roster.Get(desktop.ID()); raspi.peerState(p) != "online" {
		t.Fatalf("setup: desktop is %s, want online within onlineWindow", raspi.peerState(p))
	}
	err := raspi.Wake(t.Context(), desktop.ID(), 3*time.Second)
	if err == nil || !strings.Contains(err.Error(), "did not answer within") {
		t.Fatalf("Wake of a stopped peer = %v, want a timeout", err)
	}
	if sends.Load() == 0 {
		t.Fatal("Wake of a stopped peer sent no magic packets")
	}
}

// setContact overwrites n's last verified contact with id.
func setContact(n *Node, id string, at time.Time) {
	n.peers.mu.Lock()
	n.peers.contact[id] = at
	n.peers.mu.Unlock()
}

func TestWakeWithoutKnownMACsFails(t *testing.T) {
	desktop, raspi := wakePair(t, nil) // the desktop reports no wakeable NIC
	putToSleep(t, desktop, raspi)
	err := raspi.Wake(t.Context(), desktop.ID(), 5*time.Second)
	if err == nil || !strings.Contains(err.Error(), "no MAC address known for desktop") {
		t.Fatalf("Wake = %v, want an unknown-MAC error", err)
	}
	if err := raspi.Wake(t.Context(), strings.Repeat("a", 52), time.Second); err == nil {
		t.Fatal("Wake of an unpaired device succeeded")
	}
}

func TestMeshWakeSendsPacketsAndWaitsForContact(t *testing.T) {
	desktop, raspi := wakePair(t, desktopNICs)
	var mu sync.Mutex
	var sent [][]wol.Target
	raspi.wakes.mu.Lock()
	raspi.wakes.send = func(_ context.Context, ts []wol.Target) (wol.Report, error) {
		mu.Lock()
		sent = append(sent, ts)
		mu.Unlock()
		return wol.Report{MACs: []string{"a4:bb:6d:xx:xx:56", "a4:bb:6d:xx:xx:57"}, Sent: 12, Routes: []string{"192.168.1.5 (eth0) -> 192.168.1.255"}}, nil
	}
	raspi.wakes.mu.Unlock()
	putToSleep(t, desktop, raspi)

	s := agentSession(t, raspi)
	res, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: "mesh_wake", Arguments: map[string]any{"device": "desktop", "wait_seconds": 20}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("mesh_wake failed: %s", toolText(res))
	}
	var out control.WakeResult
	if err := json.Unmarshal([]byte(toolText(res)), &out); err != nil {
		t.Fatal(err)
	}
	if !out.Awake || out.Before != "asleep" || out.PacketsSent < 12 || len(out.MACs) != 2 || out.ID != desktop.ID() {
		t.Fatalf("mesh_wake result = %+v", out)
	}
	mu.Lock()
	first := sent[0]
	mu.Unlock()
	if len(first) != 2 || first[0].MAC != desktopNICs[0].MAC || first[1].MAC != desktopNICs[1].MAC {
		t.Fatalf("packets went to %+v, want both desktop NICs", first)
	}
	if asleep, _ := sleepStateOf(raspi, desktop.ID()); asleep {
		t.Fatal("a successful wake did not clear the asleep state")
	}
}

func TestAutoWakeWakesBeforeTheCall(t *testing.T) {
	desktop, raspi := wakePair(t, desktopNICs)
	if _, err := raspi.roster.SetAutoWake(desktop.ID(), true); err != nil {
		t.Fatal(err)
	}
	putToSleep(t, desktop, raspi)
	var calls atomic.Int32
	raspi.wakes.mu.Lock()
	raspi.wakes.waker = func(_ context.Context, id string, wait time.Duration) error {
		if id != desktop.ID() || wait != defaultWakeWait {
			t.Errorf("waker(%s, %s)", id, wait)
		}
		if calls.Add(1) == 1 {
			return errors.New("no answer")
		}
		return nil
	}
	raspi.wakes.mu.Unlock()

	res, err := raspi.Call(t.Context(), desktop.ID(), "node_info", nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(toolText(res), "auto-wake failed: no answer") {
		t.Fatalf("call with failing wake = %q, want the wake error (the call must wait for the waker)", toolText(res))
	}
	res, err = raspi.Call(t.Context(), desktop.ID(), "node_info", nil, "test")
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("call after a successful wake: %s", toolText(res))
	}
	if calls.Load() != 2 {
		t.Fatalf("waker ran %d times, want 2", calls.Load())
	}
	// Online again: no further wake.
	if _, err := raspi.Call(t.Context(), desktop.ID(), "node_info", nil, "test"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("waker ran for an online peer")
	}
}
