package node

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/provider"
	"messh/internal/state"
)

// The tests below are semantic, not wiring: every case pins a truthful
// host/guest transition or a parsing invariant, each with the recovery or
// evidence claim it must carry. Nothing here asserts exact human wording
// beyond those claims, and no test executes wsl.exe or dials the network.

func testWSLCfg() *state.WSLTargetConfig {
	return &state.WSLTargetConfig{
		Distro: "Ubuntu", User: "dom", Name: "dompc-wsl",
		ID: "wsl-id-1", GuestState: "/home/dom/.local/state/messh",
		HostAddress: "192.168.1.31", HostInterfaceIndex: 10,
		MeshPort: 7521, LocalPort: 7522,
		TaskName: "messh-wsl-start", RouteTaskName: "messh-wsl-route",
		AllowedPeers: []string{"192.168.1.189"},
	}
}

// UTF-16LE with BOM (the real wsl.exe encoding) decodes to plain names; a
// BOM-less UTF-8 dump passes through unchanged; NUL bytes without a BOM
// still decode as UTF-16LE; odd-length NUL input is corrupt, not silent.
func TestParseWSLQuietListEncodings(t *testing.T) {
	utf16Doc := append([]byte{0xFF, 0xFE}, utf16Lines("Ubuntu", "docker-desktop")...)
	got, err := parseWSLQuietList(utf16Doc)
	if err != nil || len(got) != 2 || got[0] != "Ubuntu" || got[1] != "docker-desktop" {
		t.Fatalf("utf16 BOM parse = %v, %v", got, err)
	}
	got, err = parseWSLQuietList([]byte("Ubuntu\nDebian\n\n"))
	if err != nil || len(got) != 2 || got[1] != "Debian" {
		t.Fatalf("utf8 parse = %v, %v", got, err)
	}
	got, err = parseWSLQuietList(utf16Lines("Ubuntu", "Debian"))
	if err != nil || len(got) != 2 || got[0] != "Ubuntu" {
		t.Fatalf("bom-less utf16 parse = %v, %v", got, err)
	}
	if _, err := parseWSLQuietList([]byte{'U', 0, 'x'}); err == nil {
		t.Fatal("odd-length NUL inventory parsed without error")
	}
}

// Running-but-quiet means service-or-network unknown, never service-down;
// the report must keep TCP and identity separate.
func TestRunningQuietStaysUnknown(t *testing.T) {
	rep := classifyDesktopTargets(testWSLCfg(), desktopFacts{
		Distros: []string{"Ubuntu"}, Running: []string{"Ubuntu"},
		Loopback: "refused", LoopTarget: "127.0.0.1:7521",
		PortProxy: "present", IfaceName: "WiFi 2",
		Current: []string{"192.168.1.31"},
	}, "win-id", "desktop", "windows")
	if rep.Guest.State != "running" {
		t.Fatalf("guest state = %q, want running", rep.Guest.State)
	}
	if rep.Guest.Service != svcUnknown {
		t.Fatalf("service = %q, want %q (never a fabricated service-down)", rep.Guest.Service, svcUnknown)
	}
	if !strings.Contains(rep.Guest.Identity, "not authenticated") {
		t.Fatalf("identity lost its TCP caveat: %q", rep.Guest.Identity)
	}
}

// TCP reachable on a running guest is still identity-unverified: the report
// must say so and name the paired-call confirmation.
func TestRunningReachableKeepsIdentityUnverified(t *testing.T) {
	rep := classifyDesktopTargets(testWSLCfg(), desktopFacts{
		Distros: []string{"Ubuntu"}, Running: []string{"Ubuntu"},
		Loopback: "reachable", LoopTarget: "127.0.0.1:7521",
		PortProxy: "present",
	}, "win-id", "desktop", "windows")
	if rep.Guest.Service != svcTCPReachable {
		t.Fatalf("service = %q, want %q", rep.Guest.Service, svcTCPReachable)
	}
	if !strings.Contains(rep.Guest.Identity, "not authenticated") {
		t.Fatalf("reachable report claims identity: %q", rep.Guest.Identity)
	}
	if !strings.Contains(strings.Join(rep.Recovery, "\n"), "TCP alone never authenticates") {
		t.Fatalf("recovery lacks the identity-confirmation command: %v", rep.Recovery)
	}
}

