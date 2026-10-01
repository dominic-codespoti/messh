//go:build windows

package discovery

import (
	"net/netip"
	"syscall"
)

// recvBindAddr: Windows cannot bind to a multicast address, and binding to
// INADDR_ANY would expose the socket on every interface (and trigger a firewall
// prompt). Binding to the interface address receives multicast for groups
// joined on that interface.
func recvBindAddr(group netip.AddrPort, ifaceIP netip.Addr) netip.AddrPort {
	return netip.AddrPortFrom(ifaceIP, group.Port())
}

// controlReuse sets SO_REUSEADDR so several processes can share the group port.
// On Windows every socket bound with SO_REUSEADDR receives a copy of each
// multicast datagram.
func controlReuse(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		serr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1)
	}); err != nil {
		return err
	}
	return serr
}
