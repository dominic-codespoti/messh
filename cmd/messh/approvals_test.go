package main

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

// runApprovalPipe invokes the CLI with a fresh state directory that holds no node, so
// dialing fails with node_not_running. Stdin is a pipe nobody ever writes to:
// if a command read stdin it would block, and the timeout below fails the
// test instead of hanging the suite.
func runApprovalPipe(t *testing.T, timeout time.Duration, args ...string) (int, string, string) {
	t.Helper()
	args = append(args, "--state", t.TempDir())
	pr, _ := io.Pipe() // never written, never closed
	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() { done <- run(args, pr, true, &stdout, &stderr) }()
	select {
	case code := <-done:
		return code, stdout.String(), stderr.String()
	case <-time.After(timeout):
		t.Fatalf("messh %s blocked instead of returning", strings.Join(args[:len(args)-2], " "))
		return -1, "", ""
	}
}

func TestApprovalsJSONWithoutNode(t *testing.T) {
	code, _, stderr := runApprovalPipe(t, 15*time.Second, "approvals", "--json")
	if code != exitNoNode {
		t.Fatalf("approvals --json exit = %d, want %d", code, exitNoNode)
	}
	if !strings.Contains(stderr, "node_not_running") {
		t.Fatalf("approvals --json stderr lacks kind node_not_running: %q", stderr)
	}
}

func TestApprovalsAlwaysBadScope(t *testing.T) {
	code, _, _ := runApprovalPipe(t, 15*time.Second, "approvals", "always", "3fa85f64", "--scope", "notanumber")
	if code != exitUsage {
		t.Fatalf("approvals always --scope notanumber exit = %d, want %d", code, exitUsage)
	}
}

func TestScheduleShowNeedsID(t *testing.T) {
	code, _, _ := runApprovalPipe(t, 15*time.Second, "schedule", "show")
	if code != exitUsage {
		t.Fatalf("schedule show exit = %d, want %d", code, exitUsage)
	}
}

func TestRespondMalformedURL(t *testing.T) {
	// On Windows this shows the same small message box production shows for a
	// bad click; dismiss it. The generous timeout is only so the suite fails
	// instead of hanging if respond ever blocked on stdin.
	code, _, _ := runApprovalPipe(t, 2*time.Minute, "respond", "not-a-url")
	if code != exitFailure && code != exitUsage {
		t.Fatalf("respond not-a-url exit = %d, want %d or %d", code, exitFailure, exitUsage)
	}
}
