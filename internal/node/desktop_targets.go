// Read-only Windows+WSL desktop target diagnostics: observed host/guest
// state for the configured WSL target, without starting, stopping, or
// otherwise changing anything.
//
// The tool runs on the node being inspected. Locally it reports this
// device's own WSL target; remotely (mesh_call / <host>__desktop_targets,
// or `messh wsl status WINDOWS_DEVICE`) the Windows host answers for
// itself, so a reachable host with a stopped guest is distinguishable from
// an unreachable host. An unreachable host never reports a guest state: the
// caller sees the transport failure, not a fabricated stopped verdict.
//
// Read-only means read-only. Inventories use only `wsl --list --quiet` and
// `wsl --list --running --quiet`, which never start a distribution. wsl
// --exec (even after seeing Running) is forbidden here: it races with guest
// shutdown and can boot a stopped guest. TCP dials only observe; a completed
// connection is not authenticated messh identity. The dial/exec targets come
// only from the persisted config and fixed argv: callers supply no
// addresses, so a remote caller cannot steer this tool into probing
// arbitrary hosts.
package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/provider"
	"messh/internal/state"
)

// DesktopTargetsToolName is the read-only diagnostic tool this file owns.
// Parent wires startDesktopTargets during Node.Start; the gateway then
// exposes it through the existing full (<handle>__desktop_targets) and
// compact (mesh_call) routing with no further work.
const DesktopTargetsToolName = "desktop_targets"

const (
	// desktopInventoryTimeout bounds the whole host inventory (wsl --list
	// twice plus one netsh read). Individual dials are bounded separately.
	desktopInventoryTimeout = 15 * time.Second
	// desktopDialTimeout bounds one TCP observation dial.
	desktopDialTimeout = 2 * time.Second
)

// Service verdicts for the guest mesh endpoint. TCP reachability and
// authenticated messh identity are reported separately on purpose: only the
// verdict plus identity "verified" would mean the intended service answered.
const (
	// svcTCPReachable: confirmed running guest, endpoint TCP reachable, but
	// identity still unverified (TCP is not authentication).
	svcTCPReachable = "tcp_reachable_identity_unverified"
	// svcUnknown: confirmed running guest, endpoint not answering TCP (or
	// not probed): a service-or-network problem, never a fabricated
	// service-down claim.
	svcUnknown = "service_or_network_unknown"
	// svcConsistentDown: confirmed stopped guest, endpoint quiet too.
	svcConsistentDown = "consistent_with_stopped_guest"
	// svcUnexpected: confirmed stopped guest, yet something answers TCP:
	// another process holds the port; verify identity, do not trust it.
	svcUnexpected = "unexpected_listener_for_stopped_guest"
	// svcUnknownGuest: guest state itself is unknown, so is the service.
	svcUnknownGuest = "unknown_guest_unknown_service"
	// svcNA: unconfigured target or non-Windows reporter.
	svcNA = "not_applicable"
)

// desktopIdentityNote is attached to every guest verdict that rests on TCP.
const desktopIdentityNote = "unverified: a TCP connection is not authenticated messh identity or proof of the intended service"

// desktopTargetsProvider answers desktop_targets from the persisted WSL
// target config plus a platform inventory. load/probe are fields (not
// methods) so semantic tests can substitute canned facts; production uses
// newDesktopTargetsProvider.
type desktopTargetsProvider struct {
	paths    state.Paths
	selfID   string
	selfName string
	load     func() (*state.WSLTargetConfig, error)
	probe    func(ctx context.Context, cfg *state.WSLTargetConfig) desktopFacts
}

var _ provider.Provider = (*desktopTargetsProvider)(nil)

func newDesktopTargetsProvider(paths state.Paths, selfID, selfName string) *desktopTargetsProvider {
	p := &desktopTargetsProvider{paths: paths, selfID: selfID, selfName: selfName}
	p.load = paths.LoadWSLTarget
	p.probe = probeDesktopTargets
	return p
}

// startDesktopTargets registers the read-only desktop target diagnostics.
// Registration only, no I/O: a broken or absent WSL setup can never fail
// node startup. Parent calls this from Start before the tool set is rebuilt.
func (n *Node) startDesktopTargets() {
	n.register(newDesktopTargetsProvider(n.paths, n.ID(), n.Name()))
}

