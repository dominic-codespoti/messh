package netcheck

import (
	"bytes"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"testing"
	"time"

	"messh/internal/control"
)

const messhExe = `C:\Users\Example\Projects\messh\messh.exe`

var defaultPorts = Ports{TCP: MeshPort, UDP: MeshPort}

var hostIfaces = []Iface{
	{Name: "WiFi 2", Index: 10, Addr: netip.MustParsePrefix("192.168.1.25/24")},
	{Name: "vEthernet (WSL (Hyper-V firewall))", Index: 45, Addr: netip.MustParsePrefix("172.20.0.1/20")},
}

func byID(t *testing.T, checks []Check, id string) Check {
	t.Helper()
	for _, c := range checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("no check %q in %+v", id, checks)
	return Check{}
}

func wantCheck(t *testing.T, checks []Check, id string, status Status, fix ...string) Check {
	t.Helper()
	c := byID(t, checks, id)
	if c.Status != status {
		t.Errorf("%s: status %s, want %s (%s)", id, c.Status, status, c.Finding)
	}
	if !slices.Equal(c.Fix, fix) {
		t.Errorf("%s: fix %q, want %q", id, c.Fix, fix)
	}
	return c
}

// Synthetic desktop: WiFi 2 is Public and a dismissed prompt left
// block rules for messh.exe.
func TestWindowsChecksPublicWithBlockRules(t *testing.T) {
	fw, err := ParsePowerShell(readFixture(t, "windows_ps_host.json"))
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(messhExe)
	fw.Rules = append(fw.Rules,
		Rule{Name: "messh.exe", Program: lower, Protocol: ProtoTCP, Profiles: ProfilePublic, Inbound: true, Enabled: true},
		Rule{Name: "messh.exe", Program: lower, Protocol: ProtoUDP, Profiles: ProfilePublic, Inbound: true, Enabled: true},
	)
	checks := WindowsChecks(fw, hostIfaces, messhExe, defaultPorts)
	fix := `messh firewall allow --private "WiFi 2"`
	c := wantCheck(t, checks, "iface WiFi 2", Warn, fix)
	if !strings.Contains(c.Finding, "192.168.1.25/24") || !strings.Contains(c.Finding, "Public") {
		t.Errorf("iface finding: %s", c.Finding)
	}
	wantCheck(t, checks, "iface vEthernet (WSL (Hyper-V firewall))", OK)
	wantCheck(t, checks, "firewall", OK)
	c = wantCheck(t, checks, "firewall TCP 7519", Fail, fix)
	if !strings.Contains(c.Finding, `blocked by rule "messh.exe"`) {
		t.Errorf("TCP finding: %s", c.Finding)
	}
	wantCheck(t, checks, "firewall UDP 7519", Fail, fix)
	wantCheck(t, checks, "firewall block rules", Fail, fix)
}

func TestWindowsChecksAfterAllow(t *testing.T) {
	fw := WinFirewall{
		ProfilesKnown:    true,
		Profiles:         []ConnProfile{{Alias: "WiFi 2", Index: 10, Name: "ExampleLAN", Category: "Private"}},
		Current:          ProfilePrivate,
		FirewallProfiles: []FirewallProfile{{Type: ProfileDomain, Enabled: true}, {Type: ProfilePrivate, Enabled: true}, {Type: ProfilePublic, Enabled: true}},
		RulesKnown:       true,
		Source:           "test",
		Rules: []Rule{
			{Name: RuleMesh, Program: messhExe, Protocol: ProtoTCP, LocalPorts: "7519", RemoteAddrs: "LocalSubnet", Profiles: ProfilePrivate | ProfileDomain, Allow: true, Inbound: true, Enabled: true},
			{Name: RuleDiscovery, Program: messhExe, Protocol: ProtoUDP, LocalPorts: "7519", RemoteAddrs: "LocalSubnet", Profiles: ProfilePrivate | ProfileDomain, Allow: true, Inbound: true, Enabled: true},
		},
	}
	checks := WindowsChecks(fw, hostIfaces[:1], messhExe, defaultPorts)
	for _, c := range checks {
		if c.Status != OK || len(c.Fix) > 0 {
			t.Errorf("%s: %s %s %q", c.ID, c.Status, c.Finding, c.Fix)
		}
	}
	tcp := byID(t, checks, "firewall TCP 7519")
	if !strings.Contains(tcp.Finding, RuleMesh) || !strings.Contains(tcp.Finding, "remote LocalSubnet") {
		t.Errorf("TCP finding: %s", tcp.Finding)
	}
	// The same rules do nothing for another program.
	other := WindowsChecks(fw, hostIfaces[:1], `C:\elsewhere\messh.exe`, defaultPorts)
	wantCheck(t, other, "firewall TCP 7519", Fail, "messh firewall allow")
}

