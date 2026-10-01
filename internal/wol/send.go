package wol

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// LimitedBroadcast is the all-ones address every LAN host receives.
var LimitedBroadcast = netip.AddrFrom4([4]byte{255, 255, 255, 255})

// Sender sends magic packets. The zero value sends to ports 9 and 7, three
// times 100 ms apart, from every physical interface's IPv4 address.
type Sender struct {
	Ports   []int                   // nil = 9 and 7 (discard and echo; NICs match the payload on any port)
	Repeat  int                     // rounds; 0 = 3
	Gap     time.Duration           // pause between rounds; 0 = 100 ms
	Locals  func() ([]Local, error) // source addresses; nil = LocalSources
	Limited netip.Addr              // limited-broadcast destination; zero = 255.255.255.255 (tests use loopback)
}

// Report says what Send did, for diagnostics.
type Report struct {
	MACs           []string `json:"macs"`                       // masked
	Sent           int      `json:"packets_sent"`               // datagrams handed to the OS
	Failed         int      `json:"packets_failed,omitempty"`   // datagrams the OS refused
	Routes         []string `json:"routes"`                     // "source -> destination", one per pair, ports 9 and 7 each
	NoSharedSubnet bool     `json:"no_shared_subnet,omitempty"` // no local interface is in any of the target's subnets: only the limited broadcast was used
	Errors         []string `json:"errors,omitempty"`
}

// datagram is one magic packet to send each round, on every port.
type datagram struct {
	payload []byte
	src     Local
	dst     netip.Addr
}

// plan pairs every MAC with every route: the target subnet's directed
// broadcast from each local address in that subnet, and the limited
// broadcast from every local address. With no local address at all the
// limited broadcast goes out unbound (src zero), wherever the OS routes it.
func plan(targets []Target, locals []Local, limited netip.Addr) (out []datagram, noShared bool, err error) {
	noShared = true
	type key struct {
		mac      string
		src, dst netip.Addr
	}
	seen := map[key]bool{}
	add := func(mac string, payload []byte, src Local, dst netip.Addr) {
		k := key{mac, src.Prefix.Addr(), dst}
		if !seen[k] {
			seen[k] = true
			out = append(out, datagram{payload: payload, src: src, dst: dst})
		}
	}
	valid := 0
	for _, t := range targets {
		mac, perr := net.ParseMAC(t.MAC)
		if perr != nil {
			continue
		}
		pkt, perr := MagicPacket(mac)
		if perr != nil {
			continue
		}
		valid++
		for _, s := range t.Subnets {
			if !s.Broadcast.IsValid() {
				continue
			}
			for _, l := range locals {
				if l.Prefix.Addr().Is4() && s.Prefix.IsValid() && l.Prefix.Masked() == s.Prefix.Masked() {
					noShared = false
					add(t.MAC, pkt, l, s.Broadcast)
				}
			}
		}
		if len(locals) == 0 {
			add(t.MAC, pkt, Local{Interface: "default route"}, limited)
		}
		for _, l := range locals {
			add(t.MAC, pkt, l, limited)
		}
	}
	if valid == 0 {
		return nil, noShared, errNoMAC
	}
	return out, noShared, nil
}

// Send wakes the device owning targets. It fails only when no datagram at
// all could be sent; partial failures are listed in the report.
func (s *Sender) Send(ctx context.Context, targets []Target) (Report, error) {
	ports := s.Ports
	if ports == nil {
		ports = []int{9, 7}
	}
	repeat := s.Repeat
	if repeat <= 0 {
		repeat = 3
	}
	gap := s.Gap
	if gap <= 0 {
		gap = 100 * time.Millisecond
	}
	limited := s.Limited
	if !limited.IsValid() {
		limited = LimitedBroadcast
	}
	localsFn := s.Locals
	if localsFn == nil {
		localsFn = LocalSources
	}

	var rep Report
	for _, t := range targets {
		rep.MACs = append(rep.MACs, MaskMAC(t.MAC))
	}
	locals, err := localsFn()
	if err != nil {
		rep.Errors = append(rep.Errors, "list interfaces: "+err.Error())
	}
	dgs, noShared, err := plan(targets, locals, limited)
	if err != nil {
		return rep, err
	}
	rep.NoSharedSubnet = noShared

	// One socket per source address, bound to it so the packet leaves
	// through that NIC on multi-homed hosts.
	conns := map[netip.Addr]net.PacketConn{}
	defer func() {
		for _, c := range conns {
			if c != nil {
				c.Close()
			}
		}
	}()
	errSeen := map[string]bool{}
	note := func(msg string) {
		if !errSeen[msg] && len(rep.Errors) < 8 {
			errSeen[msg] = true
			rep.Errors = append(rep.Errors, msg)
		}
	}
	for _, d := range dgs {
		src := d.src.Prefix.Addr()
		rep.Routes = append(rep.Routes, routeLabel(d))
		if _, ok := conns[src]; ok {
			continue
		}
		bind := "0.0.0.0:0"
		if src.IsValid() {
			bind = netip.AddrPortFrom(src, 0).String()
		}
		lc := net.ListenConfig{Control: func(_, _ string, rc syscall.RawConn) error {
			var serr error
			if err := rc.Control(func(fd uintptr) { serr = setBroadcast(fd) }); err != nil {
				return err
			}
			return serr
		}}
		c, err := lc.ListenPacket(ctx, "udp4", bind)
		if err != nil {
			note(fmt.Sprintf("open socket on %s: %v", bind, err))
			conns[src] = nil
			continue
		}
		conns[src] = c
	}
	rep.Routes = dedupe(rep.Routes)

	for round := range repeat {
		if round > 0 {
			select {
			case <-ctx.Done():
				return rep, finish(rep, ctx.Err())
			case <-time.After(gap):
			}
		}
		for _, d := range dgs {
			c := conns[d.src.Prefix.Addr()]
			for _, port := range ports {
				if c == nil {
					rep.Failed++
					continue
				}
				dst := net.UDPAddrFromAddrPort(netip.AddrPortFrom(d.dst, uint16(port)))
				if err := writeFrom(c, d.payload, d.src.Index, dst); err != nil {
					rep.Failed++
					note(fmt.Sprintf("%s:%d: %v", routeLabel(d), port, err))
					continue
				}
				rep.Sent++
			}
		}
	}
	return rep, finish(rep, nil)
}

func finish(rep Report, err error) error {
	if rep.Sent > 0 {
		return nil
	}
	if err != nil {
		return err
	}
	if len(rep.Errors) > 0 {
		return fmt.Errorf("no magic packet could be sent: %s", rep.Errors[0])
	}
	return fmt.Errorf("no magic packet could be sent")
}

func routeLabel(d datagram) string {
	src := d.src.Interface
	if d.src.Prefix.IsValid() {
		src = d.src.Prefix.Addr().String() + " (" + d.src.Interface + ")"
	}
	return src + " -> " + d.dst.String()
}

func dedupe(ss []string) []string {
	seen := map[string]bool{}
	out := ss[:0]
	for _, s := range ss {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