func (p *desktopTargetsProvider) Name() string { return DesktopTargetsToolName }

func (p *desktopTargetsProvider) Tools() []provider.Tool {
	return []provider.Tool{{Class: provider.ClassInfo, Def: &mcp.Tool{
		Name: DesktopTargetsToolName,
		Description: "Inspect this device's Windows+WSL desktop targets from its captured configuration and " +
			"read-only host observations (wsl distro inventories, route facts, bounded TCP checks). Never starts, " +
			"stops, or reconfigures anything; a TCP connection is reported as TCP only, never as messh identity. " +
			"On a non-Windows device the host inventory is honestly unavailable.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, Title: "Desktop targets status"},
	}}}
}

func (p *desktopTargetsProvider) Call(ctx context.Context, tool string, args json.RawMessage, _ provider.Caller) (*mcp.CallToolResult, error) {
	if tool != DesktopTargetsToolName {
		return nil, fmt.Errorf("unknown tool %q", tool)
	}
	// The tool takes no arguments: reject anything else strictly so a
	// caller cannot silently smuggle in an address to probe.
	if t := bytes.TrimSpace(args); len(t) > 0 && !bytes.Equal(t, []byte("null")) {
		var noArgs struct{}
		dec := json.NewDecoder(bytes.NewReader(t))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&noArgs); err != nil {
			return provider.ErrorResult("%s: invalid arguments: %v", DesktopTargetsToolName, err), nil
		}
		var extra any
		err := dec.Decode(&extra)
		if err == nil {
			return provider.ErrorResult("%s: invalid arguments: unexpected trailing JSON", DesktopTargetsToolName), nil
		}
		if err != io.EOF {
			return provider.ErrorResult("%s: invalid arguments: %v", DesktopTargetsToolName, err), nil
		}
	}
	cfg, err := p.load()
	if err != nil {
		// A corrupt wsl-target.json is reported, not fatal: diagnostics
		// describe evidence, and the corruption itself is the finding.
		return provider.JSONResult(classifyDesktopTargets(nil, desktopFacts{
			InvErr: "cannot load WSL target config: " + err.Error(),
		}, p.selfID, p.selfName, runtime.GOOS))
	}
	if cfg == nil {
		return provider.JSONResult(classifyDesktopTargets(nil, desktopFacts{}, p.selfID, p.selfName, runtime.GOOS))
	}
	ctx, cancel := context.WithTimeout(ctx, desktopInventoryTimeout)
	defer cancel()
	return provider.JSONResult(classifyDesktopTargets(cfg, p.probe(ctx, cfg), p.selfID, p.selfName, runtime.GOOS))
}

// desktopFacts is everything the platform layer observed. The zero value
// means nothing was observed: classify reports unknown, never stopped.
type desktopFacts struct {
	Unavailable     bool     // not a Windows host: wsl.exe inventory has no meaning here
	InvErr          string   // wsl --list failed: a failed inventory proves nothing
	Distros         []string // wsl --list --quiet (successful inventory only)
	Running         []string // wsl --list --running --quiet (successful inventory only)
	IfaceName       string   // selected host interface name, when readable
	IfaceErr        string   // selected host interface unreadable
	Current         []string // current addresses of the selected interface
	CurrentGuest    string   // current private guest address resolved from WSL NAT adapter
	GuestAddressErr string   // current guest observation failed or was ambiguous
	PortProxy       string   // present | absent | unknown
	Loopback        string   // loopback dial: reachable | refused | timeout | error | not_probed
	LoopTarget      string   // loopback address dialed ("" when not probed)
	LAN             string   // LAN dial: same vocabulary (host-local observation)
	LANTarget       string   // LAN address dialed ("" when not probed)
}

// desktopTargetsReport is the desktop_targets result: observed facts with
// stale-capable route metadata, the captured expected target IDs, the
// WSL-primary policy, and explicit recovery commands.
type desktopTargetsReport struct {
	Host     desktopHost     `json:"host"`
	Guest    desktopGuest    `json:"guest"`
	Route    desktopRoute    `json:"route"`
	Targets  []desktopTarget `json:"targets"`
	Policy   desktopPolicy   `json:"policy"`
	Recovery []string        `json:"recovery"`
}

