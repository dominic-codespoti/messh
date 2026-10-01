package netcheck

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"messh/internal/control"
	"messh/internal/identity"
	"messh/internal/state"
)

// Node is what the running node reports through its control API.
type Node struct {
	Running  bool
	Problem  string // why it is not running or not answering
	PID      int
	Status   control.Status
	Peers    []control.PeerStatus
	PeersErr error
	Seen     []control.Sighting
	SeenErr  error
}

// LoadNode asks the node owning paths for its status, peers and sightings.
// It never creates files: the control token is read, not generated.
func LoadNode(ctx context.Context, paths state.Paths) Node {
	var n Node
	run, err := paths.LoadRunInfo()
	if err != nil {
		n.Problem = "is not running (no run.json in " + paths.Root + ")"
		if !errors.Is(err, fs.ErrNotExist) {
			n.Problem = "state unreadable: run.json: " + err.Error()
		}
		return n
	}
	n.PID = run.PID
	tok, err := os.ReadFile(paths.ControlTokenFile())
	if err != nil {
		n.Problem = "state unreadable: control token: " + err.Error()
		return n
	}
	c := &control.Client{Base: "http://" + run.Local, Token: strings.TrimSpace(string(tok)), HTTP: &http.Client{Timeout: 3 * time.Second}}
	if err := c.Do(ctx, http.MethodGet, "/v1/status", nil, &n.Status); err != nil {
		n.Problem = fmt.Sprintf("is not answering: run.json names %s (pid %d) but %v", run.Local, run.PID, err)
		return n
	}
	n.Running = true
	n.PeersErr = c.Do(ctx, http.MethodGet, "/v1/peers", nil, &n.Peers)
	n.SeenErr = c.Do(ctx, http.MethodGet, "/v1/discovered", nil, &n.Seen)
	return n
}

// heardWindow is how recent an announcement must be to count as "being
// received"; nodes announce every 30 s.
const heardWindow = 2 * time.Minute

// NodeChecks reports the node and its mesh listen address. lanIPs are this
// device's LAN addresses.
func NodeChecks(n Node, lanIPs []netip.Addr) []Check {
	if !n.Running {
		return []Check{{ID: "node", Status: Fail, Finding: "messh node " + n.Problem + "; node checks skipped", Fix: []string{"messh node"}}}
	}
	st := n.Status
	return []Check{
		{ID: "node", Status: OK, Finding: fmt.Sprintf("%s (%s) %s running since %s, mesh %s, agents http://%s/mcp",
			st.Name, identity.Short(st.ID), st.Version, st.Started.Format(time.DateTime), st.Mesh, st.Local)},
		meshListenCheck(st.Mesh, lanIPs),
	}
}

// MeshChecks reports whether discovery announcements arrive and every paired
// peer's reachability; nothing when the node is not running.
func MeshChecks(n Node, now time.Time) []Check {
	if !n.Running {
		return nil
	}
	return append([]Check{discoveryCheck(n, now)}, peerChecks(n, now)...)
}

func meshListenCheck(mesh string, lanIPs []netip.Addr) Check {
	c := Check{ID: "mesh listen"}
	host, portStr, err := net.SplitHostPort(mesh)
	if err != nil {
		c.Status, c.Finding = Unknown, "cannot parse mesh address "+mesh
		return c
	}
	port, _ := strconv.Atoi(portStr)
	addr, err := netip.ParseAddr(host)
	switch {
	case host == "" || err == nil && addr.IsUnspecified():
		c.Status, c.Finding = OK, fmt.Sprintf("TCP %d on all addresses (%s)", port, mesh)
	case err == nil && addr.IsLoopback():
		c.Status, c.Finding = Fail, fmt.Sprintf("TCP %d bound to loopback only (%s): peers cannot connect", port, mesh)
		c.Fix = []string{"stop the node, then: messh node --listen :7519"}
		return c
	case err == nil && slices.Contains(lanIPs, addr.Unmap()):
		c.Status, c.Finding = OK, fmt.Sprintf("TCP %d on LAN address %s only", port, addr)
	default:
		c.Status, c.Finding = Warn, fmt.Sprintf("TCP %d bound to %s, which is not one of this device's LAN addresses", port, host)
		c.Fix = []string{"stop the node, then: messh node --listen :7519"}
		return c
	}
	if port != MeshPort {
		c.Status = Warn
		c.Finding += fmt.Sprintf("; `messh firewall allow` only opens %d", MeshPort)
	}
	return c
}

func discoveryCheck(n Node, now time.Time) Check {
	c := Check{ID: "discovery"}
	if n.SeenErr != nil {
		c.Status, c.Finding = Unknown, "cannot list sightings: "+n.SeenErr.Error()
		return c
	}
	var recent []string
	var last time.Time
	for _, s := range n.Seen {
		if s.ID == n.Status.ID {
			continue
		}
		if s.Seen.After(last) {
			last = s.Seen
		}
		if now.Sub(s.Seen) <= heardWindow {
			recent = append(recent, s.Name+" ("+s.Addr+")")
		}
	}
	switch {
	case len(recent) > 0:
		c.Status, c.Finding = OK, fmt.Sprintf("announcements received from %d device(s) in the last %s: %s",
			len(recent), heardWindow, strings.Join(recent, ", "))
	case !last.IsZero():
		c.Status, c.Finding = Warn, fmt.Sprintf("no announcement in the last %s (last one %s ago): UDP %d multicast from peers is not arriving",
			heardWindow, now.Sub(last).Round(time.Second), MeshPort)
	case len(n.Peers) > 0:
		c.Status, c.Finding = Warn, fmt.Sprintf("no announcement ever received although %d device(s) are paired: this firewall or the Wi-Fi drops UDP %d multicast, or the node runs with --no-discovery; run `messh doctor` on the other devices too",
			len(n.Peers), MeshPort)
	default:
		c.Status, c.Finding = Warn, "no other messh device heard yet (start `messh node` on another device on this LAN)"
	}
	return c
}

func peerChecks(n Node, now time.Time) []Check {
	if n.PeersErr != nil {
		return []Check{{ID: "peers", Status: Unknown, Finding: "cannot list peers: " + n.PeersErr.Error()}}
	}
	if len(n.Peers) == 0 {
		return []Check{{ID: "peers", Status: Warn, Finding: "no paired devices",
			Fix: []string{"messh pair accept   (here; then `messh pair NAME` on the other device)"}}}
	}
	var out []Check
	for _, p := range n.Peers {
		c := Check{ID: "peer " + p.Name}
		addrs := strings.Join(p.Addrs, ", ")
		if addrs == "" {
			addrs = "no known address"
		}
		last := "never"
		if !p.LastSeen.IsZero() {
			last = now.Sub(p.LastSeen).Round(time.Second).String() + " ago"
		}
		if p.Online {
			c.Status, c.Finding = OK, fmt.Sprintf("online, last contact %s, %s", last, addrs)
		} else {
			c.Status, c.Finding = Warn, fmt.Sprintf("offline, last contact %s, addresses %s", last, addrs)
			c.Fix = []string{"on " + p.Name + ": messh doctor"}
		}
		out = append(out, c)
	}
	return out
}
