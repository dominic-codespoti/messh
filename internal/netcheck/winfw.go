package netcheck

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Profile is a Windows Firewall profile bit mask (NET_FW_PROFILE_TYPE2).
type Profile uint32

const (
	ProfileDomain  Profile = 1
	ProfilePrivate Profile = 2
	ProfilePublic  Profile = 4
	ProfileAll     Profile = 0x7FFFFFFF
)

// String lists the profiles in the mask, e.g. "Domain,Private".
func (p Profile) String() string {
	if p&(ProfileDomain|ProfilePrivate|ProfilePublic) == ProfileDomain|ProfilePrivate|ProfilePublic {
		return "Any"
	}
	var names []string
	for _, x := range []struct {
		bit  Profile
		name string
	}{{ProfileDomain, "Domain"}, {ProfilePrivate, "Private"}, {ProfilePublic, "Public"}} {
		if p&x.bit != 0 {
			names = append(names, x.name)
		}
	}
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ",")
}

// ProfileForCategory maps a network category (Get-NetConnectionProfile's
// NetworkCategory) to the firewall profile Windows applies to it.
func ProfileForCategory(category string) Profile {
	switch strings.ToLower(category) {
	case "public":
		return ProfilePublic
	case "private":
		return ProfilePrivate
	case "domainauthenticated", "domain":
		return ProfileDomain
	}
	return 0
}

// IP protocol numbers as Windows Firewall reports them.
const (
	ProtoTCP = 6
	ProtoUDP = 17
	ProtoAny = 256
)

func protoName(p int) string {
	switch p {
	case ProtoTCP:
		return "TCP"
	case ProtoUDP:
		return "UDP"
	case ProtoAny:
		return "Any"
	}
	return strconv.Itoa(p)
}

// Rule is an enabled or disabled Windows Firewall rule.
type Rule struct {
	Name        string  `json:"name"`
	Program     string  `json:"program,omitempty"` // "" = any program
	Service     string  `json:"service,omitempty"` // "" = not restricted to a service
	Package     string  `json:"package,omitempty"` // app package (AppContainer) SID; "" = none
	Users       string  `json:"users,omitempty"`   // LocalUserAuthorizedList SDDL; "" = all users
	Owner       string  `json:"owner,omitempty"`   // LocalUserOwner SID; "" = none
	Secure      int     `json:"secure,omitempty"`  // SecureFlags; non-zero requires IPsec
	Protocol    int     `json:"protocol"`          // ProtoTCP, ProtoUDP, ProtoAny or another IP protocol number
	LocalPorts  string  `json:"local_ports,omitempty"`
	RemoteAddrs string  `json:"remote_addrs,omitempty"`
	Profiles    Profile `json:"profiles"`
	Allow       bool    `json:"allow"`
	Inbound     bool    `json:"inbound"`
	Enabled     bool    `json:"enabled"`

	// ScopeUnknown is set when the source cannot show package, user and
	// IPsec restrictions (netsh), so an any-program rule may not apply.
	ScopeUnknown bool `json:"scope_unknown,omitempty"`
}

// Restricted reports why the rule applies only to an app package, a
// service or specific users, or only to IPsec-authenticated traffic, and
// so never to the messh node; "" when it has no such restriction.
func (r Rule) Restricted() string {
	switch {
	case r.Package != "":
		return "app package " + r.Package
	case r.Service != "":
		return "service " + r.Service
	case r.Users != "" || r.Owner != "":
		return "specific users"
	case r.Secure != 0:
		return "IPsec-authenticated traffic"
	}
	return ""
}

// ConnProfile is one network connection as Get-NetConnectionProfile reports it.
type ConnProfile struct {
	Alias    string `json:"alias"`
	Index    int    `json:"index"`
	Name     string `json:"name"`     // network name, e.g. the Wi-Fi SSID
	Category string `json:"category"` // Public, Private or DomainAuthenticated
}

// FirewallProfile is the state of one firewall profile.
type FirewallProfile struct {
	Type            Profile `json:"type"`
	Enabled         bool    `json:"enabled"`
	BlockAllInbound bool    `json:"block_all_inbound"` // "Block all incoming connections", ignores allow rules
	DefaultAllow    bool    `json:"default_allow"`     // inbound allowed without a rule
}