type desktopHost struct {
	OS        string   `json:"os"`
	Inventory string   `json:"inventory"` // ok | unavailable | error
	Detail    string   `json:"detail"`
	Distros   []string `json:"distros"`
	Running   []string `json:"running"`
}

type desktopGuest struct {
	Distro       string `json:"distro"`
	State        string `json:"state"` // running | stopped | unknown | unconfigured
	Evidence     string `json:"evidence"`
	MeshEndpoint string `json:"mesh_endpoint,omitempty"`
	TCP          string `json:"tcp,omitempty"`
	Service      string `json:"service"`
	Identity     string `json:"identity"`
}

type desktopRoute struct {
	CapturedHost     string   `json:"captured_host_address,omitempty"`
	InterfaceIndex   int      `json:"host_interface_index,omitempty"`
	InterfaceName    string   `json:"interface_name,omitempty"`
	CurrentHost      []string `json:"current_host_addresses,omitempty"`
	Stale            *bool    `json:"captured_address_stale,omitempty"`
	ProxyTarget      string   `json:"proxy_target,omitempty"`
	PortProxy        string   `json:"portproxy,omitempty"`
	LANTarget        string   `json:"lan_target,omitempty"`
	LANTCP           string   `json:"lan_tcp,omitempty"`
	GuestAddressNote string   `json:"guest_address_note,omitempty"`
	CurrentGuest     string   `json:"current_guest_address,omitempty"`
	GuestAddressErr  string   `json:"guest_address_error,omitempty"`
	Detail           string   `json:"detail,omitempty"`
}

type desktopTarget struct {
	Role string `json:"role"` // windows | wsl
	Name string `json:"name"`
	ID   string `json:"id"`
}

type desktopPolicy struct {
	Primary   string `json:"primary"`
	Selection string `json:"selection"`
	Budgets   string `json:"budgets"`
	SharedGPU string `json:"shared_gpu"`
}

