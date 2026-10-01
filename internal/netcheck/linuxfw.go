package netcheck

import (
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// UFWStatus is `ufw status [verbose]`.
type UFWStatus struct {
	Active          bool
	DefaultIncoming string // "deny", "reject", "allow"; "" when not shown (plain `ufw status`)
	Rules           []UFWRule
}

// UFWRule is one row of the rule table.
type UFWRule struct {
	To     string
	Action string // ALLOW, DENY, REJECT, LIMIT (direction suffix removed)
	From   string
	V6     bool
}

var columns = regexp.MustCompile(`\s{2,}`)

// ParseUFW parses `ufw status` or `ufw status verbose`. ok is false when the
// text is not ufw status output (for example the "need to be root" error).
func ParseUFW(text string) (s UFWStatus, ok bool) {
	inTable := false
	for line := range strings.Lines(text) {
		line = strings.TrimRight(line, "\r\n")
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "Status:"):
			ok = true
			s.Active = strings.TrimSpace(strings.TrimPrefix(trimmed, "Status:")) == "active"
			continue
		case strings.HasPrefix(trimmed, "Default:"):
			// Default: deny (incoming), allow (outgoing), disabled (routed)
			for part := range strings.SplitSeq(strings.TrimPrefix(trimmed, "Default:"), ",") {
				part = strings.TrimSpace(part)
				if pol, ok := strings.CutSuffix(part, "(incoming)"); ok {
					s.DefaultIncoming = strings.TrimSpace(pol)
				}
			}
			continue
		case strings.HasPrefix(trimmed, "--"):
			inTable = true
			continue
		}
		if !inTable || trimmed == "" {
			continue
		}
		if i := strings.Index(trimmed, " # "); i >= 0 {
			trimmed = strings.TrimSpace(trimmed[:i]) // rule comment
		}
		cols := columns.Split(trimmed, -1)
		if len(cols) < 3 {
			continue
		}
		action := strings.Fields(cols[1])
		if len(action) == 0 || (len(action) > 1 && action[1] != "IN") {
			continue // outgoing or routed rule
		}
		r := UFWRule{To: cols[0], Action: action[0], From: cols[2]}
		if strings.HasSuffix(r.To, "(v6)") || strings.HasSuffix(r.From, "(v6)") {
			r.V6 = true
		}
		s.Rules = append(s.Rules, r)
	}
	return s, ok
}

// Allows reports whether IPv4 traffic to proto ("tcp"/"udp") port is let
// in, and by which rule. ufw applies the first matching rule, then the
// default incoming policy.
func (s UFWStatus) Allows(proto string, port int) (string, bool) {
	if !s.Active {
		return "ufw inactive", true
	}
	for _, r := range s.Rules {
		if r.V6 || !ufwToMatches(r.To, proto, port) {
			continue
		}
		desc := r.To + " " + r.Action + " from " + r.From
		return desc, r.Action == "ALLOW" || r.Action == "LIMIT"
	}
	if s.DefaultIncoming == "allow" {
		return "default incoming policy allow", true
	}
	return "", false
}

// ufwToMatches matches the "To" column: "Anywhere", "7519", "7519/tcp",
// "7000:8000/udp", "80,443/tcp", an address or subnet (all ports), or an
// address followed by a port spec, each optionally followed by "on IFACE".
// Application profiles (e.g. "OpenSSH") never match: their ports are unknown.
func ufwToMatches(to, proto string, port int) bool {
	f := strings.Fields(strings.TrimSuffix(strings.TrimSpace(to), "(v6)"))
	if i := slices.Index(f, "on"); i >= 0 {
		f = f[:i]
	}
	if len(f) == 0 {
		return false
	}
	spec := f[len(f)-1]
	if strings.EqualFold(spec, "Anywhere") {
		return true
	}
	if addr, _, _ := strings.Cut(spec, "/"); isAddr(addr) {
		return true // to a local address or subnet, every port
	}
	ports, p, hasProto := strings.Cut(spec, "/")
	if hasProto && p != proto {
		return false
	}
	return portListMatches(ports, ":", port)
}

