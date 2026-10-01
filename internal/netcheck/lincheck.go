package netcheck

import (
	"fmt"
	"strings"
)

// LinuxFirewall is what could be read of the host firewall without root.
type LinuxFirewall struct {
	UFWInstalled bool
	UFWActive    bool       // from `ufw status` or /etc/ufw/ufw.conf
	UFW          *UFWStatus // nil when `ufw status` was unreadable
	UFWNote      string

	FirewalldRunning bool
	Firewalld        []FirewalldZone // zones of the LAN interfaces; nil when unreadable
	FirewalldNote    string

	NftInstalled bool
	NftRead      bool
	Nft          []NftChain
	NftNote      string
}

// LinuxTarget says where `messh firewall allow` should add rules.
type LinuxTarget struct {
	Tool      string // "ufw", "firewalld", "nftables" or "" (no active firewall found)
	Zone      string // firewalld zone
	NftFamily string
	NftTable  string
	NftChain  string
	Known     bool // false when the tool's rules could not be read (nft table/chain are a guess)
}

// Target picks the firewall that filters inbound traffic.
func (f LinuxFirewall) Target() LinuxTarget {
	switch {
	case f.UFWActive:
		return LinuxTarget{Tool: "ufw", Known: true}
	case f.FirewalldRunning:
		t := LinuxTarget{Tool: "firewalld", Zone: "public"}
		if len(f.Firewalld) > 0 {
			t.Zone, t.Known = f.Firewalld[0].Name, true
		}
		return t
	case f.NftRead:
		for _, c := range f.Nft {
			if c.Hook == "input" {
				return LinuxTarget{Tool: "nftables", NftFamily: c.Family, NftTable: c.Table, NftChain: c.Name, Known: true}
			}
		}
		return LinuxTarget{}
	case f.NftInstalled:
		// Unreadable: the conventional table from /etc/nftables.conf.
		return LinuxTarget{Tool: "nftables", NftFamily: "inet", NftTable: "filter", NftChain: "input"}
	}
	return LinuxTarget{}
}

// LinuxChecks reports the LAN interfaces and whether the firewall lets TCP
// and UDP 7519 in.
func LinuxChecks(f LinuxFirewall, ifaces []Iface) []Check {
	var out []Check
	for _, i := range ifaces {
		out = append(out, Check{ID: "iface " + i.Name, Status: OK, Finding: i.Addr.String()})
	}
	if len(ifaces) == 0 {
		out = append(out, Check{ID: "iface", Status: Fail, Finding: "no LAN interface is up with an IPv4 address"})
	}
	t := f.Target()
	fix := LinuxAllowCommands(t, lanPrefixes(ifaces))

	type verdict func(proto string) (string, bool)
	var (
		state  Check
		judge  verdict
		reason string
	)
	state.ID = "firewall"
	switch t.Tool {
	case "ufw":
		if f.UFW == nil {
			state.Status, state.Finding = Unknown, "ufw is active; its rules are unreadable: "+f.UFWNote
			reason = "ufw rules unreadable without root; check with: sudo ufw status verbose"
			break
		}
		state.Status = OK
		state.Finding = "ufw active, default incoming " + orUnknown(f.UFW.DefaultIncoming)
		judge = func(proto string) (string, bool) { return f.UFW.Allows(proto, MeshPort) }
	case "firewalld":
		if len(f.Firewalld) == 0 {
			state.Status, state.Finding = Unknown, "firewalld is running; its zones are unreadable: "+f.FirewalldNote
			reason = "firewalld zone unreadable; check with: sudo firewall-cmd --list-all"
			break
		}
		var zones []string
		for _, z := range f.Firewalld {
			zones = append(zones, z.Name+" (target "+orUnknown(z.Target)+")")
		}
		state.Status, state.Finding = OK, "firewalld running, LAN zone(s) "+strings.Join(zones, ", ")
		judge = func(proto string) (string, bool) {
			var why []string
			for _, z := range f.Firewalld {
				w, ok := z.Allows(proto, MeshPort)
				if !ok {
					return "", false
				}
				why = append(why, w)
			}
			return strings.Join(why, "; "), true
		}
	case "nftables":
		if !f.NftRead {
			state.Status, state.Finding = Unknown, "ufw and firewalld are not active; the nftables ruleset is unreadable: "+f.NftNote
			reason = "nftables ruleset unreadable without root; check with: sudo nft list ruleset"
			break
		}
		state.Status, state.Finding = OK, fmt.Sprintf("nftables input chain %s %s %s", t.NftFamily, t.NftTable, t.NftChain)
		judge = func(proto string) (string, bool) { return NftAllows(f.Nft, proto, MeshPort) }
	default:
		state.Status, state.Finding = OK, "no active firewall (ufw, firewalld and nftables input filtering not found)"
		judge = func(string) (string, bool) { return "no firewall", true }
	}
	out = append(out, state)

	for _, p := range []struct{ id, proto, what string }{
		{"firewall TCP 7519", "tcp", "peer connections"},
		{"firewall UDP 7519", "udp", "discovery"},
	} {
		c := Check{ID: p.id}
		switch {
		case judge == nil:
			c.Status, c.Finding = Unknown, reason
			c.Fix = fix
		default:
			if why, ok := judge(p.proto); ok {
				c.Status, c.Finding = OK, "allowed: "+why
			} else {
				c.Status, c.Finding = Fail, fmt.Sprintf("%s (%s %d) not allowed by %s", p.what, p.proto, MeshPort, t.Tool)
				c.Fix = fix
			}
		}
		out = append(out, c)
	}
	return out
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
