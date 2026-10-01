package netcheck

import (
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// Step is one netsh invocation of a Windows plan.
type Step struct {
	Args          []string // arguments after netsh.exe, one element each
	IgnoreFailure bool     // deleting a rule that does not exist fails harmlessly
}

// WinPlan is what `messh firewall allow|remove` runs elevated on Windows.
type WinPlan struct {
	Steps      []Step
	SetPrivate string   // connection alias to switch to the Private category; "" = none
	Skipped    []string // block rules left alone, with the reason
}

// PlanWindowsAllow replaces messh's inbound rules for exe: it deletes the
// previous messh rules and the block rules Windows created for exe, then adds
// TCP and UDP 7519 allow rules for exe limited to LocalSubnet on the Private
// and Domain profiles. privateAlias, when not empty, must already be
// validated with ValidateAlias.
func PlanWindowsAllow(exe string, blocks []Rule, privateAlias string) (WinPlan, error) {
	if err := checkProgram(exe); err != nil {
		return WinPlan{}, err
	}
	p := WinPlan{SetPrivate: privateAlias}
	p.Steps = append(p.Steps, PlanWindowsRemove().Steps...)
	for _, b := range blocks {
		if err := safeValue(b.Name); err != nil {
			p.Skipped = append(p.Skipped, fmt.Sprintf("%q: %v; delete it in wf.msc", b.Name, err))
			continue
		}
		if err := safeValue(b.Program); err != nil {
			p.Skipped = append(p.Skipped, fmt.Sprintf("%q: program %v; delete it in wf.msc", b.Name, err))
			continue
		}
		// netsh cannot filter deletions by action; name + direction +
		// program selects exactly the prompt-created rules for exe.
		p.Steps = append(p.Steps, Step{Args: []string{"advfirewall", "firewall", "delete", "rule", "name=" + b.Name, "dir=in", "program=" + b.Program}, IgnoreFailure: true})
	}
	for _, r := range []struct{ name, proto string }{{RuleMesh, "TCP"}, {RuleDiscovery, "UDP"}} {
		p.Steps = append(p.Steps, Step{Args: []string{
			"advfirewall", "firewall", "add", "rule",
			"name=" + r.name, "dir=in", "action=allow", "program=" + exe,
			"protocol=" + r.proto, "localport=7519", "remoteip=LocalSubnet",
			"profile=private,domain", "enable=yes",
		}})
	}
	return p, nil
}

// PlanWindowsRemove deletes the rules PlanWindowsAllow adds.
func PlanWindowsRemove() WinPlan {
	var p WinPlan
	for _, name := range []string{RuleMesh, RuleDiscovery} {
		p.Steps = append(p.Steps, Step{Args: []string{"advfirewall", "firewall", "delete", "rule", "name=" + name}, IgnoreFailure: true})
	}
	return p
}

// Lines renders the plan as commands to type into an elevated prompt.
func (p WinPlan) Lines() []string {
	var out []string
	for _, s := range p.Steps {
		parts := []string{"netsh"}
		for _, a := range s.Args {
			parts = append(parts, QuoteWinArg(a))
		}
		out = append(out, strings.Join(parts, " "))
	}
	if p.SetPrivate != "" {
		out = append(out, "powershell -NoProfile -Command \"Set-NetConnectionProfile -InterfaceAlias "+psQuote(p.SetPrivate)+" -NetworkCategory Private\"")
	}
	return out
}

// Script renders the plan as one line of PowerShell for
// `powershell.exe -NoProfile -NonInteractive -Command "<script>"`, appending
// every command and its output to logPath and exiting 1 if a required step
// failed. Every value is a single-quoted PowerShell literal, and the script
// holds no double quote, so neither the command line nor PowerShell can
// reinterpret a path, rule name or alias.
func (p WinPlan) Script(logPath string) (string, error) {
	values := []string{logPath, p.SetPrivate}
	for _, s := range p.Steps {
		values = append(values, s.Args...)
	}
	for _, v := range values {
		if err := safeValue(v); err != nil {
			return "", fmt.Errorf("refusing to build the elevated command: %q %v", v, err)
		}
	}
	var b strings.Builder
	// The elevated process resolves netsh from System32, never from PATH.
	b.WriteString("$log=" + psQuote(logPath) + "; $netsh=Join-Path $env:SystemRoot 'System32\\netsh.exe'; $failed=0; ")
	b.WriteString("function Step([bool]$ignore, [string[]]$a) { ('> netsh ' + ($a -join ' ')) | Out-File -LiteralPath $log -Append -Encoding utf8; ")
	b.WriteString("& $netsh @a 2>&1 | Out-File -LiteralPath $log -Append -Encoding utf8; ")
	b.WriteString("if (-not $ignore -and $LASTEXITCODE -ne 0) { $script:failed=1 } }; ")
	for _, s := range p.Steps {
		quoted := make([]string, len(s.Args))
		for i, a := range s.Args {
			quoted[i] = psQuote(a)
		}
		ignore := "$false"
		if s.IgnoreFailure {
			ignore = "$true"
		}
		b.WriteString("Step " + ignore + " @(" + strings.Join(quoted, ",") + "); ")
	}
	if p.SetPrivate != "" {
		a := psQuote(p.SetPrivate)
		b.WriteString("try { Set-NetConnectionProfile -InterfaceAlias " + a + " -NetworkCategory Private -ErrorAction Stop; ")
		b.WriteString("('> network ' + " + a + " + ' set to Private') | Out-File -LiteralPath $log -Append -Encoding utf8 } ")
		b.WriteString("catch { ($_ | Out-String) | Out-File -LiteralPath $log -Append -Encoding utf8; $failed=1 }; ")
	}
	b.WriteString("exit $failed")
	s := b.String()
	if strings.ContainsRune(s, '"') {
		return "", errors.New("internal error: elevated script contains a double quote")
	}
	return s, nil
}

// psQuote makes s a single-quoted PowerShell string literal. PowerShell also
// treats the typographic single quotes as quote characters, so every one of
// them is doubled.
func psQuote(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		switch r {
		case '\'', '\u2018', '\u2019', '\u201A', '\u201B':
			b.WriteRune(r)
		}
		b.WriteRune(r)
	}
	b.WriteByte('\'')
	return b.String()
}

