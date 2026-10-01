//go:build linux

package jobs

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The test binary also stands in for systemd-inhibit: started with
// systemd-inhibit's arguments and MESSH_INHIBIT_HELPER set, it acts out the
// named outcome instead of running tests. Job helpers inherit the variable
// but not the arguments, so they are unaffected.
func init() {
	mode := os.Getenv("MESSH_INHIBIT_HELPER")
	if mode == "" || len(os.Args) < 2 || os.Args[1] != "--what=sleep" {
		return
	}
	switch mode {
	case "denied": // what logind/polkit answers over ssh, headless, or WSL
		fmt.Fprintln(os.Stderr, "Failed to inhibit: Access denied")
		os.Exit(1)
	case "hold":
		time.Sleep(time.Minute)
		os.Exit(0)
	case "lapse":
		time.Sleep(time.Second)
		os.Exit(0)
	}
	os.Exit(2)
}

func standInInhibitor(t *testing.T, mode string) *linuxInhibitor {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	oldBin, oldSettle := inhibitBin, inhibitSettle
	inhibitBin = exe
	t.Cleanup(func() { inhibitBin, inhibitSettle = oldBin, oldSettle })
	t.Setenv("MESSH_INHIBIT_HELPER", mode)
	in := newSleepInhibitor(slog.New(slog.NewTextHandler(io.Discard, nil))).(*linuxInhibitor)
	t.Cleanup(in.Release)
	return in
}

func TestLinuxInhibitorRefused(t *testing.T) {
	in := standInInhibitor(t, "denied")
	inhibitSettle = time.Minute // the refusal must end the wait, not the timer
	start := time.Now()
	err := in.Hold()
	if err == nil || !strings.Contains(err.Error(), "Failed to inhibit: Access denied") {
		t.Fatalf("Hold = %v, want the Access denied refusal", err)
	}
	if time.Since(start) > 30*time.Second {
		t.Fatal("Hold waited out the settle time despite the early exit")
	}
	if st := in.Status(); !strings.HasPrefix(st, "unavailable: ") || !strings.Contains(st, "Access denied") {
		t.Fatalf("Status = %q, want unavailable with the reason", st)
	}
}

func TestLinuxInhibitorHoldAndRelease(t *testing.T) {
	in := standInInhibitor(t, "hold")
	if err := in.Hold(); err != nil {
		t.Fatal(err)
	}
	if st := in.Status(); st != "held" {
		t.Fatalf("Status = %q, want held", st)
	}
	in.mu.Lock()
	c := in.child
	in.mu.Unlock()
	in.Release()
	select {
	case <-c.done:
	default:
		t.Fatal("Release returned before the child exited")
	}
	if ws, ok := c.cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("child ended with %v, want SIGKILL from Release", c.cmd.ProcessState)
	}
	if st := in.Status(); st != "not held" {
		t.Fatalf("Status after Release = %q, want not held", st)
	}
}

func TestLinuxInhibitorLapse(t *testing.T) {
	in := standInInhibitor(t, "lapse")
	if err := in.Hold(); err != nil {
		t.Fatal(err)
	}
	if st := in.Status(); st != "held" {
		t.Fatalf("Status = %q, want held", st)
	}
	deadline := time.Now().Add(20 * time.Second)
	for in.Status() == "held" {
		if time.Now().After(deadline) {
			t.Fatal("status still held after systemd-inhibit exited")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st := in.Status(); !strings.HasPrefix(st, "unavailable: lapsed: ") {
		t.Fatalf("Status = %q, want unavailable: lapsed", st)
	}
	// The lapse frees the slot, so the next Hold starts a new child.
	if err := in.Hold(); err != nil {
		t.Fatal(err)
	}
	if st := in.Status(); st != "held" {
		t.Fatalf("Status after re-Hold = %q, want held", st)
	}
}

// A refused inhibitor must not fail the job, but agents must see that the
// device may sleep under it.
func TestRefusedInhibitorReportedToAgents(t *testing.T) {
	in := standInInhibitor(t, "denied")
	h := newHarness(t, func(o *Options) { o.Inhibitor = in })
	id, tk := h.submit(helperArgs("sleep"), agentA)
	tk.Approve()
	st := h.waitState(id, StateRunning)
	const want = "sleep_inhibit: unavailable: systemd-inhibit: Failed to inhibit: Access denied"
	if !containsLine(st.Notes, want) {
		t.Fatalf("notes %q lack %q", st.Notes, want)
	}
	res, err := h.call(toolResources, map[string]any{}, agentA)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		SleepInhibit string `json:"sleep_inhibit"`
	}
	decodeResult(t, res, &out)
	if !strings.HasPrefix(out.SleepInhibit, "unavailable: ") || !strings.Contains(out.SleepInhibit, "Access denied") {
		t.Fatalf("job_resources sleep_inhibit = %q", out.SleepInhibit)
	}
	h.call(toolCancel, map[string]any{"job_id": id}, agentA)
	h.waitState(id, StateCancelled)
}
