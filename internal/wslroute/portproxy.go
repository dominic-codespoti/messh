// Package wslroute contains shared, host-independent WSL route semantics.
package wslroute

import (
	"net/netip"
	"strconv"
	"strings"
)

// IsGuestIPv4 reports whether s is a private IPv4 address suitable as a WSL
// NAT guest route destination. It deliberately excludes loopback, link-local,
// public, unspecified, multicast, and broadcast addresses.
func IsGuestIPv4(s string) bool {
	address, err := netip.ParseAddr(strings.TrimSpace(s))
	return err == nil && isGuestIPv4(address)
}

func isGuestIPv4(address netip.Addr) bool {
	if !address.Is4() {
		return false
	}
	b := address.As4()
	if b[3] == 0 || b[3] == 255 {
		return false
	}
	return b[0] == 10 || (b[0] == 172 && b[1] >= 16 && b[1] <= 31) || (b[0] == 192 && b[1] == 168)
}

// PortproxyStatus classifies the configured v4tov4 route from netsh output.
// A route is present only when its listen address and both ports match, and
// its connect address exactly equals the currently observed WSL guest address.
// If guestAddress is unresolved, a matching listen row with a plausible
// private destination is unknown rather than present. Missing or known-wrong
// rows are absent; malformed host/port config is unknown.
func PortproxyStatus(text, hostAddress string, meshPort int, guestAddress string) string {
	host, err := netip.ParseAddr(strings.TrimSpace(hostAddress))
	if err != nil || !host.Is4() || host.IsUnspecified() || host.IsLoopback() || host.IsMulticast() || host.As4() == [4]byte{255, 255, 255, 255} || meshPort < 1 || meshPort > 65535 {
		return "unknown"
	}
	wantPort := strconv.Itoa(meshPort)
	guest, guestErr := netip.ParseAddr(strings.TrimSpace(guestAddress))
	guestResolved := guestErr == nil && isGuestIPv4(guest)

	for line := range strings.Lines(text) {
		var fields [4]string
		n := 0
		for field := range strings.FieldsSeq(line) {
			fields[n] = field
			n++
			if n == len(fields) {
				break
			}
		}
		if n < len(fields) || fields[1] != wantPort || fields[3] != wantPort {
			continue
		}
		listen, err := netip.ParseAddr(fields[0])
		if err != nil || !listen.Is4() || listen != host {
			continue
		}
		connect, err := netip.ParseAddr(fields[2])
		if err != nil || !isGuestIPv4(connect) {
			continue
		}
		if !guestResolved {
			return "unknown"
		}
		if connect == guest {
			return "present"
		}
	}
	return "absent"
}