func TestWindowsChecksEdgeStates(t *testing.T) {
	base := WinFirewall{
		ProfilesKnown: true,
		Profiles:      []ConnProfile{{Alias: "WiFi 2", Index: 10, Category: "Private"}},
		RulesKnown:    true,
		Source:        "test",
	}

	// "Block all incoming connections" ignores every allow rule.
	fw := base
	fw.FirewallProfiles = []FirewallProfile{{Type: ProfilePrivate, Enabled: true, BlockAllInbound: true}}
	c := byID(t, WindowsChecks(fw, hostIfaces[:1], messhExe, defaultPorts), "firewall")
	if c.Status != Fail || len(c.Fix) != 1 || !strings.HasPrefix(c.Fix[0], "netsh advfirewall set privateprofile firewallpolicy blockinbound,allowoutbound") {
		t.Errorf("block-all: %+v", c)
	}

	// Firewall off: nothing to allow.
	fw = base
	fw.FirewallProfiles = []FirewallProfile{{Type: ProfilePrivate, Enabled: false}}
	checks := WindowsChecks(fw, hostIfaces[:1], messhExe, defaultPorts)
	wantCheck(t, checks, "firewall TCP 7519", OK)
	wantCheck(t, checks, "firewall UDP 7519", OK)

	// An allow rule whose remote addresses exclude the LAN does not count.
	fw = base
	fw.FirewallProfiles = []FirewallProfile{{Type: ProfilePrivate, Enabled: true}}
	fw.Rules = []Rule{{Name: "corp only", Protocol: ProtoTCP, LocalPorts: "7519", RemoteAddrs: "10.0.0.0/255.0.0.0", Profiles: ProfileAll, Allow: true, Inbound: true, Enabled: true}}
	wantCheck(t, WindowsChecks(fw, hostIfaces[:1], messhExe, defaultPorts), "firewall TCP 7519", Fail, "messh firewall allow")

	// A block rule on a profile that is not active is only a warning.
	fw.Rules = []Rule{{Name: "messh.exe", Program: messhExe, Protocol: ProtoAny, Profiles: ProfilePublic, Inbound: true, Enabled: true}}
	wantCheck(t, WindowsChecks(fw, hostIfaces[:1], messhExe, defaultPorts), "firewall block rules", Warn, "messh firewall allow")

	// PowerShell and netsh both failed.
	unknown := WinFirewall{Notes: []string{"PowerShell: timed out"}}
	checks = WindowsChecks(unknown, hostIfaces[:1], messhExe, defaultPorts)
	for _, id := range []string{"iface WiFi 2", "firewall", "firewall TCP 7519", "firewall UDP 7519"} {
		if c := byID(t, checks, id); c.Status != Unknown || !strings.Contains(c.Finding, "timed out") {
			t.Errorf("%s: %+v", id, c)
		}
	}
}

