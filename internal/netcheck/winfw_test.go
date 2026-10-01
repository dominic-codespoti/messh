package netcheck

import (
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"messh/internal/control"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var lan = []netip.Prefix{netip.MustParsePrefix("192.168.1.25/24")}

const unityExe = `C:\Program Files\Unity\Hub\Editor\6000.0.32f1\Editor\Unity.exe`

// Sanitized PowerShellScript output with synthetic host identifiers (rules filtered).
func TestParsePowerShellHost(t *testing.T) {
	fw, err := ParsePowerShell(readFixture(t, "windows_ps_host.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !fw.ProfilesKnown || !fw.RulesKnown {
		t.Fatalf("known: profiles %v rules %v", fw.ProfilesKnown, fw.RulesKnown)
	}
	want := ConnProfile{Alias: "WiFi 2", Index: 10, Name: "ExampleLAN", Category: "Public"}
	if len(fw.Profiles) != 1 || fw.Profiles[0] != want {
		t.Fatalf("profiles = %+v", fw.Profiles)
	}
	if fw.Current != ProfilePublic {
		t.Errorf("current = %v", fw.Current)
	}
	if len(fw.FirewallProfiles) != 3 {
		t.Fatalf("firewall profiles = %+v", fw.FirewallProfiles)
	}
	for _, p := range fw.FirewallProfiles {
		if !p.Enabled || p.BlockAllInbound || p.DefaultAllow {
			t.Errorf("profile %v = %+v, want enabled, default block", p.Type, p)
		}
	}
	if len(fw.Rules) != 11 {
		t.Fatalf("got %d rules", len(fw.Rules))
	}
	u := fw.Rules[0]
	if u.Name != "Unity 6000.0.32f1 Editor" || u.Allow || u.Protocol != ProtoAny || u.Profiles != ProfilePublic || u.Program != unityExe || u.LocalPorts != "" {
		t.Errorf("block rule = %+v", u)
	}
	lm := findRule(t, fw.Rules, "LM Studio API 1234")
	if lm.Program != "" || lm.Protocol != ProtoTCP || lm.LocalPorts != "1234" || lm.Profiles != ProfileAll || !lm.Allow {
		t.Errorf("LM Studio rule = %+v", lm)
	}
}

func findRule(t *testing.T, rules []Rule, name string) Rule {
	t.Helper()
	for _, r := range rules {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("no rule %q", name)
	return Rule{}
}

func TestEvaluateHostRules(t *testing.T) {
	fw, err := ParsePowerShell(readFixture(t, "windows_ps_host.json"))
	if err != nil {
		t.Fatal(err)
	}
	// The prompt-created block rule matches whatever case the path is given in.
	v := Evaluate(fw.Rules, `c:\PROGRAM FILES\unity\hub\editor\6000.0.32F1\editor\UNITY.EXE`, ProtoTCP, 7519, ProfilePublic, lan)
	if len(v.Blocked) != 1 || v.Blocked[0].Name != "Unity 6000.0.32f1 Editor" {
		t.Errorf("Public: blocked = %+v", v.Blocked)
	}
	// ...but only on the profile it was created for.
	if v := Evaluate(fw.Rules, unityExe, ProtoTCP, 7519, ProfilePrivate, lan); len(v.Blocked) != 0 {
		t.Errorf("Private: blocked = %+v", v.Blocked)
	}
	// A port rule for any program.
	if v := Evaluate(fw.Rules, `C:\x\messh.exe`, ProtoTCP, 1234, ProfilePublic, lan); len(v.Allowed) != 1 || v.Allowed[0].Name != "LM Studio API 1234" {
		t.Errorf("port 1234: %+v", v)
	}
	if v := Evaluate(fw.Rules, `C:\x\messh.exe`, ProtoUDP, 1234, ProfilePublic, lan); len(v.Allowed) != 0 {
		t.Errorf("udp 1234 allowed by %+v", v.Allowed)
	}
	// Service-restricted rules never apply to the node, even for svchost.exe.
	if v := Evaluate(fw.Rules, `C:\WINDOWS\system32\svchost.exe`, ProtoUDP, 5004, ProfilePrivate, lan); len(v.Allowed) != 0 {
		t.Errorf("service rule matched: %+v", v.Allowed)
	}
}

func TestParseProfilesJSON(t *testing.T) {
	// Raw `Get-NetConnectionProfile | ConvertTo-Json`: one object, numeric category.
	got, err := ParseProfilesJSON(readFixture(t, "profiles_raw.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != (ConnProfile{Alias: "WiFi 2", Index: 10, Name: "ExampleLAN", Category: "Public"}) {
		t.Fatalf("got %+v", got)
	}
	multi := `[{"InterfaceAlias":"WLAN Büro","InterfaceIndex":7,"Name":"Büro","NetworkCategory":1},
	           {"InterfaceAlias":"Ethernet","InterfaceIndex":3,"Name":"corp","NetworkCategory":"DomainAuthenticated"}]`
	got, err = ParseProfilesJSON([]byte("\xef\xbb\xbf" + multi))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Alias != "WLAN Büro" || got[0].Category != "Private" || got[1].Category != "DomainAuthenticated" {
		t.Fatalf("got %+v", got)
	}
	if ProfileForCategory(got[1].Category) != ProfileDomain {
		t.Error("DomainAuthenticated should map to the Domain profile")
	}
}

// Sanitized `netsh advfirewall firewall show rule name=all dir=in verbose` output (blocks filtered).
func TestParseNetshRules(t *testing.T) {
	rules, err := ParseNetshRules(string(readFixture(t, "netsh_rules.txt")))
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 10 {
		t.Fatalf("got %d rules", len(rules))
	}
	py := rules[0]
	if py.Name != "python.exe" || !py.Enabled || !py.Inbound || py.Profiles != ProfilePublic || py.Protocol != ProtoUDP || !py.Allow || py.LocalPorts != "Any" {
		t.Errorf("python rule = %+v", py)
	}
	lm := rules[2]
	if lm.Name != "LM Studio API 1234" || lm.Program != "" || lm.Protocol != ProtoTCP || lm.LocalPorts != "1234" || lm.Profiles&ProfilePublic == 0 {
		t.Errorf("LM Studio rule = %+v", lm)
	}
	block := rules[5]
	if block.Name != "Unity 6000.0.32f1 Editor" || block.Allow || block.Profiles != ProfilePublic || block.Program != unityExe || block.Protocol != ProtoAny {
		t.Errorf("block rule = %+v", block)
	}
	mf := rules[6]
	if mf.Service != "FrameServer" || mf.LocalPorts != "554,8554-8558" || mf.RemoteAddrs != "LocalSubnet" {
		t.Errorf("media foundation rule = %+v", mf)
	}
	icmp := rules[8]
	if icmp.Enabled || icmp.Protocol != 58 || icmp.Program != "System" {
		t.Errorf("disabled ICMP rule = %+v", icmp)
	}
	if v := Evaluate(rules, unityExe, ProtoUDP, 7519, ProfilePublic, lan); len(v.Blocked) != 1 {
		t.Errorf("netsh block rule not matched: %+v", v)
	}
	if v := Evaluate(rules, unityExe, ProtoUDP, 7519, ProfileDomain, lan); len(v.Blocked) != 0 || len(v.Allowed) != 1 {
		t.Errorf("Domain: %+v", v)
	}
}

func TestParseNetshRulesEdges(t *testing.T) {
	if rules, err := ParseNetshRules("\r\nNo rules match the specified criteria.\r\n"); err != nil || len(rules) != 0 {
		t.Errorf("no rules: %v %v", rules, err)
	}
	if _, err := ParseNetshRules("Regelname:     x\r\nAktiviert:     Ja\r\n"); err == nil {
		t.Error("localized output should be reported as not understood")
	}
}

func TestParseNetshProfiles(t *testing.T) {
	got := ParseNetshProfiles(string(readFixture(t, "netsh_currentprofile.txt")))
	want := []FirewallProfile{{Type: ProfilePublic, Enabled: true}}
	if len(got) != 1 || got[0] != want[0] {
		t.Fatalf("got %+v", got)
	}
	shield := ParseNetshProfiles("Private Profile Settings: \r\n------\r\nState                                 ON\r\nFirewall Policy                       BlockInboundAlways,AllowOutbound\r\n")
	if len(shield) != 1 || shield[0].Type != ProfilePrivate || !shield[0].BlockAllInbound {
		t.Fatalf("got %+v", shield)
	}
}

func TestPortMatches(t *testing.T) {
	for _, c := range []struct {
		spec string
		want bool
	}{
		{"", true}, {"*", true}, {"Any", true},
		{"7519", true}, {"7520", false},
		{"7000-8000", true}, {"7519-7519", true}, {"7520-8000", false}, {"8000-7000", false},
		{"80,443,7500-7520", true}, {"80, 443", false},
		{"RPC-EPMap", false}, {"RPC", false}, {"IPHTTPS", false},
	} {
		if got := PortMatches(c.spec, 7519); got != c.want {
			t.Errorf("PortMatches(%q) = %v", c.spec, got)
		}
	}
}

func TestProgramMatches(t *testing.T) {
	exe := `C:\Users\Example\Projects\messh\messh.exe`
	t.Setenv("MESSHTESTROOT", `C:\Users\Example`)
	for _, c := range []struct {
		rule string
		want bool
	}{
		{"", true}, {"Any", true}, {"*", true},
		{exe, true},
		{`c:\users\example\projects\messh\messh.exe`, true},
		{`C:/Users/Example/Projects/messh/messh.exe`, true},
		{`%MESSHTESTROOT%\Projects\messh\messh.exe`, true},
		{`C:\Users\Example\Projects\messh\messh2.exe`, false},
		{"System", false},
	} {
		if got := ProgramMatches(c.rule, exe); got != c.want {
			t.Errorf("ProgramMatches(%q) = %v", c.rule, got)
		}
	}
	if ProgramMatches(exe, "") {
		t.Error("a program rule must not match an unknown executable")
	}
}

func TestRemoteMatches(t *testing.T) {
	for _, c := range []struct {
		spec string
		want bool
	}{
		{"*", true}, {"Any", true}, {"LocalSubnet", true}, {"localsubnet", true},
		{"192.168.1.0/255.255.255.0", true}, {"192.168.0.0/16", true},
		{"192.168.1.10-192.168.1.20", true}, {"192.168.1.77", true},
		{"10.0.0.0/255.0.0.0", false}, {"192.168.2.0/24", false},
		{"fe80::/64", false}, {"DefaultGateway", false},
		{"10.0.0.0/8,LocalSubnet", true},
	} {
		if got := RemoteMatches(c.spec, lan); got != c.want {
			t.Errorf("RemoteMatches(%q) = %v", c.spec, got)
		}
	}
}

func TestEvaluateProfilesAndBlockPrecedence(t *testing.T) {
	exe := `C:\messh\messh.exe`
	rules := []Rule{
		{Name: RuleMesh, Program: exe, Protocol: ProtoTCP, LocalPorts: "7519", RemoteAddrs: "LocalSubnet", Profiles: ProfilePrivate | ProfileDomain, Allow: true, Inbound: true, Enabled: true},
		{Name: "messh.exe", Program: `c:\messh\messh.exe`, Protocol: ProtoTCP, Profiles: ProfilePublic, Inbound: true, Enabled: true},
		{Name: "range", Protocol: ProtoUDP, LocalPorts: "7000-8000", RemoteAddrs: "*", Profiles: ProfileAll, Allow: true, Inbound: true, Enabled: true},
		{Name: "disabled", Protocol: ProtoAny, Profiles: ProfileAll, Allow: true, Inbound: true},
	}
	if v := Evaluate(rules, exe, ProtoTCP, 7519, ProfilePrivate, lan); len(v.Allowed) != 1 || len(v.Blocked) != 0 {
		t.Errorf("Private TCP: %+v", v)
	}
	if v := Evaluate(rules, exe, ProtoTCP, 7519, ProfilePublic, lan); len(v.Allowed) != 0 || len(v.Blocked) != 1 {
		t.Errorf("Public TCP: %+v", v)
	}
	if v := Evaluate(rules, exe, ProtoUDP, 7519, ProfilePublic, lan); len(v.Allowed) != 1 || v.Allowed[0].Name != "range" {
		t.Errorf("Public UDP: %+v", v)
	}
	if got := ProgramBlockRules(rules, exe); len(got) != 1 || got[0].Name != "messh.exe" {
		t.Errorf("block rules = %+v", got)
	}
}

// Sanitized "Microsoft Store" inbound rule read through HNetCfg.FwPolicy2
// with the properties PowerShellScript reads: any program, any port, all
// profiles, but owned by a synthetic user SID (an AppContainer rule), so
// it never admits messh.exe.
func TestMicrosoftStoreRuleDoesNotAllow(t *testing.T) {
	fw, err := ParsePowerShell(readFixture(t, "windows_ps_msstore.json"))
	if err != nil {
		t.Fatal(err)
	}
	store := findRule(t, fw.Rules, "Microsoft Store")
	if store.Program != "" || store.Owner == "" || store.Restricted() == "" {
		t.Fatalf("store rule = %+v", store)
	}
	checks := WindowsChecks(fw, hostIfaces[:1], messhExe, defaultPorts)
	for _, id := range []string{"firewall TCP 7519", "firewall UDP 7519"} {
		if c := byID(t, checks, id); c.Status != Fail || strings.Contains(c.Finding, "Microsoft Store") {
			t.Errorf("%s: %s %s", id, c.Status, c.Finding)
		}
	}
}

func TestEvaluateRestrictedRules(t *testing.T) {
	plain := Rule{Protocol: ProtoAny, Profiles: ProfileAll, Allow: true, Inbound: true, Enabled: true}
	with := func(name, program string, f func(*Rule)) Rule {
		r := plain
		r.Name, r.Program = name, program
		if f != nil {
			f(&r)
		}
		return r
	}
	restrictions := map[string]func(*Rule){
		"package": func(r *Rule) {
			r.Package = "S-1-15-2-1111111111-2222222222-3333333333-444444444-555555555-666666666-777777777"
		},
		"service": func(r *Rule) { r.Service = "FrameServer" },
		"users":   func(r *Rule) { r.Users = "O:LSD:(A;;CC;;;S-1-5-21-1-2-3-1001)" },
		"owner":   func(r *Rule) { r.Owner = "S-1-5-21-1-2-3-1001" },
		"ipsec":   func(r *Rule) { r.Secure = 1 },
	}
	for name, f := range restrictions {
		for _, program := range []string{"", messhExe} {
			rules := []Rule{with(name, program, f)}
			for _, proto := range []int{ProtoTCP, ProtoUDP} {
				if v := Evaluate(rules, messhExe, proto, 7519, ProfilePrivate, lan); len(v.Allowed)+len(v.Unsure)+len(v.Blocked) != 0 {
					t.Errorf("%s rule (program %q) matched %s: %+v", name, program, protoName(proto), v)
				}
			}
			checks := WindowsChecks(privateFirewall("test", rules), hostIfaces[:1], messhExe, defaultPorts)
			wantCheck(t, checks, "firewall TCP 7519", Fail, "messh firewall allow")
			wantCheck(t, checks, "firewall UDP 7519", Fail, "messh firewall allow")
		}
	}
	for _, r := range []Rule{with("messh program", strings.ToLower(messhExe), nil), with("any program", "", nil)} {
		for _, proto := range []int{ProtoTCP, ProtoUDP} {
			if v := Evaluate([]Rule{r}, messhExe, proto, 7519, ProfilePrivate, lan); len(v.Allowed) != 1 {
				t.Errorf("%s did not allow %s: %+v", r.Name, protoName(proto), v)
			}
		}
	}
	if v := Evaluate([]Rule{with("other program", `C:\other\app.exe`, nil)}, messhExe, ProtoTCP, 7519, ProfilePrivate, lan); len(v.Allowed) != 0 {
		t.Errorf("other program allowed: %+v", v)
	}
}

// netsh cannot show package or user restrictions, so its any-program allow
// rules are only a warning; its program rules still count.
func TestNetshAnyProgramRulesUnsure(t *testing.T) {
	rules, err := ParseNetshRules(string(readFixture(t, "netsh_rules.txt")))
	if err != nil {
		t.Fatal(err)
	}
	v := Evaluate(rules, messhExe, ProtoTCP, 1234, ProfilePublic, lan)
	if len(v.Allowed) != 0 || len(v.Unsure) != 1 || v.Unsure[0].Name != "LM Studio API 1234" {
		t.Errorf("port 1234: %+v", v)
	}
	rules = append(rules, Rule{Name: "open 7519", Protocol: ProtoAny, LocalPorts: "7519", Profiles: ProfileAll, Allow: true, Inbound: true, Enabled: true, ScopeUnknown: true})
	fw := privateFirewall("netsh", rules)
	checks := WindowsChecks(fw, hostIfaces[:1], messhExe, defaultPorts)
	c := wantCheck(t, checks, "firewall TCP 7519", Warn, "messh firewall allow")
	if !strings.Contains(c.Finding, `"open 7519"`) || !strings.Contains(c.Finding, "app package") {
		t.Errorf("TCP finding: %s", c.Finding)
	}
	wantCheck(t, checks, "firewall UDP 7519", Warn, "messh firewall allow")
	// A rule naming messh.exe is trusted even from netsh.
	fw.Rules = append(fw.Rules, Rule{Name: "messh", Program: messhExe, Protocol: ProtoTCP, LocalPorts: "7519", Profiles: ProfileAll, Allow: true, Inbound: true, Enabled: true, ScopeUnknown: true})
	wantCheck(t, WindowsChecks(fw, hostIfaces[:1], messhExe, defaultPorts), "firewall TCP 7519", OK)
}

func TestFirewallPortsFollowNode(t *testing.T) {
	if got := FirewallPorts(Node{}); got != (Ports{TCP: 7519, UDP: 7519}) {
		t.Errorf("no node: %+v", got)
	}
	node := Node{Running: true, Status: control.Status{Mesh: "127.0.0.1:17519"}}
	ports := FirewallPorts(node)
	if ports != (Ports{TCP: 17519, UDP: 7519}) {
		t.Fatalf("node on 17519: %+v", ports)
	}
	fw := privateFirewall("test", []Rule{
		{Name: RuleMesh, Program: messhExe, Protocol: ProtoTCP, LocalPorts: "7519", Profiles: ProfileAll, Allow: true, Inbound: true, Enabled: true},
		{Name: RuleDiscovery, Program: messhExe, Protocol: ProtoUDP, LocalPorts: "7519", Profiles: ProfileAll, Allow: true, Inbound: true, Enabled: true},
	})
	checks := WindowsChecks(fw, hostIfaces[:1], messhExe, ports)
	c := wantCheck(t, checks, "firewall TCP 17519", Fail, "messh firewall allow")
	if !strings.Contains(c.Finding, "TCP 17519") {
		t.Errorf("TCP finding: %s", c.Finding)
	}
	wantCheck(t, checks, "firewall UDP 7519", OK)
	fw.Rules[0].LocalPorts = "17519"
	wantCheck(t, WindowsChecks(fw, hostIfaces[:1], messhExe, ports), "firewall TCP 17519", OK)
}

// privateFirewall is WiFi 2 on a Private network with the firewall on.
func privateFirewall(source string, rules []Rule) WinFirewall {
	return WinFirewall{
		ProfilesKnown:    true,
		Profiles:         []ConnProfile{{Alias: "WiFi 2", Index: 10, Category: "Private"}},
		FirewallProfiles: []FirewallProfile{{Type: ProfilePrivate, Enabled: true}},
		RulesKnown:       true,
		Source:           source,
		Rules:            rules,
	}
}