// QuoteWinArg quotes an argument the way Windows programs split their
// command line (CommandLineToArgvW rules), for display and copy-paste.
func QuoteWinArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for _, r := range s {
		switch r {
		case '\\':
			slashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, slashes+1))
			slashes = 0
		default:
			slashes = 0
		}
		b.WriteRune(r)
	}
	b.WriteString(strings.Repeat(`\`, slashes))
	b.WriteByte('"')
	return b.String()
}

// safeValue rejects characters that could change how a command line or the
// elevated script is split: double quotes and control characters.
func safeValue(s string) error {
	for _, r := range s {
		if r == '"' || unicode.IsControl(r) || r == '\u201C' || r == '\u201D' || r == '\u201E' {
			return fmt.Errorf("contains %q", r)
		}
	}
	return nil
}

func checkProgram(exe string) error {
	if exe == "" || !filepath.IsAbs(exe) && !strings.HasPrefix(exe, `\\`) && !winAbs.MatchString(exe) {
		return fmt.Errorf("program path %q is not absolute", exe)
	}
	if err := safeValue(exe); err != nil {
		return fmt.Errorf("program path %q %v", exe, err)
	}
	return nil
}

var winAbs = regexp.MustCompile(`^[A-Za-z]:\\`)

// ValidateAlias accepts alias only when it names one of the connection
// profiles Get-NetConnectionProfile reported (compared case-insensitively,
// as Windows does) and returns the alias exactly as Windows spells it.
func ValidateAlias(alias string, profiles []ConnProfile) (ConnProfile, error) {
	if strings.TrimSpace(alias) == "" {
		return ConnProfile{}, errors.New("empty network alias")
	}
	if err := safeValue(alias); err != nil {
		return ConnProfile{}, fmt.Errorf("network alias %q %v", alias, err)
	}
	var known []string
	for _, p := range profiles {
		if strings.EqualFold(p.Alias, alias) {
			if err := safeValue(p.Alias); err != nil {
				return ConnProfile{}, fmt.Errorf("network alias %q %v", p.Alias, err)
			}
			return p, nil
		}
		known = append(known, quoteArg(p.Alias))
	}
	if len(known) == 0 {
		return ConnProfile{}, fmt.Errorf("no network named %q: Windows reports no connected networks", alias)
	}
	return ConnProfile{}, fmt.Errorf("no network named %q; connected networks: %s", alias, strings.Join(known, ", "))
}

// LinuxAllowCommands returns the commands that let TCP and UDP 7519 in from
// the LAN subnets with the detected firewall. They need root; messh only
// prints them.
func LinuxAllowCommands(t LinuxTarget, subnets []netip.Prefix) []string {
	var out []string
	for _, s := range uniquePrefixes(subnets) {
		for _, proto := range []string{"tcp", "udp"} {
			switch t.Tool {
			case "ufw":
				out = append(out, fmt.Sprintf("sudo ufw allow from %s to any port %d proto %s", s, MeshPort, proto))
			case "firewalld":
				out = append(out, fmt.Sprintf("sudo firewall-cmd --permanent --zone=%s --add-rich-rule='%s'", t.Zone, richRule(s, proto)))
			case "nftables":
				out = append(out, fmt.Sprintf("sudo nft insert rule %s %s %s ip saddr %s %s dport %d accept", t.NftFamily, t.NftTable, t.NftChain, s, proto, MeshPort))
			}
		}
	}
	if t.Tool == "firewalld" && len(out) > 0 {
		out = append(out, "sudo firewall-cmd --reload")
	}
	return out
}

// LinuxRemoveCommands undoes LinuxAllowCommands.
func LinuxRemoveCommands(t LinuxTarget, subnets []netip.Prefix) []string {
	var out []string
	for _, s := range uniquePrefixes(subnets) {
		for _, proto := range []string{"tcp", "udp"} {
			switch t.Tool {
			case "ufw":
				out = append(out, fmt.Sprintf("sudo ufw delete allow from %s to any port %d proto %s", s, MeshPort, proto))
			case "firewalld":
				out = append(out, fmt.Sprintf("sudo firewall-cmd --permanent --zone=%s --remove-rich-rule='%s'", t.Zone, richRule(s, proto)))
			}
		}
	}
	switch {
	case t.Tool == "firewalld" && len(out) > 0:
		out = append(out, "sudo firewall-cmd --reload")
	case t.Tool == "nftables":
		out = append(out,
			fmt.Sprintf("sudo nft -a list chain %s %s %s   (note the handle of each 'dport %d accept' rule)", t.NftFamily, t.NftTable, t.NftChain, MeshPort),
			fmt.Sprintf("sudo nft delete rule %s %s %s handle HANDLE", t.NftFamily, t.NftTable, t.NftChain))
	}
	return out
}

func richRule(s netip.Prefix, proto string) string {
	return fmt.Sprintf(`rule family="ipv4" source address="%s" port port="%d" protocol="%s" accept`, s, MeshPort, proto)
}

func uniquePrefixes(in []netip.Prefix) []netip.Prefix {
	var out []netip.Prefix
	for _, p := range in {
		p = p.Masked()
		if p.IsValid() && p.Addr().Is4() && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	return out
}