// WinFirewall is everything the Windows checks need, from PowerShell or netsh.
type WinFirewall struct {
	Profiles         []ConnProfile
	ProfilesKnown    bool
	Current          Profile // active firewall profiles; 0 = unknown
	FirewallProfiles []FirewallProfile
	Rules            []Rule // enabled inbound rules
	RulesKnown       bool
	Source           string   // how the rules were read
	Notes            []string // why something is unknown
}

// psOutput is what PowerShellScript prints.
type psOutput struct {
	Profiles  []psProfile `json:"profiles"`
	RulesRead bool        `json:"rulesRead"`
	Current   uint32      `json:"current"`
	Firewall  []struct {
		Type                 uint32 `json:"type"`
		Enabled              bool   `json:"enabled"`
		BlockAllInbound      bool   `json:"blockAllInbound"`
		DefaultInboundAction int    `json:"defaultInboundAction"`
	} `json:"firewall"`
	Rules []struct {
		Name            string  `json:"name"`
		App             *string `json:"app"`
		Service         *string `json:"service"`
		Package         *string `json:"package"`
		Users           *string `json:"users"`
		Owner           *string `json:"owner"`
		Secure          int     `json:"secure"`
		Protocol        int     `json:"protocol"`
		LocalPorts      *string `json:"localPorts"`
		RemoteAddresses *string `json:"remoteAddresses"`
		Profiles        uint32  `json:"profiles"`
		Action          int     `json:"action"`
	} `json:"rules"`
}

type psProfile struct {
	Alias    string `json:"alias"`
	Index    int    `json:"index"`
	Name     string `json:"name"`
	Category string `json:"category"`
}

// PowerShellScript prints the connection profiles and, through the
// HNetCfg.FwPolicy2 COM object, the firewall profile state and every enabled
// inbound rule as one JSON object. The Get-NetFirewall*Filter cmdlets are not
// used: their port and address filters are access-denied without admin and
// take 6-8 s, while the COM object answers in about a second for any user.
// The script contains no double quotes so it survives command-line quoting
// unchanged, and it only reads.
const PowerShellScript = "[Console]::OutputEncoding=New-Object System.Text.UTF8Encoding $false; " +
	"$ErrorActionPreference='SilentlyContinue'; $o=[ordered]@{}; " +
	"$o.profiles=@(Get-NetConnectionProfile | ForEach-Object { [ordered]@{alias=$_.InterfaceAlias;index=$_.InterfaceIndex;name=$_.Name;category=[string]$_.NetworkCategory} }); " +
	"$f=New-Object -ComObject HNetCfg.FwPolicy2; $o.rulesRead=($f -ne $null); " +
	"if ($f) { $o.current=$f.CurrentProfileTypes; " +
	"$o.firewall=@(1,2,4 | ForEach-Object { [ordered]@{type=$_;enabled=$f.FirewallEnabled($_);blockAllInbound=$f.BlockAllInboundTraffic($_);defaultInboundAction=$f.DefaultInboundAction($_)} }); " +
	"$o.rules=@($f.Rules | Where-Object { $_.Direction -eq 1 -and $_.Enabled } | ForEach-Object { [ordered]@{name=$_.Name;app=$_.ApplicationName;service=$_.ServiceName;package=$_.LocalAppPackageId;users=$_.LocalUserAuthorizedList;owner=$_.LocalUserOwner;secure=$_.SecureFlags;protocol=$_.Protocol;localPorts=$_.LocalPorts;remoteAddresses=$_.RemoteAddresses;profiles=$_.Profiles;action=$_.Action} }) }; " +
	"ConvertTo-Json -InputObject $o -Depth 4 -Compress"

