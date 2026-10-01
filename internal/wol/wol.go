// Package wol wakes sleeping devices with Wake-on-LAN magic packets, lists
// the interfaces of this device that another one could wake, and reports when
// this device is about to sleep.
package wol

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
)

// Subnet is an IPv4 network an interface is on, with its directed broadcast address.
type Subnet struct {
	Prefix    netip.Prefix `json:"prefix"`    // interface address and mask, e.g. 192.168.1.20/24
	Broadcast netip.Addr   `json:"broadcast"` // e.g. 192.168.1.255
}

// Target is one wakeable network interface of a device.
type Target struct {
	MAC       string   `json:"mac"` // aa:bb:cc:dd:ee:ff
	Interface string   `json:"interface,omitempty"`
	Subnets   []Subnet `json:"subnets,omitempty"`
}

// Limits on what a peer may hand us; real devices have a handful of NICs.
const (
	maxTargets = 8
	maxSubnets = 4
)

// MagicPacket returns the 102-byte payload that wakes the NIC with mac:
// six 0xFF bytes followed by the MAC sixteen times.
func MagicPacket(mac net.HardwareAddr) ([]byte, error) {
	if len(mac) != 6 {
		return nil, fmt.Errorf("wake-on-lan needs a 6-byte MAC address, got %d bytes", len(mac))
	}
	b := make([]byte, 6, 102)
	for i := range b {
		b[i] = 0xff
	}
	for range 16 {
		b = append(b, mac...)
	}
	return b, nil
}

// BroadcastAddr returns the directed broadcast address of an IPv4 prefix.
// Host routes (/31, /32) and non-IPv4 prefixes have none.
func BroadcastAddr(p netip.Prefix) (netip.Addr, bool) {
	if !p.IsValid() || !p.Addr().Is4() || p.Bits() < 1 || p.Bits() > 30 {
		return netip.Addr{}, false
	}
	a := p.Masked().Addr().As4()
	host := ^uint32(0) >> p.Bits()
	v := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	v |= host
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}), true
}

// MaskMAC hides the middle of a MAC for logs and agent output, keeping the
// vendor prefix and the last byte so NICs stay distinguishable.
func MaskMAC(mac string) string {
	parts := strings.Split(mac, ":")
	if len(parts) != 6 {
		return "invalid"
	}
	return strings.Join([]string{parts[0], parts[1], parts[2], "xx", "xx", parts[5]}, ":")
}