func TestLinuxChecks(t *testing.T) {
	ifaces := []Iface{{Name: "wlan0", Index: 3, Addr: netip.MustParsePrefix("192.168.1.20/24")}}
	ufw, _ := ParseUFW(string(readFixture(t, "ufw_active.txt")))
	checks := LinuxChecks(LinuxFirewall{UFWInstalled: true, UFWActive: true, UFW: &ufw}, ifaces)
	wantCheck(t, checks, "iface wlan0", OK)
	wantCheck(t, checks, "firewall", OK)
	wantCheck(t, checks, "firewall TCP 7519", OK)
	wantCheck(t, checks, "firewall UDP 7519", OK)

	ufwFix := []string{
		"sudo ufw allow from 192.168.1.0/24 to any port 7519 proto tcp",
		"sudo ufw allow from 192.168.1.0/24 to any port 7519 proto udp",
	}
	checks = LinuxChecks(LinuxFirewall{UFWInstalled: true, UFWActive: true, UFWNote: "ERROR: You need to be root to run this script"}, ifaces)
	wantCheck(t, checks, "firewall", Unknown)
	wantCheck(t, checks, "firewall TCP 7519", Unknown, ufwFix...)

	zone, _ := ParseFirewalld("home (active)\n  target: default\n  interfaces: wlan0\n  ports: 22/tcp\n")
	checks = LinuxChecks(LinuxFirewall{FirewalldRunning: true, Firewalld: []FirewalldZone{zone}}, ifaces)
	c := wantCheck(t, checks, "firewall TCP 7519", Fail,
		`sudo firewall-cmd --permanent --zone=home --add-rich-rule='rule family="ipv4" source address="192.168.1.0/24" port port="7519" protocol="tcp" accept'`,
		`sudo firewall-cmd --permanent --zone=home --add-rich-rule='rule family="ipv4" source address="192.168.1.0/24" port port="7519" protocol="udp" accept'`,
		"sudo firewall-cmd --reload")
	if !strings.Contains(c.Finding, "firewalld") {
		t.Errorf("finding: %s", c.Finding)
	}

	checks = LinuxChecks(LinuxFirewall{NftInstalled: true, NftNote: "Operation not permitted"}, ifaces)
	wantCheck(t, checks, "firewall", Unknown)
	wantCheck(t, checks, "firewall UDP 7519", Unknown,
		"sudo nft insert rule inet filter input ip saddr 192.168.1.0/24 tcp dport 7519 accept",
		"sudo nft insert rule inet filter input ip saddr 192.168.1.0/24 udp dport 7519 accept")

	checks = LinuxChecks(LinuxFirewall{NftInstalled: true, NftRead: true}, ifaces)
	wantCheck(t, checks, "firewall TCP 7519", OK)
}

func TestNodeChecks(t *testing.T) {
	lanIPs := []netip.Addr{netip.MustParseAddr("192.168.1.25")}
	down := Node{Problem: "is not running (no run.json in X)"}
	checks := NodeChecks(down, lanIPs)
	if len(checks) != 1 {
		t.Fatalf("checks = %+v", checks)
	}
	wantCheck(t, checks, "node", Fail, "messh node")
	if MeshChecks(down, time.Now()) != nil {
		t.Error("mesh checks need a running node")
	}

	for _, c := range []struct {
		mesh   string
		status Status
	}{
		{"[::]:7519", OK},
		{"0.0.0.0:7519", OK},
		{"192.168.1.25:7519", OK},
		{"127.0.0.1:7519", Fail},
		{"10.9.9.9:7519", Warn},
		{"[::]:7600", Warn},
	} {
		n := Node{Running: true, Status: control.Status{ID: "self", Name: "desktop", Mesh: c.mesh, Local: "127.0.0.1:7520"}}
		if got := byID(t, NodeChecks(n, lanIPs), "mesh listen"); got.Status != c.status {
			t.Errorf("mesh %s: %s (%s)", c.mesh, got.Status, got.Finding)
		}
	}
}

