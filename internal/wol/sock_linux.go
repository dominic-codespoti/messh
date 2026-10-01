//go:build linux

package wol

import (
	"net"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/net/ipv4"
)

func setBroadcast(fd uintptr) error {
	return syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
}

// writeFrom sends b to dst out of interface ifIndex. Linux routes the
// limited broadcast by the routing table regardless of the bound address,
// so IP_PKTINFO pins the egress interface (no privileges needed, unlike
// SO_BINDTODEVICE on older kernels).
func writeFrom(c net.PacketConn, b []byte, ifIndex int, dst *net.UDPAddr) error {
	if ifIndex == 0 {
		_, err := c.WriteTo(b, dst)
		return err
	}
	_, err := ipv4.NewPacketConn(c).WriteTo(b, &ipv4.ControlMessage{IfIndex: ifIndex}, dst)
	return err
}

// annotate marks software interfaces: the kernel places every virtual
// device (bridges, veth, tun/tap, wireguard, docker) under /sys/devices/virtual.
func annotate(ifs []Iface) {
	for i := range ifs {
		if _, err := os.Stat(filepath.Join("/sys/devices/virtual/net", ifs[i].Name)); err == nil {
			ifs[i].Virtual = true
		}
	}
}
