//go:build unix

package discovery

import (
	"net/netip"
	"syscall"

	"golang.org/x/sys/unix"
)

// recvBindAddr: on Unix, binding to the group address restricts the socket to
// datagrams addressed to that group (binding to the interface address would
// filter multicast out entirely).
func recvBindAddr(group netip.AddrPort, _ netip.Addr) netip.AddrPort {
	return group
}

// controlReuse sets SO_REUSEADDR and SO_REUSEPORT so several processes can
// share the group port; multicast datagrams are delivered to every such
// socket. On Linux it also clears IP_MULTICAST_ALL so the socket only receives
// groups (per interface) it joined itself.
func controlReuse(_, _ string, c syscall.RawConn) error {
	var serr error
	if err := c.Control(func(fd uintptr) {
		s := int(fd)
		if serr = unix.SetsockoptInt(s, unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); serr != nil {
			return
		}
		if serr = unix.SetsockoptInt(s, unix.SOL_SOCKET, unix.SO_REUSEPORT, 1); serr != nil {
			return
		}
		serr = setMulticastAllOff(s)
	}); err != nil {
		return err
	}
	return serr
}