// Stopped needs both inventories to succeed: listing present, running
// absent. The unexpected-listener variant must warn, not bless the port.
func TestStoppedConfirmedOnlyByBothInventories(t *testing.T) {
	quiet := desktopFacts{
		Distros: []string{"Ubuntu"}, Running: []string{"docker-desktop"},
		Loopback: "refused", LoopTarget: "127.0.0.1:7521",
		PortProxy: "present",
	}
	rep := classifyDesktopTargets(testWSLCfg(), quiet, "win-id", "desktop", "windows")
	if rep.Guest.State != "stopped" || rep.Guest.Service != svcConsistentDown {
		t.Fatalf("stopped = %q/%q, want stopped/%q", rep.Guest.State, rep.Guest.Service, svcConsistentDown)
	}
	loud := quiet
	loud.Loopback = "reachable"
	rep = classifyDesktopTargets(testWSLCfg(), loud, "win-id", "desktop", "windows")
	if rep.Guest.Service != svcUnexpected {
		t.Fatalf("stopped-but-listening = %q, want %q", rep.Guest.Service, svcUnexpected)
	}
	if !strings.Contains(strings.Join(rep.Recovery, "\n"), "do not trust it") {
		t.Fatalf("unexpected listener lacks the distrust warning: %v", rep.Recovery)
	}
}

// A failed inventory is unknown, never stopped — even when the endpoint is
// also quiet. An unreachable host never even reaches classify (transport
// failure), so no test fabricates that path.
func TestFailedInventoryNeverReportsStopped(t *testing.T) {
	rep := classifyDesktopTargets(testWSLCfg(), desktopFacts{
		InvErr:   "wsl --list --quiet failed: exit status 1",
		Loopback: "refused", LoopTarget: "127.0.0.1:7521",
	}, "win-id", "desktop", "windows")
	if rep.Guest.State != "unknown" {
		t.Fatalf("failed-inventory guest = %q, want unknown", rep.Guest.State)
	}
	if rep.Host.Inventory != "error" {
		t.Fatalf("host inventory = %q, want error", rep.Host.Inventory)
	}
	if !strings.Contains(strings.Join(rep.Recovery, "\n"), "wsl --list --quiet") {
		t.Fatalf("recovery lacks the inventory re-run: %v", rep.Recovery)
	}
}

// Absent-from-successful-inventory is unknown (renamed?), not stopped, and
// the recovery must offer re-capture rather than a boot command.
func TestMissingDistroIsUnknownWithRecapture(t *testing.T) {
	rep := classifyDesktopTargets(testWSLCfg(), desktopFacts{
		Distros: []string{"Debian"}, Running: []string{"Debian"},
	}, "win-id", "desktop", "windows")
	if rep.Guest.State != "unknown" {
		t.Fatalf("missing-distro guest = %q, want unknown", rep.Guest.State)
	}
	if !strings.Contains(strings.Join(rep.Recovery, "\n"), "messh wsl setup") {
		t.Fatalf("recovery lacks re-capture: %v", rep.Recovery)
	}
}