// classifyDesktopTargets folds config plus platform facts into the report.
// It is pure (no I/O, no clock) so every host/guest transition is unit
// testable. Central rule: a stopped verdict needs successful inventories
// showing the distro present-but-not-running; anything less is unknown, and
// an unreachable host never reaches here at all (the caller sees the
// transport failure instead).
func classifyDesktopTargets(cfg *state.WSLTargetConfig, f desktopFacts, selfID, selfName, goos string) desktopTargetsReport {
	rep := desktopTargetsReport{
		Host: desktopHost{
			OS: goos, Inventory: "ok",
			Distros: desktopNonNilStrings(f.Distros), Running: desktopNonNilStrings(f.Running),
		},
		Targets:  []desktopTarget{{Role: "windows", Name: selfName, ID: selfID}},
		Policy:   desktopPolicyBlock(),
		Recovery: []string{},
	}
	if cfg == nil {
		if f.InvErr != "" {
			rep.Host.Inventory = "error"
			rep.Host.Detail = f.InvErr
			rep.Guest = desktopGuest{State: "unknown", Evidence: f.InvErr, Service: svcUnknownGuest, Identity: desktopIdentityNote}
			rep.Recovery = append(rep.Recovery,
				"inspect the WSL target file on the Windows host and re-run its setup to re-capture it",
				"re-check with: messh wsl status",
			)
			return rep
		}
		rep.Guest = desktopGuest{State: "unconfigured", Evidence: "no WSL target configured on this device (wsl-target.json absent)", Service: svcNA, Identity: "not_applicable"}
		rep.Recovery = append(rep.Recovery, "on the Windows host, configure the WSL target with: messh wsl setup (then re-check with: messh wsl status)")
		return rep
	}

	rep.Targets = append(rep.Targets, desktopTarget{Role: "wsl", Name: cfg.Name, ID: cfg.ID})

	// Host inventory section.
	switch {
	case f.Unavailable:
		rep.Host.Inventory = "unavailable"
		rep.Host.Detail = "host inventory unavailable: the answering node runs " + goos + ", not Windows, so wsl.exe has no meaning here"
	case f.InvErr != "":
		rep.Host.Inventory = "error"
		rep.Host.Detail = f.InvErr
	default:
		rep.Host.Detail = fmt.Sprintf("wsl --list inventories succeeded: %d distro(s), %d running", len(f.Distros), len(f.Running))
	}

	// Guest state machine. Stopped is confirmed only by successful host
	// inventories listing the distro present but not running.
	rep.Guest.Distro = cfg.Distro
	meshPort := desktopValidPort(cfg.MeshPort)
	loopTarget := ""
	if meshPort {
		loopTarget = net.JoinHostPort(cfg.HostAddress, strconv.Itoa(cfg.MeshPort))
	}
	rep.Guest.MeshEndpoint = loopTarget
	rep.Guest.TCP = f.Loopback
	rep.Guest.Identity = desktopIdentityNote
	tcpNote := desktopTCPEvidence(f.Loopback, f.LoopTarget)
	switch {
	case f.Unavailable:
		rep.Guest.State = "unknown"
		rep.Guest.Service = svcNA
		rep.Guest.Identity = "not_applicable"
		rep.Guest.TCP = ""
		rep.Guest.MeshEndpoint = loopTarget
		rep.Guest.Evidence = "host inventory unavailable on this OS (" + goos + "): run on the Windows host itself, or remotely with: messh wsl status <WINDOWS_DEVICE>"
		rep.Recovery = append(rep.Recovery, "run on the Windows host itself, or remotely with: messh wsl status <WINDOWS_DEVICE>")
	case f.InvErr != "":
		rep.Guest.State = "unknown"
		rep.Guest.Service = svcUnknownGuest
		rep.Guest.Evidence = "wsl inventory failed (" + f.InvErr + "); a failed inventory cannot confirm a stopped guest." + tcpNote
		rep.Recovery = append(rep.Recovery,
			"on the Windows host, run: wsl --list --quiet (it must succeed before any stopped verdict)",
			"re-check with: messh wsl status",
		)
	case desktopContainsName(f.Running, cfg.Distro):
		rep.Guest.State = "running"
		rep.Guest.Evidence = fmt.Sprintf("%s present in wsl --list --running --quiet on %s.", cfg.Distro, selfName) + tcpNote
		switch f.Loopback {
		case "reachable":
			rep.Guest.Service = svcTCPReachable
			rep.Recovery = append(rep.Recovery,
				"no repair indicated: confirm messh identity with a paired call (e.g. a job submit/status on "+cfg.Name+") before trusting the endpoint — TCP alone never authenticates it",
			)
		default:
			rep.Guest.Service = svcUnknown
			rep.Recovery = append(rep.Recovery,
				"on the host, read-only state check: wsl -l -v",
				fmt.Sprintf("inside %s, check the guest service: systemctl --user status messh.service (guest state: %s)", cfg.Distro, cfg.GuestState),
				"re-check with: messh wsl status",
				"if the Windows LAN address changed (DHCP), refresh the host route with: messh wsl refresh",
			)
		}
	case desktopContainsName(f.Distros, cfg.Distro):
		rep.Guest.State = "stopped"
		rep.Guest.Evidence = fmt.Sprintf("%s present in wsl --list --quiet but absent from wsl --list --running --quiet on %s: confirmed stopped by successful host inventories.", cfg.Distro, selfName) + tcpNote
		if f.Loopback == "reachable" {
			rep.Guest.Service = svcUnexpected
			rep.Recovery = append(rep.Recovery,
				fmt.Sprintf("the guest is stopped but %s answers TCP: another process holds the port — do not trust it; find it on the host before starting %s", f.LoopTarget, cfg.Distro),
			)
		} else {
			rep.Guest.Service = svcConsistentDown
		}
		rep.Recovery = append(rep.Recovery,
			fmt.Sprintf("log on to Windows as %s: the logon task %s boots %s", cfg.User, cfg.TaskName, cfg.Distro),
			fmt.Sprintf("or run on the host: wsl -d %s", cfg.Distro),
			"then start the guest service inside the distro: systemctl --user start messh.service",
			"re-check with: messh wsl status",
		)
	default:
		rep.Guest.State = "unknown"
		rep.Guest.Service = svcUnknownGuest
		rep.Guest.Evidence = fmt.Sprintf("expected distro %s absent from a successful host inventory (distros: [%s]); not confirmed stopped — it may be renamed or uninstalled.", cfg.Distro, strings.Join(f.Distros, ", ")) + tcpNote
		rep.Recovery = append(rep.Recovery,
			"on the Windows host, compare with: wsl --list --quiet",
			"re-capture the target with: messh wsl setup (corrects a renamed distro), then re-check with: messh wsl status",
		)
	}

	// Route section: stale-capable metadata. The live proxy destination is
	// resolved from the default WSL2 NAT adapter, never global ARP; the captured
	// guest address stays informational only and is never pinned.
	route := desktopRoute{
		CapturedHost:    cfg.HostAddress,
		InterfaceIndex:  cfg.HostInterfaceIndex,
		InterfaceName:   f.IfaceName,
		CurrentHost:     desktopNonNilStrings(f.Current),
		PortProxy:       f.PortProxy,
		CurrentGuest:    f.CurrentGuest,
		GuestAddressErr: f.GuestAddressErr,
		LANTarget:       f.LANTarget,
		LANTCP:          f.LAN,
	}
	if meshPort {
		route.ProxyTarget = net.JoinHostPort(cfg.HostAddress, strconv.Itoa(cfg.MeshPort))
	}
	if cfg.GuestAddress != "" {
		route.GuestAddressNote = "captured guest address " + cfg.GuestAddress + " is informational only; the current guest IP is resolved from the default WSL2 NAT adapter, never global ARP or pinned from this capture"
	}
	stale, staleDetail := desktopCapturedStale(cfg.HostAddress, f.Current, f.IfaceErr, f.Unavailable)
	route.Stale = stale
	var details []string
	if f.IfaceName != "" {
		details = append(details, "selected interface "+f.IfaceName+" (#"+strconv.Itoa(cfg.HostInterfaceIndex)+")")
	} else if f.IfaceErr != "" && !f.Unavailable {
		details = append(details, "selected interface #"+strconv.Itoa(cfg.HostInterfaceIndex)+" unreadable: "+f.IfaceErr)
	}
	details = append(details, staleDetail)
	switch f.PortProxy {
	case "present":
		details = append(details, "portproxy LAN "+route.ProxyTarget+" to current WSL guest "+f.CurrentGuest+" present on the host (destination matched against the default WSL2 NAT adapter)")
	case "absent":
		details = append(details, "portproxy LAN "+route.ProxyTarget+" to the live guest IP absent on the host: refresh the host route with: messh wsl refresh")
	case "unknown":
		if !f.Unavailable {
			details = append(details, "portproxy state unknown")
		}
	}
	if f.GuestAddressErr != "" && !f.Unavailable {
		details = append(details, "current WSL guest address unavailable: "+f.GuestAddressErr)
	}
	if cfg.LocalPort != 0 {
		details = append(details, "guest local API port "+strconv.Itoa(cfg.LocalPort)+" is never proxied or firewalled: it stays unexposed")
	}
	route.Detail = strings.Join(details, "; ")
	rep.Route = route

	if len(rep.Recovery) == 0 {
		rep.Recovery = append(rep.Recovery, "re-check with: messh wsl status")
	}
	return rep
}

