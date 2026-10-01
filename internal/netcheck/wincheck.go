package netcheck

import (
	"fmt"
	"slices"
	"strings"
)

// AnnotateCategories copies each interface's Windows network category from
// the connection profiles (matched by interface index, then alias).
func AnnotateCategories(ifaces []Iface, profiles []ConnProfile) []Iface {
	out := slices.Clone(ifaces)
	for i := range out {
		for _, p := range profiles {
			if p.Index == out[i].Index || p.Index == 0 && strings.EqualFold(p.Alias, out[i].Name) {
				out[i].Category, out[i].Profiled = p.Category, true
				break
			}
		}
	}
	return out
}

// privateFix is the command that makes alias Private and opens the ports.
func privateFix(alias string) string {
	return "messh firewall allow --private " + quoteArg(alias)
}

// WindowsChecks evaluates the network categories and Windows Firewall for
// the node's executable exe on the LAN interfaces and the ports it uses.
func WindowsChecks(fw WinFirewall, ifaces []Iface, exe string, ports Ports) []Check {
	tcpID := fmt.Sprintf("firewall TCP %d", ports.TCP)
	udpID := fmt.Sprintf("firewall UDP %d", ports.UDP)
	if fw.ProfilesKnown {
		ifaces = AnnotateCategories(ifaces, fw.Profiles)
	}
	var out []Check
	var publicAliases []string
	active := Profile(0)
	for _, i := range ifaces {
		c := Check{ID: "iface " + i.Name}
		base := i.Addr.String()
		switch {
		case !fw.ProfilesKnown:
			c.Status, c.Finding = Unknown, base+", network category unknown ("+strings.Join(fw.Notes, "; ")+")"
		case !i.Profiled:
			c.Status, c.Finding = OK, base+", no Windows network profile (virtual or unidentified adapter)"
		case ProfileForCategory(i.Category) == ProfilePublic:
			c.Status = Warn
			c.Finding = base + ", network category Public: Windows treats it as untrusted and messh's rules apply to Private networks"
			c.Fix = []string{privateFix(i.Name)}
			publicAliases = append(publicAliases, i.Name)
		default:
			c.Status, c.Finding = OK, base+", network category "+i.Category
		}
		if i.Profiled {
			active |= ProfileForCategory(i.Category)
		}
		out = append(out, c)
	}
	if len(ifaces) == 0 {
		out = append(out, Check{ID: "iface", Status: Fail, Finding: "no LAN interface is up with an IPv4 address"})
	}
	if active == 0 {
		active = fw.Current
	}

	allowFix := []string{"messh firewall allow"}
	if len(publicAliases) > 0 {
		allowFix = nil
		for _, a := range publicAliases {
			allowFix = append(allowFix, privateFix(a))
		}
	}

	if !fw.RulesKnown {
		why := strings.Join(fw.Notes, "; ")
		out = append(out,
			Check{ID: "firewall", Status: Unknown, Finding: "Windows Firewall state unknown: " + why},
			Check{ID: tcpID, Status: Unknown, Finding: "rules unreadable: " + why},
			Check{ID: udpID, Status: Unknown, Finding: "rules unreadable: " + why},
		)
		return out
	}
	if active == 0 {
		out = append(out, Check{ID: "firewall", Status: Unknown, Finding: "cannot tell which firewall profile applies to the LAN"})
		return out
	}

	// Profile state.
	fc := Check{ID: "firewall", Status: OK}
	var states []string
	var enforced []Profile
	for _, p := range []Profile{ProfileDomain, ProfilePrivate, ProfilePublic} {
		if active&p == 0 {
			continue
		}
		st, known := profileState(fw.FirewallProfiles, p)
		switch {
		case !known:
			states = append(states, p.String()+": state unknown")
			enforced = append(enforced, p)
		case !st.Enabled:
			states = append(states, p.String()+": firewall off")
		case st.BlockAllInbound:
			states = append(states, p.String()+": blocks ALL incoming connections, even allowed apps")
			fc.Status = Fail
			fc.Fix = append(fc.Fix, fmt.Sprintf("netsh advfirewall set %sprofile firewallpolicy blockinbound,allowoutbound   (elevated; or untick \"Blocks all incoming connections\" in Windows Security > Firewall > %s network)",
				strings.ToLower(p.String()), p.String()))
		case st.DefaultAllow:
			states = append(states, p.String()+": on, inbound allowed by default")
		default:
			states = append(states, p.String()+": on, inbound blocked unless a rule allows it")
			enforced = append(enforced, p)
		}
	}
	fc.Finding = "active profile(s) " + strings.Join(states, "; ") + " (rules read via " + fw.Source + ")"
	out = append(out, fc)

	lans := lanPrefixes(ifaces)
	for _, t := range []struct {
		id    string
		proto int
		port  int
		what  string
	}{{tcpID, ProtoTCP, ports.TCP, "peer connections"}, {udpID, ProtoUDP, ports.UDP, "discovery"}} {
		c := Check{ID: t.id, Status: OK}
		var parts []string
		for _, p := range enforced {
			v := Evaluate(fw.Rules, exe, t.proto, t.port, p, lans)
			switch {
			case len(v.Blocked) > 0:
				c.Status = Fail
				parts = append(parts, fmt.Sprintf("%s: blocked by rule %q", p, v.Blocked[0].Name))
				c.Fix = allowFix
			case len(v.Allowed) > 0:
				parts = append(parts, fmt.Sprintf("%s: allowed by %s", p, describeRule(v.Allowed[0])))
			case len(v.Unsure) > 0:
				if c.Status == OK {
					c.Status = Warn
				}
				parts = append(parts, fmt.Sprintf("%s: only %s may allow %s (%s %d), but %s cannot show whether it is limited to an app package or specific users",
					p, describeRule(v.Unsure[0]), t.what, protoName(t.proto), t.port, fw.Source))
				c.Fix = allowFix
			default:
				c.Status = Fail
				parts = append(parts, fmt.Sprintf("%s: no rule allows %s (%s %d)", p, t.what, protoName(t.proto), t.port))
				c.Fix = allowFix
			}
		}
		if len(enforced) == 0 {
			parts = append(parts, "no active profile filters inbound traffic")
		}
		c.Finding = strings.Join(parts, "; ")
		out = append(out, c)
	}

	// Block rules for the program itself, on any profile.
	bc := Check{ID: "firewall block rules", Status: OK, Finding: "no block rule for " + exeName(exe)}
	if blocks := ProgramBlockRules(fw.Rules, exe); len(blocks) > 0 {
		var names []string
		var hitsActive bool
		for _, b := range blocks {
			names = append(names, fmt.Sprintf("%q (%s, %s)", b.Name, protoName(b.Protocol), b.Profiles))
			if b.Profiles&active != 0 {
				hitsActive = true
			}
		}
		bc.Status = Warn
		bc.Finding = "block rules for " + exeName(exe) + ", created when the firewall prompt was dismissed: " + strings.Join(names, ", ")
		if hitsActive {
			bc.Status = Fail
		}
		bc.Fix = allowFix
	}
	if exe == "" {
		bc.Status, bc.Finding = Unknown, "path of the messh executable unknown"
	}
	out = append(out, bc)
	return out
}

func profileState(list []FirewallProfile, p Profile) (FirewallProfile, bool) {
	for _, s := range list {
		if s.Type == p {
			return s, true
		}
	}
	return FirewallProfile{}, false
}

func describeRule(r Rule) string {
	s := fmt.Sprintf("%q", r.Name)
	var scope []string
	if r.Program == "" {
		scope = append(scope, "any program")
	}
	if r.RemoteAddrs != "" && r.RemoteAddrs != "*" && !strings.EqualFold(r.RemoteAddrs, "Any") {
		scope = append(scope, "remote "+r.RemoteAddrs)
	}
	if len(scope) > 0 {
		s += " (" + strings.Join(scope, ", ") + ")"
	}
	return s
}

func exeName(exe string) string {
	if exe == "" {
		return "messh"
	}
	return exe
}
