package state

import (
	"errors"
	"fmt"
	"io/fs"
	"net/netip"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
)

// Default WSL scheduled-task names. The owner-context logon task boots the
// selected distro/user and starts its user service; the SYSTEM route task
// only refreshes host-native portproxy/firewall from administrator-owned
// config. The owner may trigger the route task but must not modify it.
const (
	DefaultWSLTaskName      = "messh-wsl-start"
	DefaultWSLRouteTaskName = "messh-wsl-route"
)

// WSLTargetConfig is the selected WSL target persisted in the Windows owner's
// state directory. HostAddress is the last captured explicit Windows LAN
// address and is stale-capable across DHCP; HostInterfaceIndex selects the
// adapter used to re-resolve it. GuestAddress is optional captured
// information and is never the portproxy destination: the proxy always
// forwards Windows LAN MeshPort to literal 127.0.0.1:MeshPort, which the
// proven native WSL localhost forwarder carries to the guest. GuestState is
// guest-native and never a /mnt/c Windows shared path. No credentials or
// tokens are persisted here.
type WSLTargetConfig struct {
	Distro             string   `json:"distro"`
	User               string   `json:"user"`
	Name               string   `json:"name"`
	ID                 string   `json:"id"`
	GuestState         string   `json:"guest_state"`
	HostAddress        string   `json:"host_address"`
	GuestAddress       string   `json:"guest_address,omitempty"`
	TaskName           string   `json:"task_name"`
	RouteTaskName      string   `json:"route_task_name"`
	MeshPort           int      `json:"mesh_port"`
	LocalPort          int      `json:"local_port"`
	HostInterfaceIndex int      `json:"host_interface_index"`
	AllowedPeers       []string `json:"allowed_peers"`
}

// WSLTargetFile is the owner-private JSON document holding the selection.
func (p Paths) WSLTargetFile() string { return filepath.Join(p.Root, "wsl-target.json") }

// LoadWSLTarget returns nil when no target has been configured yet.
func (p Paths) LoadWSLTarget() (*WSLTargetConfig, error) {
	var c WSLTargetConfig
	if err := readJSON(p.WSLTargetFile(), &c); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	return &c, nil
}

// SaveWSLTarget validates and publishes the selection with the existing
// owner-only atomic JSON convention.
func (p Paths) SaveWSLTarget(c WSLTargetConfig) error {
	if err := c.Validate(); err != nil {
		return err
	}
	return writeJSON(p.WSLTargetFile(), c, 0o600)
}

// WSLFirewallRuleName is the single inbound firewall rule for the WSL mesh
// port. The guest local API port is never firewalled or forwarded.
func WSLFirewallRuleName(meshPort int) string {
	return fmt.Sprintf("messh wsl (mesh TCP %d)", meshPort)
}

var (
	wslTaskNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
	wslUserRE     = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
)

// Validate rejects empty selections, unsafe task/distro/user spellings,
// non-private or non-literal scopes, guest-state paths that would share
// Windows identity through /mnt, and port/task collisions.
func (c WSLTargetConfig) Validate() error {
	if err := validateWSLDistro(c.Distro); err != nil {
		return err
	}
	if !wslUserRE.MatchString(c.User) {
		return fmt.Errorf("invalid user %q: use a POSIX username of 1-32 chars [a-z0-9_-] starting with [a-z_]", c.User)
	}
	if err := validateWSLName(c.Name); err != nil {
		return err
	}
	if err := validateWSLID(c.ID); err != nil {
		return err
	}
	if err := validateWSLGuestState(c.GuestState); err != nil {
		return err
	}
	host, err := parseWSLPrivateIPv4(c.HostAddress, "host-address")
	if err != nil {
		return err
	}
	if c.GuestAddress != "" {
		addr, err := netip.ParseAddr(strings.TrimSpace(c.GuestAddress))
		if err != nil || addr.Zone() != "" || !addr.IsValid() || addr.IsUnspecified() {
			return fmt.Errorf("invalid guest-address %q: use a literal guest IP address or leave it empty", c.GuestAddress)
		}
	}
	if !wslTaskNameRE.MatchString(c.TaskName) {
		return fmt.Errorf("invalid task-name %q: use 1-64 chars [A-Za-z0-9_-] starting with alphanumerics", c.TaskName)
	}
	if !wslTaskNameRE.MatchString(c.RouteTaskName) {
		return fmt.Errorf("invalid route-task-name %q: use 1-64 chars [A-Za-z0-9_-] starting with alphanumerics", c.RouteTaskName)
	}
	if c.TaskName == c.RouteTaskName {
		return fmt.Errorf("task-name and route-task-name must differ (both are %q)", c.TaskName)
	}
	if c.MeshPort < 1 || c.MeshPort > 65535 {
		return fmt.Errorf("invalid mesh-port %d: use 1-65535", c.MeshPort)
	}
	if c.LocalPort < 1 || c.LocalPort > 65535 {
		return fmt.Errorf("invalid local-port %d: use 1-65535", c.LocalPort)
	}
	if c.MeshPort == c.LocalPort {
		return fmt.Errorf("mesh-port and local-port must differ (both are %d): the guest local API is never forwarded", c.MeshPort)
	}
	if c.HostInterfaceIndex <= 0 {
		return fmt.Errorf("invalid interface-index %d: use the positive Windows adapter index holding the host address", c.HostInterfaceIndex)
	}
	if len(c.AllowedPeers) == 0 {
		return fmt.Errorf("at least one allowed peer is required: pass the laptop's private address with --peer")
	}
	if len(c.AllowedPeers) > 16 {
		return fmt.Errorf("%d allowed peers: keep at most 16 explicit private addresses, never a broad scope", len(c.AllowedPeers))
	}
	seen := make(map[netip.Addr]bool, len(c.AllowedPeers))
	for _, p := range c.AllowedPeers {
		addr, err := parseWSLPrivateIPv4(p, "peer")
		if err != nil {
			return err
		}
		if addr == host {
			return fmt.Errorf("peer %q equals host-address %q: peers are other devices, not this host", p, c.HostAddress)
		}
		if seen[addr] {
			return fmt.Errorf("duplicate peer %q: list each allowed address once", p)
		}
		seen[addr] = true
	}
	return nil
}