// Stale capture (DHCP moved the host) is flagged with the refresh command;
// a matching capture is explicitly fresh. Off-host, staleness is unknown.
func TestCapturedAddressStaleness(t *testing.T) {
	fresh := desktopFacts{
		Distros: []string{"Ubuntu"}, Running: []string{"Ubuntu"},
		Current: []string{"192.168.1.31"}, IfaceName: "WiFi 2", PortProxy: "present",
	}
	rep := classifyDesktopTargets(testWSLCfg(), fresh, "win-id", "desktop", "windows")
	if rep.Route.Stale == nil || *rep.Route.Stale {
		t.Fatalf("matching capture reported stale: %+v", rep.Route)
	}
	moved := fresh
	moved.Current = []string{"192.168.1.44"}
	rep = classifyDesktopTargets(testWSLCfg(), moved, "win-id", "desktop", "windows")
	if rep.Route.Stale == nil || !*rep.Route.Stale {
		t.Fatalf("moved host not reported stale: %+v", rep.Route)
	}
	if !strings.Contains(rep.Route.Detail, "messh wsl refresh") {
		t.Fatalf("stale detail lacks refresh: %q", rep.Route.Detail)
	}
	rep = classifyDesktopTargets(testWSLCfg(), desktopFacts{Unavailable: true}, "win-id", "desktop", "linux")
	if rep.Route.Stale != nil {
		t.Fatalf("off-host staleness answered: %+v", rep.Route)
	}
}

// Off-host answers must be honest about what they cannot know: inventory
// unavailable, guest unknown, proxy target still the literal loopback.
func TestOffHostReportIsHonestlyUnknown(t *testing.T) {
	rep := classifyDesktopTargets(testWSLCfg(), desktopFacts{Unavailable: true}, "win-id", "desktop", "linux")
	if rep.Host.Inventory != "unavailable" || rep.Guest.State != "unknown" {
		t.Fatalf("off-host = %q/%q, want unavailable/unknown", rep.Host.Inventory, rep.Guest.State)
	}
	if !strings.Contains(rep.Guest.Evidence, "messh wsl status <WINDOWS_DEVICE>") {
		t.Fatalf("off-host evidence lacks the remote command: %q", rep.Guest.Evidence)
	}
}

// Unconfigured (nil load) is its own state with the setup command — never
// confused with stopped or unknown.
func TestUnconfiguredTarget(t *testing.T) {
	rep := classifyDesktopTargets(nil, desktopFacts{}, "win-id", "desktop", "windows")
	if rep.Guest.State != "unconfigured" {
		t.Fatalf("nil config guest = %q, want unconfigured", rep.Guest.State)
	}
	if !strings.Contains(strings.Join(rep.Recovery, "\n"), "messh wsl setup") {
		t.Fatalf("unconfigured recovery lacks setup: %v", rep.Recovery)
	}
	if len(rep.Targets) != 1 || rep.Targets[0].Role != "windows" {
		t.Fatalf("unconfigured targets = %+v, want windows only", rep.Targets)
	}
}

// Captured IDs travel in the report; the LAN proxy target is the captured
// host address, while the guest address stays a note, never a pinned route.
func TestReportCarriesIDsAndLoopbackTarget(t *testing.T) {
	cfg := testWSLCfg()
	cfg.GuestAddress = "172.25.172.58"
	rep := classifyDesktopTargets(cfg, desktopFacts{
		Distros: []string{"Ubuntu"}, Running: []string{"Ubuntu"},
		Loopback: "reachable", LoopTarget: "192.168.1.31:7521",
		CurrentGuest: "172.25.172.59", PortProxy: "present",
	}, "win-id", "desktop", "windows")
	if len(rep.Targets) != 2 || rep.Targets[0].ID != "win-id" || rep.Targets[1].ID != "wsl-id-1" {
		t.Fatalf("targets = %+v, want windows win-id + wsl wsl-id-1", rep.Targets)
	}
	if rep.Route.ProxyTarget != "192.168.1.31:7521" {
		t.Fatalf("proxy target = %q, want captured LAN address", rep.Route.ProxyTarget)
	}
	if rep.Route.CurrentGuest != "172.25.172.59" {
		t.Fatalf("current guest address = %q, want observed address rather than captured %q", rep.Route.CurrentGuest, cfg.GuestAddress)
	}
	if rep.Route.GuestAddressErr != "" {
		t.Fatalf("unexpected guest address error: %q", rep.Route.GuestAddressErr)
	}
}

