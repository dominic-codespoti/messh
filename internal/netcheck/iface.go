package netcheck

import (
	"fmt"
	"net"
	"net/netip"

	"golang.org/x/net/ipv4"
)

// Iface is a LAN interface discovery would use: up, multicast-capable, not
// loopback, with an IPv4 address.
type Iface struct {
	Name     string
	Index    int
	Addr     netip.Prefix // address with its prefix length, e.g. 192.168.1.25/24
	Category string       // Windows network category; "" = unknown or no connection profile
	Profiled bool         // Windows has a connection profile for it
}

// Subnet is the network the interface is on, e.g. 192.168.1.0/24.
func (i Iface) Subnet() netip.Prefix { return i.Addr.Masked() }

// LANInterfaces lists the interfaces discovery would join on, using the same
// rule (and the same preference for a non-link-local address) as the node.
func LANInterfaces() ([]Iface, error) {
	all, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	var out []Iface
	for _, ifi := range all {
		if ifi.Flags&net.FlagUp == 0 || ifi.Flags&net.FlagMulticast == 0 || ifi.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		var best, fallback netip.Prefix
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipn.IP)
			if !ok || !ip.Unmap().Is4() {
				continue
			}
			ones, _ := ipn.Mask.Size()
			if len(ipn.Mask) == net.IPv6len {
				ones -= 96
			}
			p := netip.PrefixFrom(ip.Unmap(), ones)
			if !ip.Unmap().IsLinkLocalUnicast() {
				best = p
				break
			}
			if !fallback.IsValid() {
				fallback = p
			}
		}
		if !best.IsValid() {
			best = fallback
		}
		if best.IsValid() {
			out = append(out, Iface{Name: ifi.Name, Index: ifi.Index, Addr: best})
		}
	}
	return out, nil
}

// JoinMulticast checks that this host can join the discovery group on the
// interface. The throwaway socket is bound to loopback with an ephemeral
// port, so it never triggers a firewall prompt and sends nothing; the
// membership ends when it is closed.
func JoinMulticast(ifaceIndex int) error {
	ifi, err := net.InterfaceByIndex(ifaceIndex)
	if err != nil {
		return err
	}
	c, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("open test socket: %w", err)
	}
	defer c.Close()
	return ipv4.NewPacketConn(c).JoinGroup(ifi, &net.UDPAddr{IP: net.IP(DiscoveryGroup.AsSlice())})
}

// MulticastChecks joins the discovery group on every LAN interface.
func MulticastChecks(ifaces []Iface, join func(index int) error) []Check {
	var out []Check
	for _, i := range ifaces {
		c := Check{ID: "multicast " + i.Name}
		if err := join(i.Index); err != nil {
			c.Status, c.Finding = Fail, fmt.Sprintf("cannot join %s: %v", DiscoveryGroup, err)
		} else {
			c.Status, c.Finding = OK, fmt.Sprintf("joined %s on %s", DiscoveryGroup, i.Addr.Addr())
		}
		out = append(out, c)
	}
	return out
}

// lanPrefixes returns the subnets of ifaces.
func lanPrefixes(ifaces []Iface) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(ifaces))
	for _, i := range ifaces {
		out = append(out, i.Subnet())
	}
	return out
}

// lanAddrs returns the addresses of ifaces.
func lanAddrs(ifaces []Iface) []netip.Addr {
	out := make([]netip.Addr, 0, len(ifaces))
	for _, i := range ifaces {
		out = append(out, i.Addr.Addr())
	}
	return out
}
