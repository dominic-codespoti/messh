package netcheck

import (
	"net/netip"
	"slices"
	"strings"
	"testing"
)

const progFiles = `C:\Program Files\messh\messh.exe`

func TestPlanWindowsAllowArgv(t *testing.T) {
	blocks := []Rule{
		{Name: "messh.exe", Program: `c:\program files\messh\messh.exe`, Protocol: ProtoTCP, Profiles: ProfilePublic, Inbound: true, Enabled: true},
		{Name: `evil" dir=out`, Program: `c:\program files\messh\messh.exe`, Protocol: ProtoUDP, Profiles: ProfilePublic, Inbound: true, Enabled: true},
	}
	p, err := PlanWindowsAllow(progFiles, blocks, "WiFi 2")
	if err != nil {
		t.Fatal(err)
	}
	want := []Step{
		{Args: []string{"advfirewall", "firewall", "delete", "rule", "name=messh (mesh TCP 7519)"}, IgnoreFailure: true},
		{Args: []string{"advfirewall", "firewall", "delete", "rule", "name=messh (discovery UDP 7519)"}, IgnoreFailure: true},
		{Args: []string{"advfirewall", "firewall", "delete", "rule", "name=messh.exe", "dir=in", `program=c:\program files\messh\messh.exe`}, IgnoreFailure: true},
		{Args: []string{"advfirewall", "firewall", "add", "rule", "name=messh (mesh TCP 7519)", "dir=in", "action=allow",
			`program=C:\Program Files\messh\messh.exe`, "protocol=TCP", "localport=7519", "remoteip=LocalSubnet", "profile=private,domain", "enable=yes"}},
		{Args: []string{"advfirewall", "firewall", "add", "rule", "name=messh (discovery UDP 7519)", "dir=in", "action=allow",
			`program=C:\Program Files\messh\messh.exe`, "protocol=UDP", "localport=7519", "remoteip=LocalSubnet", "profile=private,domain", "enable=yes"}},
	}
	if len(p.Steps) != len(want) {
		t.Fatalf("got %d steps: %+v", len(p.Steps), p.Steps)
	}
	for i := range want {
		if !slices.Equal(p.Steps[i].Args, want[i].Args) || p.Steps[i].IgnoreFailure != want[i].IgnoreFailure {
			t.Errorf("step %d = %q (ignore %v), want %q (ignore %v)", i, p.Steps[i].Args, p.Steps[i].IgnoreFailure, want[i].Args, want[i].IgnoreFailure)
		}
	}
	if p.SetPrivate != "WiFi 2" {
		t.Errorf("SetPrivate = %q", p.SetPrivate)
	}
	if len(p.Skipped) != 1 || !strings.Contains(p.Skipped[0], "evil") {
		t.Errorf("unsafe block rule name not skipped: %q", p.Skipped)
	}

	lines := p.Lines()
	wantLines := []string{
		`netsh advfirewall firewall delete rule "name=messh (mesh TCP 7519)"`,
		`netsh advfirewall firewall delete rule "name=messh (discovery UDP 7519)"`,
		`netsh advfirewall firewall delete rule name=messh.exe dir=in "program=c:\program files\messh\messh.exe"`,
		`netsh advfirewall firewall add rule "name=messh (mesh TCP 7519)" dir=in action=allow "program=C:\Program Files\messh\messh.exe" protocol=TCP localport=7519 remoteip=LocalSubnet profile=private,domain enable=yes`,
		`netsh advfirewall firewall add rule "name=messh (discovery UDP 7519)" dir=in action=allow "program=C:\Program Files\messh\messh.exe" protocol=UDP localport=7519 remoteip=LocalSubnet profile=private,domain enable=yes`,
		`powershell -NoProfile -Command "Set-NetConnectionProfile -InterfaceAlias 'WiFi 2' -NetworkCategory Private"`,
	}
	if !slices.Equal(lines, wantLines) {
		t.Errorf("Lines() =\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(wantLines, "\n"))
	}
}

func TestPlanWindowsRemove(t *testing.T) {
	got := PlanWindowsRemove().Lines()
	want := []string{
		`netsh advfirewall firewall delete rule "name=messh (mesh TCP 7519)"`,
		`netsh advfirewall firewall delete rule "name=messh (discovery UDP 7519)"`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("got %q", got)
	}
}

func TestPlanWindowsAllowRejectsBadProgram(t *testing.T) {
	for _, exe := range []string{"", "messh.exe", `C:\a"b\messh.exe`, "C:\\a\nb\\messh.exe"} {
		if _, err := PlanWindowsAllow(exe, nil, ""); err == nil {
			t.Errorf("program %q accepted", exe)
		}
	}
	if _, err := PlanWindowsAllow(`\\server\share\messh.exe`, nil, ""); err != nil {
		t.Errorf("UNC path rejected: %v", err)
	}
}

func TestScriptQuoting(t *testing.T) {
	exe := `C:\Users\Test'user\messh\messh.exe`
	p, err := PlanWindowsAllow(exe, nil, "Wi’Fi 'Büro'")
	if err != nil {
		t.Fatal(err)
	}
	s, err := p.Script(`C:\Users\Test'user\AppData\Local\Temp\messh-firewall-1.log`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsRune(s, '"') {
		t.Fatalf("script contains a double quote: %s", s)
	}
	for _, want := range []string{
		`$log='C:\Users\Test''user\AppData\Local\Temp\messh-firewall-1.log'`,
		`Step $true @('advfirewall','firewall','delete','rule','name=messh (mesh TCP 7519)');`,
		`Step $false @('advfirewall','firewall','add','rule','name=messh (mesh TCP 7519)','dir=in','action=allow','program=C:\Users\Test''user\messh\messh.exe','protocol=TCP','localport=7519','remoteip=LocalSubnet','profile=private,domain','enable=yes');`,
		`Set-NetConnectionProfile -InterfaceAlias 'Wi’’Fi ''Büro''' -NetworkCategory Private`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script lacks %s\nscript: %s", want, s)
		}
	}
	if !strings.HasSuffix(s, "exit $failed") {
		t.Errorf("script must end with its exit code: %s", s)
	}
	if _, err := p.Script(`C:\tmp\"x.log`); err == nil {
		t.Error("double quote in the log path accepted")
	}
}

func TestPSQuote(t *testing.T) {
	for _, c := range [][2]string{
		{"WiFi 2", `'WiFi 2'`},
		{"it's", `'it''s'`},
		{"a\u2018b\u201Bc", "'a\u2018\u2018b\u201B\u201Bc'"},
		{"$env:x; rm", `'$env:x; rm'`},
	} {
		if got := psQuote(c[0]); got != c[1] {
			t.Errorf("psQuote(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

func TestQuoteWinArg(t *testing.T) {
	for _, c := range [][2]string{
		{"dir=in", "dir=in"},
		{"name=a b", `"name=a b"`},
		{`program=C:\Program Files\x\`, `"program=C:\Program Files\x\\"`},
		{`a"b`, `"a\"b"`},
		{"", `""`},
	} {
		if got := QuoteWinArg(c[0]); got != c[1] {
			t.Errorf("QuoteWinArg(%q) = %q, want %q", c[0], got, c[1])
		}
	}
}

func TestValidateAlias(t *testing.T) {
	profiles := []ConnProfile{
		{Alias: "WiFi 2", Index: 10, Name: "ExampleLAN", Category: "Public"},
		{Alias: "WLAN Büro", Index: 7, Name: "Büro", Category: "Private"},
		{Alias: "bad\nalias", Index: 9, Category: "Public"},
	}
	if p, err := ValidateAlias("wifi 2", profiles); err != nil || p.Alias != "WiFi 2" {
		t.Errorf("case-insensitive match: %+v %v", p, err)
	}
	if p, err := ValidateAlias("WLAN Büro", profiles); err != nil || p.Index != 7 {
		t.Errorf("unicode alias: %+v %v", p, err)
	}
	for _, bad := range []string{
		"", "  ", "WiFi 3", "Ethernet",
		`WiFi 2"; Remove-Item C:\ -Recurse; "`,
		"WiFi 2'; Set-NetFirewallProfile -Enabled False; '",
		"bad\nalias",
		"WiFi 2\x00",
	} {
		if _, err := ValidateAlias(bad, profiles); err == nil {
			t.Errorf("alias %q accepted", bad)
		}
	}
	if _, err := ValidateAlias("WiFi 2", nil); err == nil {
		t.Error("alias accepted with no known networks")
	}
}

func TestLinuxAllowCommands(t *testing.T) {
	subnets := []netip.Prefix{netip.MustParsePrefix("192.168.1.25/24"), netip.MustParsePrefix("192.168.1.40/24"), netip.MustParsePrefix("fe80::1/64")}
	got := LinuxAllowCommands(LinuxTarget{Tool: "ufw"}, subnets)
	want := []string{
		"sudo ufw allow from 192.168.1.0/24 to any port 7519 proto tcp",
		"sudo ufw allow from 192.168.1.0/24 to any port 7519 proto udp",
	}
	if !slices.Equal(got, want) {
		t.Errorf("ufw: %q", got)
	}
	got = LinuxAllowCommands(LinuxTarget{Tool: "firewalld", Zone: "home"}, subnets)
	want = []string{
		`sudo firewall-cmd --permanent --zone=home --add-rich-rule='rule family="ipv4" source address="192.168.1.0/24" port port="7519" protocol="tcp" accept'`,
		`sudo firewall-cmd --permanent --zone=home --add-rich-rule='rule family="ipv4" source address="192.168.1.0/24" port port="7519" protocol="udp" accept'`,
		"sudo firewall-cmd --reload",
	}
	if !slices.Equal(got, want) {
		t.Errorf("firewalld: %q", got)
	}
	got = LinuxAllowCommands(LinuxTarget{Tool: "nftables", NftFamily: "inet", NftTable: "filter", NftChain: "input"}, subnets)
	want = []string{
		"sudo nft insert rule inet filter input ip saddr 192.168.1.0/24 tcp dport 7519 accept",
		"sudo nft insert rule inet filter input ip saddr 192.168.1.0/24 udp dport 7519 accept",
	}
	if !slices.Equal(got, want) {
		t.Errorf("nftables: %q", got)
	}
	if got := LinuxAllowCommands(LinuxTarget{}, subnets); len(got) != 0 {
		t.Errorf("no firewall: %q", got)
	}
	got = LinuxRemoveCommands(LinuxTarget{Tool: "ufw"}, subnets[:1])
	if !slices.Equal(got, []string{
		"sudo ufw delete allow from 192.168.1.0/24 to any port 7519 proto tcp",
		"sudo ufw delete allow from 192.168.1.0/24 to any port 7519 proto udp",
	}) {
		t.Errorf("ufw remove: %q", got)
	}
}