// Sanitize validates targets received from a peer: MACs must be 6-byte
// unicast addresses, subnets IPv4 with a broadcast address, which is
// recomputed from the prefix rather than trusted. Counts are capped.
func Sanitize(ts []Target) []Target {
	var out []Target
	for _, t := range ts {
		if len(out) == maxTargets {
			break
		}
		mac, err := net.ParseMAC(t.MAC)
		if err != nil || len(mac) != 6 || mac[0]&1 != 0 || allZero(mac) {
			continue
		}
		c := Target{MAC: mac.String(), Interface: truncate(t.Interface, 64)}
		for _, s := range t.Subnets {
			if len(c.Subnets) == maxSubnets {
				break
			}
			if b, ok := BroadcastAddr(s.Prefix); ok {
				c.Subnets = append(c.Subnets, Subnet{Prefix: s.Prefix, Broadcast: b})
			}
		}
		if !slices.ContainsFunc(out, func(o Target) bool { return o.MAC == c.MAC }) {
			out = append(out, c)
		}
	}
	return out
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Iface is a network interface as the filter sees it.
type Iface struct {
	Name    string
	Index   int
	Desc    string // adapter description (Windows); empty elsewhere
	Flags   net.Flags
	MAC     net.HardwareAddr
	Addrs   []netip.Prefix
	Virtual bool // the OS reports a software device (Linux /sys/devices/virtual; Windows non-Ethernet, non-Wi-Fi types)
}

// virtualPrefixes are interface names of software devices: Hyper-V/WSL,
// containers, bridges, VMs, VPNs and tunnels. Matched case-insensitively.
var virtualPrefixes = []string{
	"vethernet", "docker", "br-", "bridge", "veth", "virbr", "vmnet", "vboxnet", "tun", "tap", "wg",
	"tailscale", "zt", "utun", "lxcbr", "lxdbr", "cni", "flannel", "cali", "podman", "ppp", "nordlynx",
	"bluetooth", "local area connection*",
}

// virtualWords appear in names or Windows adapter descriptions of software devices.
var virtualWords = []string{
	"virtual", "hyper-v", "vmware", "virtualbox", "vpn", "tap-windows", "wireguard", "wintun", "tunnel",
	"tailscale", "zerotier", "openvpn", "wan miniport", "teredo", "isatap", "loopback", "bluetooth",
	"wi-fi direct", "anyconnect", "fortinet",
}

// IsVirtual reports whether ifi looks like a software interface that no
// magic packet could wake the machine through.
func IsVirtual(ifi Iface) bool {
	if ifi.Virtual {
		return true
	}
	name, desc := strings.ToLower(ifi.Name), strings.ToLower(ifi.Desc)
	for _, p := range virtualPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	for _, w := range virtualWords {
		if strings.Contains(name, w) || strings.Contains(desc, w) {
			return true
		}
	}
	return false
}

// wakeableIface reports whether ifi is an up, physical, non-loopback
// interface with an Ethernet-style hardware address.
func wakeableIface(ifi Iface) bool {
	return ifi.Flags&net.FlagUp != 0 && ifi.Flags&net.FlagLoopback == 0 &&
		len(ifi.MAC) == 6 && !allZero(ifi.MAC) && !IsVirtual(ifi)
}

// Wakeable picks the interfaces another device could wake this one through,
// with their IPv4 subnets. An interface without IPv4 (say, an Ethernet port
// enslaved to a bridge) is kept: 255.255.255.255 still reaches it.
func Wakeable(ifs []Iface) []Target {
	var out []Target
	for _, ifi := range ifs {
		if !wakeableIface(ifi) {
			continue
		}
		t := Target{MAC: ifi.MAC.String(), Interface: ifi.Name}
		for _, p := range ifi.Addrs {
			if b, ok := BroadcastAddr(p); ok {
				t.Subnets = append(t.Subnets, Subnet{Prefix: p, Broadcast: b})
			}
		}
		out = append(out, t)
	}
	return out
}

// Local is an IPv4 address of this device that magic packets are sent from.
type Local struct {
	Interface string
	Index     int
	Prefix    netip.Prefix
}

// sources picks the broadcast-capable physical interfaces' IPv4 addresses.
func sources(ifs []Iface) []Local {
	var out []Local
	for _, ifi := range ifs {
		if !wakeableIface(ifi) || ifi.Flags&net.FlagBroadcast == 0 {
			continue
		}
		for _, p := range ifi.Addrs {
			if p.Addr().Is4() {
				out = append(out, Local{Interface: ifi.Name, Index: ifi.Index, Prefix: p})
			}
		}
	}
	return out
}

// LocalInterfaces lists this device's interfaces with their IPv4 prefixes,
// annotated by the platform (adapter type/description).
func LocalInterfaces() ([]Iface, error) {
	all, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	out := make([]Iface, 0, len(all))
	for _, ni := range all {
		ifi := Iface{Name: ni.Name, Index: ni.Index, Flags: ni.Flags, MAC: ni.HardwareAddr}
		addrs, _ := ni.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(ipn.IP)
			if !ok {
				continue
			}
			ip = ip.Unmap()
			ones, _ := ipn.Mask.Size()
			if ip.Is4() && ones > 0 && ones <= 32 {
				ifi.Addrs = append(ifi.Addrs, netip.PrefixFrom(ip, ones))
			}
		}
		out = append(out, ifi)
	}
	annotate(out)
	return out, nil
}

// LocalTargets lists this device's wakeable interfaces, for peers to store.
func LocalTargets() ([]Target, error) {
	ifs, err := LocalInterfaces()
	if err != nil {
		return nil, err
	}
	return Wakeable(ifs), nil
}

// LocalSources lists the addresses this device sends magic packets from.
func LocalSources() ([]Local, error) {
	ifs, err := LocalInterfaces()
	if err != nil {
		return nil, err
	}
	return sources(ifs), nil
}

var errNoMAC = errors.New("no valid MAC address to wake")