// ParsePowerShell parses the output of PowerShellScript.
func ParsePowerShell(data []byte) (WinFirewall, error) {
	var o psOutput
	if err := json.Unmarshal(trimBOM(data), &o); err != nil {
		return WinFirewall{}, fmt.Errorf("parse PowerShell output: %w", err)
	}
	fw := WinFirewall{ProfilesKnown: true, Source: "PowerShell (HNetCfg.FwPolicy2)"}
	for _, p := range o.Profiles {
		fw.Profiles = append(fw.Profiles, ConnProfile(p))
	}
	if !o.RulesRead {
		fw.Notes = append(fw.Notes, "the firewall COM object (HNetCfg.FwPolicy2) could not be created")
		return fw, nil
	}
	fw.Current = Profile(o.Current)
	for _, p := range o.Firewall {
		fw.FirewallProfiles = append(fw.FirewallProfiles, FirewallProfile{
			Type: Profile(p.Type), Enabled: p.Enabled, BlockAllInbound: p.BlockAllInbound,
			DefaultAllow: p.DefaultInboundAction == 1, // NET_FW_ACTION_ALLOW
		})
	}
	fw.RulesKnown = true
	for _, r := range o.Rules {
		fw.Rules = append(fw.Rules, Rule{
			Name:        r.Name,
			Program:     anyToEmpty(deref(r.App)),
			Service:     deref(r.Service),
			Package:     deref(r.Package),
			Users:       deref(r.Users),
			Owner:       deref(r.Owner),
			Secure:      r.Secure,
			Protocol:    r.Protocol,
			LocalPorts:  deref(r.LocalPorts),
			RemoteAddrs: deref(r.RemoteAddresses),
			Profiles:    Profile(r.Profiles),
			Allow:       r.Action == 1, // NET_FW_ACTION_ALLOW; 0 is block
			Inbound:     true,
			Enabled:     true,
		})
	}
	return fw, nil
}

// ParseProfilesJSON parses `Get-NetConnectionProfile | ConvertTo-Json`
// (an object for one connection, an array for several; NetworkCategory as
// number or name) as well as PowerShellScript's own profile objects.
func ParseProfilesJSON(data []byte) ([]ConnProfile, error) {
	data = bytes.TrimSpace(trimBOM(data))
	if len(data) == 0 {
		return nil, nil
	}
	type raw struct {
		InterfaceAlias  string          `json:"InterfaceAlias"`
		InterfaceIndex  int             `json:"InterfaceIndex"`
		Name            string          `json:"Name"`
		NetworkCategory json.RawMessage `json:"NetworkCategory"`
	}
	var list []raw
	if data[0] == '[' {
		if err := json.Unmarshal(data, &list); err != nil {
			return nil, fmt.Errorf("parse connection profiles: %w", err)
		}
	} else {
		var one raw
		if err := json.Unmarshal(data, &one); err != nil {
			return nil, fmt.Errorf("parse connection profiles: %w", err)
		}
		list = []raw{one}
	}
	out := make([]ConnProfile, 0, len(list))
	for _, r := range list {
		out = append(out, ConnProfile{Alias: r.InterfaceAlias, Index: r.InterfaceIndex, Name: r.Name, Category: categoryName(r.NetworkCategory)})
	}
	return out, nil
}

// categoryName turns NetworkCategory (0/1/2 or its enum name) into a name.
func categoryName(v json.RawMessage) string {
	var n int
	if json.Unmarshal(v, &n) == nil {
		switch n {
		case 0:
			return "Public"
		case 1:
			return "Private"
		case 2:
			return "DomainAuthenticated"
		}
		return strconv.Itoa(n)
	}
	var s string
	json.Unmarshal(v, &s)
	return s
}

// NetshRuleArgs lists every inbound rule with all fields.
var NetshRuleArgs = []string{"advfirewall", "firewall", "show", "rule", "name=all", "dir=in", "verbose"}

// NetshProfileArgs shows the active profile(s) and their state.
var NetshProfileArgs = []string{"advfirewall", "show", "currentprofile"}

// ParseNetshRules parses `netsh advfirewall firewall show rule ... verbose`.
// netsh labels are localized; only English output is understood, and an
// error is returned when no rule could be recognised in non-empty output.
func ParseNetshRules(text string) ([]Rule, error) {
	var (
		rules []Rule
		cur   *Rule
	)
	flush := func() {
		if cur != nil {
			rules = append(rules, *cur)
			cur = nil
		}
	}
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" || line[0] == ' ' || line[0] == '\t' || strings.HasPrefix(line, "---") {
			continue // blank, ICMP type/code continuation, or underline
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue // "Ok."
		}
		val = strings.TrimSpace(val)
		if key == "Rule Name" {
			flush()
			cur = &Rule{Name: val, Protocol: ProtoAny, ScopeUnknown: true}
			continue
		}
		if cur == nil {
			continue
		}
		switch key {
		case "Enabled":
			cur.Enabled = strings.EqualFold(val, "Yes")
		case "Direction":
			cur.Inbound = strings.EqualFold(val, "In")
		case "Profiles":
			cur.Profiles = parseProfileList(val)
		case "RemoteIP":
			cur.RemoteAddrs = val
		case "Protocol":
			cur.Protocol = parseProtocol(val)
		case "LocalPort":
			cur.LocalPorts = val
		case "Program":
			cur.Program = anyToEmpty(val)
		case "Service":
			cur.Service = anyToEmpty(val)
		case "Security":
			if !strings.EqualFold(val, "NotRequired") {
				cur.Secure = 1 // authentication or encryption required
			}
		case "Action":
			cur.Allow = strings.EqualFold(val, "Allow")
		}
	}
	flush()
	if len(rules) == 0 && strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), "Ok.")) != "" &&
		!strings.Contains(text, "No rules match") {
		return nil, errors.New("netsh output not understood (non-English Windows?)")
	}
	return rules, sc.Err()
}