// capturedStale compares the last captured Windows LAN address against the
// selected interface's current addresses. DHCP moves the host, so a mismatch
// means stale (refresh), not broken: the report says exactly that. A nil
// result means unanswerable (no capture, or the interface cannot be read).
func desktopCapturedStale(captured string, current []string, ifaceErr string, unavailable bool) (*bool, string) {
	if captured == "" {
		return nil, "no captured host address to compare"
	}
	if unavailable {
		return nil, "staleness unknown off-host: captured " + captured
	}
	if ifaceErr != "" {
		return nil, "staleness unknown: selected interface unreadable (" + ifaceErr + "); captured " + captured
	}
	want, err := netip.ParseAddr(captured)
	if err != nil {
		return nil, "captured host address " + captured + " is unparsable"
	}
	for _, c := range current {
		if got, err := netip.ParseAddr(c); err == nil && got == want {
			return new(false), "captured " + captured + " still on the selected interface"
		}
	}
	return new(true), "captured " + captured + " not among current [" + strings.Join(current, ", ") + "] (DHCP may have moved the host): refresh with: messh wsl refresh"
}

// tcpEvidence renders one bounded TCP observation exactly: what was dialed
// and what happened, never an identity claim.
func desktopTCPEvidence(label, target string) string {
	switch label {
	case "":
		return ""
	case "not_probed":
		return " Endpoint TCP not probed" + desktopTargetSuffix(target) + "."
	case "reachable":
		return " Endpoint TCP reachable" + desktopTargetSuffix(target) + " (host-local observation; identity unverified)."
	default:
		return " Endpoint TCP " + label + desktopTargetSuffix(target) + " (host-local observation)."
	}
}

