//go:build windows

package wol

import (
	"net"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func setBroadcast(fd uintptr) error {
	return syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_BROADCAST, 1)
}

// writeFrom sends b to dst. Windows sends even the limited broadcast out of
// the interface owning the socket's bound address, so ifIndex is not needed.
func writeFrom(c net.PacketConn, b []byte, _ int, dst *net.UDPAddr) error {
	_, err := c.WriteTo(b, dst)
	return err
}

// annotate adds adapter descriptions and marks every adapter that is
// neither Ethernet nor Wi-Fi (tunnels, PPP, loopback) virtual. Hyper-V and
// VPN adapters pose as Ethernet; their names and descriptions catch them.
func annotate(ifs []Iface) {
	size := uint32(16 << 10)
	var buf []byte
	for range 3 {
		buf = make([]byte, size)
		err := windows.GetAdaptersAddresses(syscall.AF_UNSPEC,
			windows.GAA_FLAG_SKIP_ANYCAST|windows.GAA_FLAG_SKIP_MULTICAST|windows.GAA_FLAG_SKIP_DNS_SERVER,
			0, (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0])), &size)
		if err == nil {
			break
		}
		if err != windows.ERROR_BUFFER_OVERFLOW {
			return
		}
		buf = nil
	}
	if buf == nil {
		return
	}
	type info struct {
		desc   string
		ifType uint32
	}
	byIndex := map[int]info{}
	for aa := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buf[0])); aa != nil; aa = aa.Next {
		index := int(aa.IfIndex)
		if index == 0 {
			index = int(aa.Ipv6IfIndex) // net.Interfaces uses the same fallback
		}
		byIndex[index] = info{desc: windows.UTF16PtrToString(aa.Description), ifType: aa.IfType}
	}
	for i := range ifs {
		in, ok := byIndex[ifs[i].Index]
		if !ok {
			continue
		}
		ifs[i].Desc = in.desc
		if in.ifType != windows.IF_TYPE_ETHERNET_CSMACD && in.ifType != windows.IF_TYPE_IEEE80211 {
			ifs[i].Virtual = true
		}
	}
}