func TestMeshChecks(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	n := Node{
		Running: true,
		Status:  control.Status{ID: "self", Name: "desktop"},
		Peers: []control.PeerStatus{
			{ID: "pi", Name: "pi", Online: true, LastSeen: now.Add(-10 * time.Second), Addrs: []string{"192.168.1.50:7519"}},
			{ID: "laptop", Name: "laptop", Addrs: []string{"192.168.1.60:7519"}},
		},
		Seen: []control.Sighting{
			{ID: "self", Name: "desktop", Seen: now},
			{ID: "pi", Name: "pi", Addr: "192.168.1.50:7519", Seen: now.Add(-20 * time.Second)},
		},
	}
	checks := MeshChecks(n, now)
	c := wantCheck(t, checks, "discovery", OK)
	if !strings.Contains(c.Finding, "pi (192.168.1.50:7519)") || strings.Contains(c.Finding, "desktop") {
		t.Errorf("discovery finding: %s", c.Finding)
	}
	wantCheck(t, checks, "peer pi", OK)
	c = wantCheck(t, checks, "peer laptop", Warn, "on laptop: messh doctor")
	if !strings.Contains(c.Finding, "never") || !strings.Contains(c.Finding, "192.168.1.60:7519") {
		t.Errorf("offline finding: %s", c.Finding)
	}

	n.Seen = n.Seen[:1] // only our own announcement
	wantCheck(t, MeshChecks(n, now), "discovery", Warn)
	n.Seen = []control.Sighting{{ID: "pi", Name: "pi", Seen: now.Add(-10 * time.Minute)}}
	if c := byID(t, MeshChecks(n, now), "discovery"); c.Status != Warn || !strings.Contains(c.Finding, "10m0s ago") {
		t.Errorf("stale discovery: %+v", c)
	}
	n.SeenErr = errors.New("boom")
	wantCheck(t, MeshChecks(n, now), "discovery", Unknown)

	n.Peers, n.SeenErr = nil, nil
	wantCheck(t, MeshChecks(n, now), "peers", Warn, "messh pair accept   (here; then `messh pair NAME` on the other device)")
}

func TestMulticastChecks(t *testing.T) {
	checks := MulticastChecks(hostIfaces, func(index int) error {
		if index == 45 {
			return errors.New("no such device")
		}
		return nil
	})
	wantCheck(t, checks, "multicast WiFi 2", OK)
	if c := wantCheck(t, checks, "multicast vEthernet (WSL (Hyper-V firewall))", Fail); !strings.Contains(c.Finding, "239.255.75.19") {
		t.Errorf("finding: %s", c.Finding)
	}
}

func TestRender(t *testing.T) {
	fix := `messh firewall allow --private "WiFi 2"`
	r := Report{Checks: []Check{
		{ID: "node", Status: OK, Finding: "running"},
		{ID: "iface WiFi 2", Status: Warn, Finding: "network\tcategory\nPublic", Fix: []string{fix}},
		{ID: "firewall TCP 7519", Status: Fail, Finding: "no rule allows it", Fix: []string{fix}},
		{ID: "firewall UDP 7519", Status: Fail, Finding: "no rule allows discovery", Fix: []string{fix, "x"}},
	}}
	var b bytes.Buffer
	if err := r.Render(&b); err != nil {
		t.Fatal(err)
	}
	row := func(a, b, c string) string { return fmt.Sprintf("%-8s%-19s%s\n", a, b, c) }
	want := row("STATUS", "CHECK", "FINDING") +
		row("ok", "node", "running") +
		row("warn", "iface WiFi 2", "network category Public") +
		row("fail", "firewall TCP 7519", "no rule allows it") +
		row("fail", "firewall UDP 7519", "no rule allows discovery") +
		"\nFix:\n" +
		"  1. " + fix + "\n" +
		"  2. x\n"
	if b.String() != want {
		t.Errorf("Render =\n%s\nwant\n%s", b.String(), want)
	}
	if r.Worst() != Fail || r.Count(Fail) != 2 {
		t.Errorf("worst %s, fails %d", r.Worst(), r.Count(Fail))
	}
	if got := r.Filter("firewall"); len(got.Checks) != 2 {
		t.Errorf("filter = %+v", got.Checks)
	}

	b.Reset()
	if err := (Report{Checks: []Check{{ID: "node", Status: OK, Finding: "running"}}}).Render(&b); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(b.String(), "\nNo fixes needed.\n") {
		t.Errorf("no-fix render: %q", b.String())
	}
}
