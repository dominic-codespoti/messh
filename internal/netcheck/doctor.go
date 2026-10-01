package netcheck

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"messh/internal/discovery"
	"messh/internal/state"
)

// Run performs every check for the node owning paths. It changes nothing:
// no files are written, no rules or settings touched, the multicast test
// socket is bound to loopback and sends nothing.
func Run(ctx context.Context, paths state.Paths) Report {
	host, _ := os.Hostname()
	r := Report{Host: host, OS: runtime.GOOS}
	node := LoadNode(ctx, paths)
	ifaces, ifErr := LANInterfaces()
	r.Program = ExecutablePath(node)

	r.Checks = append(r.Checks, NodeChecks(node, lanAddrs(ifaces))...)
	if ifErr != nil {
		r.Checks = append(r.Checks, Check{ID: "iface", Status: Unknown, Finding: "cannot list network interfaces: " + ifErr.Error()})
	}
	r.Checks = append(r.Checks, osChecks(ctx, ifaces, r.Program, FirewallPorts(node))...)
	r.Checks = append(r.Checks, MulticastChecks(ifaces, JoinMulticast)...)
	r.Checks = append(r.Checks, MeshChecks(node, time.Now())...)
	return r
}

// Ports are the inbound ports the firewall checks test.
type Ports struct {
	TCP int // mesh listener
	UDP int // discovery
}

// FirewallPorts returns the running node's mesh port (MeshPort when no node
// runs or its address is unreadable) and the fixed discovery port.
func FirewallPorts(n Node) Ports {
	p := Ports{TCP: MeshPort, UDP: int(discovery.DefaultGroup.Port())}
	if !n.Running {
		return p
	}
	if _, s, err := net.SplitHostPort(n.Status.Mesh); err == nil {
		if port, err := strconv.Atoi(s); err == nil && port > 0 && port < 65536 {
			p.TCP = port
		}
	}
	return p
}

// ExecutablePath is the program the firewall must admit: the running node's
// executable when it can be read, otherwise this one.
func ExecutablePath(n Node) string {
	if n.Running && n.PID > 0 {
		if p, err := processPath(n.PID); err == nil && p != "" {
			return p
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe
}
