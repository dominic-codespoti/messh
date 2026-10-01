//go:build linux

package discovery

import "golang.org/x/sys/unix"

func setMulticastAllOff(fd int) error {
	return unix.SetsockoptInt(fd, unix.IPPROTO_IP, unix.IP_MULTICAST_ALL, 0)
}