func desktopTargetSuffix(target string) string {
	if target == "" {
		return ""
	}
	return " at " + target
}

// desktopPolicyBlock states the WSL-primary desktop compute policy:
// explicit target selection, separate per-node budgets, and the shared
// physical GPU warning. Budgets never aggregate across Windows+WSL.
func desktopPolicyBlock() desktopPolicy {
	return desktopPolicy{
		Primary:   "WSL-primary desktop compute: run desktop Linux work on the WSL target; use the native Windows target for Windows-only commands, services, and browser work.",
		Selection: "Target selection is explicit (device handle, name, or exact ID, e.g. messh job submit <DEVICE>); the mesh never routes by bare task name.",
		Budgets:   "CPU, RAM, and VRAM budgets are per node: a job on one target claims only that target's resources, and budgets do not aggregate across Windows+WSL.",
		SharedGPU: "Windows and WSL share one physical GPU: heavy GPU work on both targets at once contends for the same device; prefer bounded CPU proof when the GPU is busy.",
	}
}

// parseWSLQuietList decodes `wsl --list --quiet` output (with or without
// --running): one distro name per line. wsl.exe writes UTF-16LE with a BOM;
// without a BOM, any NUL byte still means UTF-16LE, otherwise the bytes are
// plain UTF-8 lines. Empty lines are dropped. The --quiet form prints no
// header, so unlike `wsl -l -v` parsing there is nothing to skip.
func parseWSLQuietList(data []byte) ([]string, error) {
	text := string(data)
	if len(data) >= 2 && data[0] == 0xFF && data[1] == 0xFE {
		text = decodeUTF16LE(data[2:])
	} else if bytes.IndexByte(data, 0) >= 0 {
		if len(data)%2 != 0 {
			return nil, fmt.Errorf("wsl inventory has %d bytes: odd-length UTF-16", len(data))
		}
		text = decodeUTF16LE(data)
	}
	out := []string{}
	for _, line := range strings.Split(text, "\n") {
		if name := strings.TrimSpace(line); name != "" {
			out = append(out, name)
		}
	}
	return out, nil
}

// decodeUTF16LE decodes little-endian UTF-16 (the wsl.exe console encoding)
// without external dependencies.
func decodeUTF16LE(b []byte) string {
	u16 := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u16 = append(u16, uint16(b[i])|uint16(b[i+1])<<8)
	}
	return string(utf16.Decode(u16))
}

func desktopContainsName(list []string, name string) bool {
	for _, s := range list {
		if s == name {
			return true
		}
	}
	return false
}

func desktopValidPort(p int) bool { return p >= 1 && p <= 65535 }

func desktopNonNilStrings(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// dialLabel makes one bounded TCP observation and reports only what
// happened: reachable, refused, timeout, or a bare error. It never claims
// whose service answered — that takes authenticated messh identity.
func desktopDialLabel(ctx context.Context, addr string) string {
	ctx, cancel := context.WithTimeout(ctx, desktopDialTimeout)
	defer cancel()
	c, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err == nil {
		c.Close()
		return "reachable"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "refused"
	}
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return "timeout"
	}
	return "error"
}

// ifaceAddrs reports the selected host interface's name and current IP
// address strings (no CIDR suffixes), for stale-capture comparison. It reads
// local interface state only: no exec, no network, no privilege.
func desktopIfaceAddrs(index int) (string, []string, error) {
	if index <= 0 {
		return "", nil, errors.New("no host interface selected")
	}
	iface, err := net.InterfaceByIndex(index)
	if err != nil {
		return "", nil, err
	}
	var addrs []string
	list, err := iface.Addrs()
	if err != nil {
		return iface.Name, nil, err
	}
	for _, a := range list {
		if ipnet, ok := a.(*net.IPNet); ok && ipnet.IP != nil {
			addrs = append(addrs, ipnet.IP.String())
		}
	}
	return iface.Name, addrs, nil
}
