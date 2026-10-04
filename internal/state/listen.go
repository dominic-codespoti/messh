package state

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// ValidateListenAddress reports whether addr is usable as the mesh listen
// address. Empty means the built-in default; otherwise addr must be
// host:port with a numeric port (0-65535, including 0 for an ephemeral
// port). An empty host (":port") and any IP literal are accepted; DNS names
// are accepted when syntactically valid and left for the listener to resolve.
func ValidateListenAddress(addr string) error {
	if addr == "" {
		return nil
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("listen address %q: %w", addr, err)
	}
	if err := checkNumericPort(portStr, addr, "listen"); err != nil {
		return err
	}
	if host == "" {
		return nil
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return nil
	}
	if !validHostname(host) {
		return fmt.Errorf("listen address %q has an invalid host", addr)
	}
	return nil
}

// ValidateLocalAddress reports whether addr is usable as the loopback-only
// local address. Empty means the built-in default; otherwise addr must be
// host:port with a numeric port (0-65535, including 0) and a literal
// loopback IP host. Hostnames such as localhost and DNS names are rejected:
// the local endpoint serves without TLS, so only a literal loopback IP is
// safe. An empty host is rejected: ":port" would bind all interfaces.
func ValidateLocalAddress(addr string) error {
	if addr == "" {
		return nil
	}
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("local address %q: %w", addr, err)
	}
	if err := checkNumericPort(portStr, addr, "local"); err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil || !ip.IsLoopback() {
		return fmt.Errorf("local address %q must be a loopback IP; agents and the CLI connect there without TLS", addr)
	}
	return nil
}

func checkNumericPort(portStr, addr, kind string) error {
	if portStr == "" {
		return fmt.Errorf("%s address %q needs a numeric port", kind, addr)
	}
	for i := range len(portStr) {
		if portStr[i] < '0' || portStr[i] > '9' {
			return fmt.Errorf("%s address %q needs a numeric port 0-65535", kind, addr)
		}
	}
	n, err := strconv.Atoi(portStr)
	if err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("%s address %q needs a numeric port 0-65535", kind, addr)
	}
	return nil
}

// validHostname reports whether host is syntactically a DNS name: dot
// separated labels of letters, digits and hyphens. It exists so obvious
// garbage fails validation before publication instead of surfacing later as
// a listen error after identity state was created.
func validHostname(host string) bool {
	host = strings.TrimSuffix(host, ".")
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	for label := range strings.SplitSeq(host, ".") {
		if len(label) == 0 || len(label) > 63 {
			return false
		}
		for i := range len(label) {
			c := label[i]
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' {
				return false
			}
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
	}
	return true
}