// ParseNetshProfiles parses `netsh advfirewall show currentprofile` (or
// allprofiles): one section per profile with its state and inbound policy.
func ParseNetshProfiles(text string) []FirewallProfile {
	var (
		out []FirewallProfile
		cur *FirewallProfile
	)
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if name, ok := strings.CutSuffix(line, "Profile Settings:"); ok {
			if cur != nil {
				out = append(out, *cur)
			}
			cur = &FirewallProfile{Type: ProfileForCategory(strings.TrimSpace(name))}
			continue
		}
		if cur == nil {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch {
		case f[0] == "State":
			cur.Enabled = strings.EqualFold(f[1], "ON")
		case f[0] == "Firewall" && f[1] == "Policy" && len(f) >= 3:
			inbound, _, _ := strings.Cut(f[2], ",")
			cur.BlockAllInbound = strings.EqualFold(inbound, "BlockInboundAlways")
			cur.DefaultAllow = strings.EqualFold(inbound, "AllowInbound")
		}
	}
	if cur != nil {
		out = append(out, *cur)
	}
	return out
}

func parseProfileList(s string) Profile {
	var p Profile
	for part := range strings.SplitSeq(s, ",") {
		part = strings.TrimSpace(part)
		if strings.EqualFold(part, "Any") || strings.EqualFold(part, "All") {
			return ProfileAll
		}
		p |= ProfileForCategory(part)
	}
	return p
}

func parseProtocol(s string) int {
	switch strings.ToUpper(s) {
	case "TCP":
		return ProtoTCP
	case "UDP":
		return ProtoUDP
	case "ANY", "":
		return ProtoAny
	case "ICMPV4":
		return 1
	case "ICMPV6":
		return 58
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	return -1
}

// PortMatches reports whether a Windows local-port spec covers port: "*",
// "Any" or empty match everything; otherwise a comma list of ports and
// "lo-hi" ranges. Keywords (RPC, RPC-EPMap, IPHTTPS, Teredo, ...) never do.
func PortMatches(spec string, port int) bool {
	spec = strings.TrimSpace(spec)
	if spec == "" || spec == "*" || strings.EqualFold(spec, "Any") {
		return true
	}
	for part := range strings.SplitSeq(spec, ",") {
		part = strings.TrimSpace(part)
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			continue
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil {
				continue
			}
		}
		if a <= port && port <= b {
			return true
		}
	}
	return false
}

// ProgramMatches reports whether a rule's program applies to exe. Windows
// compares paths case-insensitively (rules it creates from the firewall
// prompt are lower-cased) and stores some with %VARIABLES%.
func ProgramMatches(ruleProgram, exe string) bool {
	if ruleProgram == "" || ruleProgram == "*" || strings.EqualFold(ruleProgram, "Any") {
		return true
	}
	if exe == "" {
		return false
	}
	return strings.EqualFold(cleanWinPath(expandWinEnv(ruleProgram)), cleanWinPath(exe))
}