func isAddr(s string) bool {
	_, err := netip.ParseAddr(s)
	return err == nil
}

// portListMatches matches "a,b,lo<sep>hi"; anything non-numeric fails.
func portListMatches(list, sep string, port int) bool {
	for part := range strings.SplitSeq(list, ",") {
		part = strings.TrimSpace(part)
		lo, hi, isRange := strings.Cut(part, sep)
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

// FirewalldZone is `firewall-cmd --list-all [--zone=Z]`.
type FirewalldZone struct {
	Name       string
	Active     bool
	Target     string
	Interfaces []string
	Sources    []string
	Services   []string
	Ports      []string
	RichRules  []string
}

// ParseFirewalld parses firewall-cmd --list-all; ok is false when the text
// is not a zone listing ("FirewallD is not running", authorization errors).
func ParseFirewalld(text string) (z FirewalldZone, ok bool) {
	inRich := false
	for line := range strings.Lines(text) {
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			// Header: "public (active)", "public (default, active)" or "public".
			name, rest, _ := strings.Cut(line, " ")
			rest = strings.TrimSpace(rest)
			if rest != "" && !(strings.HasPrefix(rest, "(") && strings.HasSuffix(rest, ")")) {
				return z, false // "FirewallD is not running", "Authorization failed."
			}
			z.Name, z.Active, ok = name, strings.Contains(rest, "active"), true
			continue
		}
		if !ok {
			continue
		}
		trimmed := strings.TrimSpace(line)
		key, val, isKey := strings.Cut(trimmed, ":")
		if isKey && !strings.HasPrefix(trimmed, "rule ") {
			inRich = key == "rich rules"
			val = strings.TrimSpace(val)
			switch key {
			case "target":
				z.Target = val
			case "interfaces":
				z.Interfaces = strings.Fields(val)
			case "sources":
				z.Sources = strings.Fields(val)
			case "services":
				z.Services = strings.Fields(val)
			case "ports":
				z.Ports = strings.Fields(val)
			case "rich rules":
				if val != "" {
					z.RichRules = append(z.RichRules, val)
				}
			}
			continue
		}
		if inRich {
			z.RichRules = append(z.RichRules, trimmed)
		}
	}
	return z, ok
}

var richPort = regexp.MustCompile(`port port="?([0-9-]+)"? protocol="?(tcp|udp)"?`)

// Allows reports whether the zone lets proto/port in and why. Custom
// services are not resolved (messh has no predefined firewalld service).
func (z FirewalldZone) Allows(proto string, port int) (string, bool) {
	if strings.EqualFold(z.Target, "ACCEPT") {
		return "zone " + z.Name + " target ACCEPT", true
	}
	for _, p := range z.Ports {
		ports, pr, _ := strings.Cut(p, "/")
		if pr == proto && portListMatches(ports, "-", port) {
			return "zone " + z.Name + " port " + p, true
		}
	}
	for _, r := range z.RichRules {
		if !strings.Contains(r, " accept") || strings.Contains(r, "family=\"ipv6\"") {
			continue
		}
		for _, m := range richPort.FindAllStringSubmatch(r, -1) {
			if m[2] == proto && portListMatches(m[1], "-", port) {
				return "zone " + z.Name + " rich rule", true
			}
		}
	}
	return "", false
}

// NftChain is a base or regular chain of `nft list ruleset`.
type NftChain struct {
	Family, Table, Name string
	Hook                string // "input", "forward", ...; "" for regular chains
	Policy              string // "accept" or "drop"; base chains only
	Rules               []string
}

var (
	nftHook   = regexp.MustCompile(`\bhook\s+(\w+)`)
	nftPolicy = regexp.MustCompile(`\bpolicy\s+(\w+)`)
)

// ParseNft parses the chains of `nft list ruleset`.
func ParseNft(text string) []NftChain {
	var (
		chains        []NftChain
		family, table string
		cur           *NftChain
		depth         int
	)
	for line := range strings.Lines(text) {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, "table ") && strings.HasSuffix(t, "{"):
			f := strings.Fields(t)
			if len(f) >= 3 {
				family, table = f[1], f[2]
			}
			depth = 1
		case strings.HasPrefix(t, "chain ") && strings.HasSuffix(t, "{") && depth == 1:
			f := strings.Fields(t)
			chains = append(chains, NftChain{Family: family, Table: table, Name: f[1]})
			cur = &chains[len(chains)-1]
			depth = 2
		case t == "}":
			if depth == 2 {
				cur = nil
			}
			if depth > 0 {
				depth--
			}
		case strings.HasSuffix(t, "{") && depth >= 1:
			depth++ // set, map or flowtable block
		case cur != nil && depth == 2 && t != "":
			if strings.HasPrefix(t, "type ") {
				if m := nftHook.FindStringSubmatch(t); m != nil {
					cur.Hook = m[1]
					cur.Policy = "accept" // nft's default when no policy is printed
					if p := nftPolicy.FindStringSubmatch(t); p != nil {
						cur.Policy = p[1]
					}
				}
				continue
			}
			cur.Rules = append(cur.Rules, t)
		}
	}
	return chains
}