func validateWSLDistro(d string) error {
	if d == "" || len(d) > 64 || strings.TrimSpace(d) != d {
		return fmt.Errorf("invalid distro %q: use 1-64 chars without leading/trailing spaces, as `wsl --list --quiet` shows it", d)
	}
	// Distro names become bare wsl.exe arguments of the owner logon task.
	// Whitespace would need quoting that the fixed task action avoids;
	// real distribution names never contain it.
	if strings.ContainsFunc(d, unicode.IsSpace) {
		return fmt.Errorf("invalid distro %q: whitespace would change how the startup command is split", d)
	}
	for _, r := range d {
		if r == '"' || unicode.IsControl(r) || r == '‘' || r == '’' || r == '“' || r == '”' || r == '„' {
			return fmt.Errorf("invalid distro %q: contains %q, which would change how the startup command is split", d, r)
		}
	}
	return nil
}

func validateWSLName(name string) error {
	if name == "" || len(name) > 64 || strings.TrimSpace(name) != name {
		return fmt.Errorf("invalid name %q: use 1-64 printable characters without leading/trailing spaces", name)
	}
	for _, r := range name {
		if !unicode.IsPrint(r) {
			return fmt.Errorf("invalid name %q: use printable characters", name)
		}
	}
	return nil
}

func validateWSLID(id string) error {
	if len(id) != 52 {
		return fmt.Errorf("invalid id %q: use the 52-char device ID of the WSL target", id)
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= '2' && r <= '7') {
			return fmt.Errorf("invalid id %q: use the 52-char device ID of the WSL target ([a-z2-7])", id)
		}
	}
	return nil
}

func validateWSLGuestState(s string) error {
	if s == "" || len(s) > 256 || strings.TrimSpace(s) != s {
		return fmt.Errorf("invalid guest-state %q: use the absolute guest-native state directory (e.g. /home/dom/.local/state/messh)", s)
	}
	if strings.Contains(s, "\\") {
		return fmt.Errorf("invalid guest-state %q: use a guest-native path, never a Windows path or /mnt/c share", s)
	}
	if !path.IsAbs(s) || path.Clean(s) != s {
		return fmt.Errorf("invalid guest-state %q: use a cleaned absolute guest path without trailing slash or dot segments", s)
	}
	if s == "/mnt" || strings.HasPrefix(s, "/mnt/") {
		return fmt.Errorf("invalid guest-state %q: guest state must never share Windows identity through /mnt", s)
	}
	if s == "/" {
		return fmt.Errorf("invalid guest-state %q: use the messh state directory, not the filesystem root", s)
	}
	return nil
}

func parseWSLPrivateIPv4(s, what string) (netip.Addr, error) {
	trimmed := strings.TrimSpace(s)
	addr, err := netip.ParseAddr(trimmed)
	if err != nil || addr.Zone() != "" {
		return netip.Addr{}, fmt.Errorf("invalid %s %q: use a literal IPv4 address, not a name or CIDR", what, s)
	}
	if !addr.Is4() || !isRFC1918(addr) {
		return netip.Addr{}, fmt.Errorf("invalid %s %q: use an explicit private IPv4 address (10/8, 172.16/12, 192.168/16)", what, s)
	}
	return addr, nil
}

// isRFC1918 reports the three private-use ranges. Loopback, link-local,
// CGNAT and IPv6 ULA are not accepted: peers and the host address are
// explicit LAN addresses on the selected Private interface.
func isRFC1918(addr netip.Addr) bool {
	b := addr.As4()
	switch {
	case b[0] == 10:
		return true
	case b[0] == 172 && b[1] >= 16 && b[1] <= 31:
		return true
	case b[0] == 192 && b[1] == 168:
		return true
	}
	return false
}
