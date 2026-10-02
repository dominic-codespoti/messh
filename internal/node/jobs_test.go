package node

import (
	"encoding/json"
	"strings"
	"testing"

	"messh/internal/approval"
)

// A paired device submits a job to the desktop: it waits for the person at
// the desktop, runs once allowed, and the owner can read it back.
func TestJobsThroughTheMesh(t *testing.T) {
	desktop := startGateNode(t, "desktop", &scriptSurface{})
	raspi := startGateNode(t, "raspi", &scriptSurface{})
	pair(t, desktop, raspi)
	s := agentSession(t, raspi)
	waitForTool(t, s, "desktop__job_submit")

	res := callTool(t, s, "desktop__job_submit", map[string]any{"command": "echo from-the-mesh", "shell": true})
	if res.IsError {
		t.Fatalf("job_submit: %s", resultText(res))
	}
	var sub struct {
		JobID string `json:"job_id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(resultText(res)), &sub); err != nil || sub.JobID == "" {
		t.Fatalf("submit result %q: %v", resultText(res), err)
	}
	if sub.State != "pending_delivery" {
		t.Fatalf("initial submit state = %s, want pending_delivery", sub.State)
	}
	// Submission is durable before the host has accepted it, so wait on the
	// owner's real status tool until the asynchronous acceptance is visible.
	var pending struct {
		State      string   `json:"state"`
		Started    *string  `json:"started"`
		Finished   *string  `json:"finished"`
		StdoutTail []string `json:"stdout_tail"`
		StderrTail []string `json:"stderr_tail"`
	}
	waitFor(t, "durable job acceptance", func() bool {
		res = callTool(t, s, "desktop__job_status", map[string]any{"job_id": sub.JobID})
		if res.IsError || json.Unmarshal([]byte(resultText(res)), &pending) != nil {
			return false
		}
		return pending.State == "awaiting_approval"
	})
	if pending.Started != nil || pending.Finished != nil || len(pending.StdoutTail) != 0 || len(pending.StderrTail) != 0 {
		t.Fatalf("unapproved job has execution data: %+v", pending)
	}

	waitFor(t, "the prompt", func() bool { return len(desktop.approvals.Pending()) == 1 })
	if err := desktop.approvals.Resolve(desktop.approvals.Pending()[0].ID, approval.Answer{Allow: true}); err != nil {
		t.Fatal(err)
	}
	res = callTool(t, s, "desktop__job_wait", map[string]any{"job_id": sub.JobID, "timeout_seconds": 30})
	if res.IsError || !strings.Contains(resultText(res), `"succeeded"`) || !strings.Contains(resultText(res), "from-the-mesh") {
		t.Fatalf("job_wait: error=%v %s", res.IsError, resultText(res))
	}

	// A different device (here the desktop's own agent) cannot see the job.
	other := agentSession(t, desktop)
	waitForTool(t, other, "desktop__job_status")
	if res := callTool(t, other, "desktop__job_status", map[string]any{"job_id": sub.JobID}); !res.IsError {
		t.Fatalf("a different device read the job: %s", resultText(res))
	}
}