var nftDport = regexp.MustCompile(`\b(tcp|udp|th)\s+dport\s+(\{[^}]*\}|\S+)`)

// NftAllows reports whether the input base chains let proto/port in: an
// explicit accept rule in every chain that would otherwise drop it (drop
// policy or a catch-all drop/reject rule), and no drop rule for the port.
// Jumps to other chains are not followed.
func NftAllows(chains []NftChain, proto string, port int) (string, bool) {
	inputs := 0
	why := ""
	for _, c := range chains {
		if c.Hook != "input" {
			continue
		}
		inputs++
		if nftDrops(c, proto, port) {
			return "", false
		}
		if c.Policy == "accept" && !slices.ContainsFunc(c.Rules, nftCatchAll) {
			continue
		}
		rule, ok := nftAcceptRule(c, proto, port)
		if !ok {
			return "", false
		}
		why = c.Family + " " + c.Table + " " + c.Name + ": " + rule
	}
	if inputs == 0 {
		return "no input chain", true
	}
	if why == "" {
		why = "input policy accept"
	}
	return why, true
}

// nftCatchAll reports whether a rule drops or rejects everything that reaches
// it, e.g. "counter drop" or "reject with icmpx type port-unreachable".
func nftCatchAll(r string) bool {
	f := strings.Fields(r)
	verdict := false
	for i := 0; i < len(f); i++ {
		switch f[i] {
		case "drop", "reject":
			verdict = true
		case "counter", "with", "icmp", "icmpx", "icmpv6", "tcp", "reset", "type":
		case "packets", "bytes", "prefix", "comment":
			i++ // skip the value
		case "log":
		default:
			if !verdict {
				return false // a match expression before the verdict
			}
		}
	}
	return verdict
}

func nftAcceptRule(c NftChain, proto string, port int) (string, bool) {
	for _, r := range c.Rules {
		f := strings.Fields(r)
		if len(f) == 0 || f[len(f)-1] != "accept" && !strings.Contains(r, " accept ") {
			continue
		}
		if nftRuleCovers(r, proto, port) {
			return r, true
		}
	}
	return "", false
}

// nftDrops spots an explicit drop/reject for the port ahead of an accept policy.
func nftDrops(c NftChain, proto string, port int) bool {
	for _, r := range c.Rules {
		if (strings.HasSuffix(r, " drop") || strings.Contains(r, " reject")) && nftRuleCovers(r, proto, port) {
			return true
		}
	}
	return false
}

func nftRuleCovers(r, proto string, port int) bool {
	if strings.Contains(r, "ip6 ") {
		return false
	}
	for _, m := range nftDport.FindAllStringSubmatch(r, -1) {
		if m[1] != proto && m[1] != "th" {
			continue
		}
		spec := strings.Trim(m[2], "{} ")
		if portListMatches(strings.ReplaceAll(spec, " ", ""), "-", port) {
			return true
		}
	}
	return false
}