func TestRouteReportsGuestResolutionFailure(t *testing.T) {
	rep := classifyDesktopTargets(testWSLCfg(), desktopFacts{
		PortProxy: "unknown", GuestAddressErr: "ambiguous WSL NAT neighbors",
	}, "win-id", "desktop", "windows")
	if rep.Route.GuestAddressErr != "ambiguous WSL NAT neighbors" {
		t.Fatalf("guest address error = %q, want resolver evidence", rep.Route.GuestAddressErr)
	}
	if !strings.Contains(rep.Route.Detail, "ambiguous WSL NAT neighbors") {
		t.Fatalf("route detail omits resolver evidence: %q", rep.Route.Detail)
	}
}

// The tool path itself: unconfigured load reports unconfigured without
// probing; a corrupt config reports the corruption as the finding; extra
// arguments are rejected rather than probed.
func TestDesktopTargetsCallSemantics(t *testing.T) {
	unconfigured := &desktopTargetsProvider{
		selfID: "win-id", selfName: "desktop",
		load: func() (*state.WSLTargetConfig, error) { return nil, nil },
		probe: func(context.Context, *state.WSLTargetConfig) desktopFacts {
			t.Error("probe ran for an unconfigured target")
			return desktopFacts{}
		},
	}
	res, err := unconfigured.Call(context.Background(), DesktopTargetsToolName, nil, provider.Caller{})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("unconfigured call errored: %s", textOf(t, res))
	}
	var rep desktopTargetsReport
	if err := json.Unmarshal([]byte(textOf(t, res)), &rep); err != nil {
		t.Fatal(err)
	}
	if rep.Guest.State != "unconfigured" {
		t.Fatalf("call guest = %q, want unconfigured", rep.Guest.State)
	}

	corrupt := &desktopTargetsProvider{
		selfID: "win-id", selfName: "desktop",
		load: func() (*state.WSLTargetConfig, error) { return nil, errCorruptTarget },
	}
	res, err = corrupt.Call(context.Background(), DesktopTargetsToolName, nil, provider.Caller{})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("corrupt-config call errored: %s", textOf(t, res))
	}
	var bad desktopTargetsReport
	if err := json.Unmarshal([]byte(textOf(t, res)), &bad); err != nil {
		t.Fatal(err)
	}
	if bad.Guest.State != "unknown" || !strings.Contains(bad.Guest.Evidence, "cannot load") {
		t.Fatalf("corrupt config = %+v, want unknown with load evidence", bad.Guest)
	}

	res, _ = unconfigured.Call(context.Background(), DesktopTargetsToolName, json.RawMessage(`{"device":"other"}`), provider.Caller{})
	if !res.IsError {
		t.Fatal("address-bearing arguments accepted: callers must not steer probes")
	}
	if _, err := unconfigured.Call(context.Background(), "nope", nil, provider.Caller{}); err == nil {
		t.Fatal("unknown tool accepted")
	}
}

// errCorruptTarget stands in for a corrupt wsl-target.json load failure.
var errCorruptTarget = errors.New("wsl-target.json: invalid JSON")

// utf16Lines encodes names the way wsl.exe writes them: UTF-16LE lines.
func utf16Lines(names ...string) []byte {
	var u16 []uint16
	for i, n := range names {
		if i > 0 {
			u16 = append(u16, utf16.Encode([]rune("\n"))...)
		}
		u16 = append(u16, utf16.Encode([]rune(n))...)
	}
	u16 = append(u16, utf16.Encode([]rune("\n"))...)
	var out []byte
	for _, u := range u16 {
		out = append(out, byte(u), byte(u>>8))
	}
	return out
}

func textOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