func cleanWinPath(p string) string {
	return filepath.Clean(strings.ReplaceAll(p, "/", `\`))
}

// expandWinEnv expands %NAME% references; unknown names stay as they are.
func expandWinEnv(s string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '%')
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		j := strings.IndexByte(s[i+1:], '%')
		if j < 0 {
			b.WriteString(s)
			return b.String()
		}
		name := s[i+1 : i+1+j]
		b.WriteString(s[:i])
		if v, ok := os.LookupEnv(name); ok && name != "" {
			b.WriteString(v)
		} else {
			b.WriteString(s[i : i+j+2])
		}
		s = s[i+j+2:]
	}
}

// RemoteMatches reports whether a rule's remote-address spec admits peers on
// one of the LAN subnets: any address, the LocalSubnet keyword, or an IPv4
// address, range or network overlapping a subnet.
func RemoteMatches(spec string, lans []netip.Prefix) bool {
	spec = strings.TrimSpace(spec)
	if spec == "" || spec == "*" || strings.EqualFold(spec, "Any") {
		return true
	}
	for part := range strings.SplitSeq(spec, ",") {
		part = strings.TrimSpace(part)
		if strings.EqualFold(part, "LocalSubnet") {
			return true
		}
		lo, hi, ok := addrRange(part)
		if !ok {
			continue // other keywords (DefaultGateway, DNS, Internet, ...) or IPv6
		}
		for _, lan := range lans {
			lan = lan.Masked()
			first := lan.Addr()
			last := lastAddr(lan)
			if lo.Compare(last) <= 0 && hi.Compare(first) >= 0 {
				return true
			}
		}
	}
	return false
}

// addrRange parses an IPv4 address, "a-b" range, CIDR or "addr/mask".
func addrRange(s string) (lo, hi netip.Addr, ok bool) {
	if a, b, isRange := strings.Cut(s, "-"); isRange {
		x, err1 := netip.ParseAddr(a)
		y, err2 := netip.ParseAddr(b)
		if err1 != nil || err2 != nil || !x.Is4() || !y.Is4() {
			return lo, hi, false
		}
		return x, y, true
	}
	if a, m, isNet := strings.Cut(s, "/"); isNet {
		addr, err := netip.ParseAddr(a)
		if err != nil || !addr.Is4() {
			return lo, hi, false
		}
		bits, err := strconv.Atoi(m)
		if err != nil {
			mask, err := netip.ParseAddr(m)
			if err != nil || !mask.Is4() {
				return lo, hi, false
			}
			bits = maskBits(mask)
		}
		p, err := addr.Prefix(bits)
		if err != nil {
			return lo, hi, false
		}
		return p.Addr(), lastAddr(p), true
	}
	addr, err := netip.ParseAddr(s)
	if err != nil || !addr.Is4() {
		return lo, hi, false
	}
	return addr, addr, true
}

func maskBits(mask netip.Addr) int {
	b := mask.As4()
	n := 0
	for _, x := range b {
		for i := 7; i >= 0; i-- {
			if x&(1<<i) == 0 {
				return n
			}
			n++
		}
	}
	return n
}

func lastAddr(p netip.Prefix) netip.Addr {
	b := p.Masked().Addr().As4()
	host := 32 - p.Bits()
	v := uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	if host > 0 {
		v |= 1<<host - 1
	}
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}

// Verdict is how the rules treat one kind of inbound traffic on one profile.
type Verdict struct {
	Allowed []Rule
	Blocked []Rule
	// Unsure are any-program allow rules whose package, user and IPsec
	// restrictions the source could not show (netsh).
	Unsure []Rule
}

// Evaluate finds the enabled inbound rules for traffic to proto/port of exe
// on profile from one of the LAN subnets. Block rules win over allow rules.
// Rules restricted to an app package, a service or specific users, or
// requiring IPsec, never apply: the node is none of those.
func Evaluate(rules []Rule, exe string, proto, port int, profile Profile, lans []netip.Prefix) Verdict {
	var v Verdict
	for _, r := range rules {
		if !r.Enabled || !r.Inbound || r.Profiles&profile == 0 || r.Restricted() != "" {
			continue
		}
		if r.Protocol != ProtoAny && r.Protocol != proto {
			continue
		}
		if !PortMatches(r.LocalPorts, port) || !ProgramMatches(r.Program, exe) || !RemoteMatches(r.RemoteAddrs, lans) {
			continue
		}
		switch {
		case !r.Allow:
			v.Blocked = append(v.Blocked, r)
		case r.ScopeUnknown && r.Program == "":
			v.Unsure = append(v.Unsure, r)
		default:
			v.Allowed = append(v.Allowed, r)
		}
	}
	return v
}

// ProgramBlockRules returns the enabled inbound block rules naming exe itself,
// which Windows creates when its firewall prompt is dismissed or cancelled.
func ProgramBlockRules(rules []Rule, exe string) []Rule {
	var out []Rule
	for _, r := range rules {
		if r.Enabled && r.Inbound && !r.Allow && r.Program != "" && ProgramMatches(r.Program, exe) {
			out = append(out, r)
		}
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func anyToEmpty(s string) string {
	if strings.EqualFold(s, "Any") || s == "*" {
		return ""
	}
	return s
}

func trimBOM(b []byte) []byte {
	return bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
}
