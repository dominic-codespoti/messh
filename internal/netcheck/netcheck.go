// Package netcheck diagnoses whether this device can take part in the mesh:
// the running node and its listen address, the LAN interfaces and their
// Windows network category, the OS firewall, multicast, discovery and paired
// peers. Everything here only reads; the firewall command builders return
// commands for `messh firewall` to run.
package netcheck

import (
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"

	"messh/internal/discovery"
)

// MeshPort is the TCP port peers connect to and the UDP port of discovery.
const MeshPort = 7519

// Names of the inbound rules `messh firewall allow` creates on Windows.
const (
	RuleMesh      = "messh (mesh TCP 7519)"
	RuleDiscovery = "messh (discovery UDP 7519)"
)

// DiscoveryGroup is the multicast group discovery announcements go to.
var DiscoveryGroup = discovery.DefaultGroup.Addr()

// Status is the outcome of one check.
type Status string

const (
	OK      Status = "ok"
	Warn    Status = "warn"
	Fail    Status = "fail"
	Unknown Status = "unknown" // could not be determined (no permission, timeout, tool missing)
)

func (s Status) rank() int {
	switch s {
	case Fail:
		return 3
	case Warn:
		return 2
	case Unknown:
		return 1
	}
	return 0
}

// Check is one diagnostic: what was looked at, the verdict, a one-line
// finding and the commands that fix it (empty when nothing is to be done or
// the fix is not a command on this device).
type Check struct {
	ID      string   `json:"id"`
	Status  Status   `json:"status"`
	Finding string   `json:"finding"`
	Fix     []string `json:"fix,omitempty"`
}

// Report is the result of `messh doctor`.
type Report struct {
	Host    string  `json:"host"`
	OS      string  `json:"os"`
	Program string  `json:"program,omitempty"` // the executable the firewall must let in
	Checks  []Check `json:"checks"`
}

// Worst returns the most severe status in the report.
func (r Report) Worst() Status {
	w := OK
	for _, c := range r.Checks {
		if c.Status.rank() > w.rank() {
			w = c.Status
		}
	}
	return w
}

// Count returns how many checks have status s.
func (r Report) Count(s Status) int {
	n := 0
	for _, c := range r.Checks {
		if c.Status == s {
			n++
		}
	}
	return n
}

// Filter returns a copy holding only the checks whose ID starts with one of prefixes.
func (r Report) Filter(prefixes ...string) Report {
	out := r
	out.Checks = nil
	for _, c := range r.Checks {
		for _, p := range prefixes {
			if strings.HasPrefix(c.ID, p) {
				out.Checks = append(out.Checks, c)
				break
			}
		}
	}
	return out
}

// Fixes returns the fix commands of all checks in report order, each once.
func (r Report) Fixes() []string {
	var out []string
	for _, c := range r.Checks {
		for _, f := range c.Fix {
			if !slices.Contains(out, f) {
				out = append(out, f)
			}
		}
	}
	return out
}

// Render writes the human table followed by the numbered fix list.
func (r Report) Render(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "STATUS\tCHECK\tFINDING")
	for _, c := range r.Checks {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", c.Status, c.ID, oneLine(c.Finding))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fixes := r.Fixes()
	if len(fixes) == 0 {
		_, err := fmt.Fprintln(w, "\nNo fixes needed.")
		return err
	}
	if _, err := fmt.Fprintln(w, "\nFix:"); err != nil {
		return err
	}
	for i, f := range fixes {
		if _, err := fmt.Fprintf(w, "  %d. %s\n", i+1, f); err != nil {
			return err
		}
	}
	return nil
}

// oneLine keeps tabs and newlines from breaking the table.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// quoteArg quotes a command-line argument for display when it needs it;
// double quotes work in cmd, PowerShell and POSIX shells for the values used here.
func quoteArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t'\"&|;<>()$`") {
		return s
	}
	return `"` + s + `"`
}
