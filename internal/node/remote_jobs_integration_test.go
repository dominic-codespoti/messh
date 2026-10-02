package node

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRemoteJobOutboxRetriesAcceptedSubmissionAcrossNodeRestart(t *testing.T) {
	surfaceA, surfaceB := &scriptSurface{}, &scriptSurface{}
	b := startGateNode(t, "target", surfaceB)
	a := startGateNode(t, "source", surfaceA)
	pair(t, b, a)
	if _, err := a.paths.AddAgent("agent"); err != nil {
		t.Fatal(err)
	}

	lost := make(chan struct{}, 2)
	var drop atomic.Bool
	var calls atomic.Int32
	drop.Store(true)
	installResponseLossHook(a, &drop, &calls, lost)
	firstArgs := json.RawMessage(`{"command":"echo first","shell":true,"request_id":"lost-response-1"}`)
	accepted, err := a.Call(t.Context(), b.ID(), "job_submit", firstArgs, "agent")
	if err != nil {
		t.Fatal(err)
	}
	var first struct {
		JobID string `json:"job_id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(resultText(accepted)), &first); err != nil {
		t.Fatal(err)
	}
	if first.JobID == "" || first.State != "pending_delivery" {
		t.Fatalf("submission was not durably queued: %s", resultText(accepted))
	}
	select {
	case <-lost:
	case <-time.After(10 * time.Second):
		t.Fatal("B never accepted the submission before its response was lost")
	}
	waitFor(t, "B approval ticket", func() bool { return len(b.approvals.Pending()) == 1 })

	// Reopening A reloads the same ID and retries the accepted request. B's
	// durable receipt must short-circuit the gate rather than prompt twice.
	a.Close()
	a2 := restartRemoteJobsNode(t, a)
	installResponseLossHook(a2, &drop, &calls, lost)
	waitFor(t, "A outbox retry after restart", func() bool { return calls.Load() >= 2 && len(b.approvals.Pending()) == 1 })
	status, err := a2.Call(t.Context(), b.ID(), "job_status", json.RawMessage(`{"job_id":"`+first.JobID+`"}`), "agent")
	if err != nil {
		t.Fatal(err)
	}
	var known map[string]any
	if err := json.Unmarshal([]byte(resultText(status)), &known); err != nil {
		t.Fatal(err)
	}
	if known["job_id"] != first.JobID {
		t.Fatalf("retry changed job identity: %s", resultText(status))
	}
	if got := len(b.approvals.Pending()); got != 1 {
		t.Fatalf("one accepted submission created %d approval tickets", got)
	}

	// A second accepted request loses its response too. Cancel it while B is
	// down; A must retain an unconfirmed cancellation and apply it after B restarts.
	drop.Store(true)
	secondArgs := json.RawMessage(`{"command":"echo second","shell":true,"request_id":"lost-response-2"}`)
	secondRes, err := a2.Call(t.Context(), b.ID(), "job_submit", secondArgs, "agent")
	if err != nil {
		t.Fatal(err)
	}
	var second struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal([]byte(resultText(secondRes)), &second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lost:
	case <-time.After(10 * time.Second):
		t.Fatal("B never accepted the second submission")
	}
	waitFor(t, "second B approval ticket", func() bool { return len(b.approvals.Pending()) == 2 })
	b.Close()
	cancelled, err := a2.Call(t.Context(), b.ID(), "job_cancel", json.RawMessage(`{"job_id":"`+second.JobID+`"}`), "agent")
	if err != nil {
		t.Fatal(err)
	}
	var pending struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(resultText(cancelled)), &pending); err != nil {
		t.Fatal(err)
	}
	if pending.State != "cancellation_pending" {
		t.Fatalf("offline cancellation was reported as complete: %s", resultText(cancelled))
	}

	b3 := restartRemoteJobsNode(t, b)
	if err := a2.roster.Seen(b3.ID(), b3.MeshAddr(), time.Now()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "remote cancellation confirmed", func() bool {
		r := a2.remoteJobs.find(b3.ID(), "agent", second.JobID)
		return r != nil && r.State == "cancelled" && !r.Cancel
	})
	final, err := a2.Call(t.Context(), b3.ID(), "job_status", json.RawMessage(`{"job_id":"`+second.JobID+`"}`), "agent")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resultText(final), `"cancelled"`) {
		t.Fatalf("B did not confirm cancellation: %s", resultText(final))
	}
	if got := len(b3.approvals.Pending()); got != 1 {
		t.Fatalf("cancelled submission left an approval ticket: pending=%d", got)
	}
}
func TestRemoteJobOutboxDefaultsWorkspaceForOfflineSubmission(t *testing.T) {
	b := startGateNode(t, "target", &scriptSurface{})
	a := startGateNode(t, "source", &scriptSurface{})
	pair(t, b, a)
	if err := a.peers.refresh(t.Context(), b.ID()); err != nil {
		t.Fatalf("establish peer session before target shutdown: %v", err)
	}
	if _, err := a.paths.AddAgent("agent"); err != nil {
		t.Fatal(err)
	}
	b.Close()

	args := json.RawMessage(`{"command":"echo workspace","shell":true,"request_id":"default-workspace-restart"}`)
	queued, err := a.Call(t.Context(), b.ID(), "job_submit", args, "agent")
	if err != nil {
		t.Fatal(err)
	}
	var first struct {
		JobID     string `json:"job_id"`
		State     string `json:"state"`
		Workspace string `json:"workspace"`
	}
	if err := json.Unmarshal([]byte(resultText(queued)), &first); err != nil {
		t.Fatal(err)
	}
	if first.JobID == "" || first.State != "pending_delivery" || first.Workspace != "ws/"+first.JobID {
		t.Fatalf("offline submission receipt = %+v", first)
	}
	waitFor(t, "offline submission transport result persisted", func() bool {
		rec := a.remoteJobs.find(b.ID(), "agent", first.JobID)
		return rec != nil && (!rec.NextTry.IsZero() || rec.State != "pending_delivery")
	})
	if rec := a.remoteJobs.find(b.ID(), "agent", first.JobID); rec == nil || rec.State != "pending_delivery" {
		t.Fatalf("transport failure was treated as target rejection: %+v", rec)
	}
	beforeRestart := a.remoteJobs.find(b.ID(), "agent", first.JobID)
	if beforeRestart == nil {
		t.Fatal("queued outbox record disappeared")
	}
	a.Close()
	a2 := restartRemoteJobsNode(t, a)
	waitFor(t, "pending delivery retried after source restart", func() bool {
		rec := a2.remoteJobs.find(b.ID(), "agent", first.JobID)
		return rec != nil && rec.State == "pending_delivery" && rec.Attempt > beforeRestart.Attempt
	})

	b2 := restartRemoteJobsNode(t, b)
	if err := a2.roster.Seen(b2.ID(), b2.MeshAddr(), time.Now()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "queued workspace job accepted by target", func() bool { return len(b2.approvals.Pending()) == 1 })
	// Target acceptance precedes the origin receiving and persisting its receipt.
	waitFor(t, "target acceptance receipt persisted by origin", func() bool {
		rec := a2.remoteJobs.find(b2.ID(), "agent", first.JobID)
		return rec != nil && rec.State == "awaiting_approval"
	})
	retried, err := a2.Call(t.Context(), b2.ID(), "job_submit", args, "agent")
	if err != nil {
		t.Fatal(err)
	}
	var accepted struct {
		JobID     string `json:"job_id"`
		State     string `json:"state"`
		Workspace string `json:"workspace"`
	}
	responseText := resultText(retried)
	if retried.IsError {
		t.Fatalf("repeated job_submit returned IsError response: %q", responseText)
	}
	if err := json.Unmarshal([]byte(responseText), &accepted); err != nil {
		t.Fatalf("decode repeated job_submit response %q: %v", responseText, err)
	}
	if accepted.JobID != first.JobID || accepted.State != "awaiting_approval" || accepted.Workspace != first.Workspace {
		t.Fatalf("target acceptance changed submission identity/workspace: queued=%+v accepted=%+v response=%q", first, accepted, responseText)
	}
}

func restartRemoteJobsNode(t *testing.T, previous *Node) *Node {
	t.Helper()
	opts := previous.opts
	opts.MeshAddr = "127.0.0.1:0"
	opts.LocalAddr = "127.0.0.1:0"
	n, err := Start(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	return n
}
func installResponseLossHook(n *Node, drop *atomic.Bool, calls *atomic.Int32, lost chan<- struct{}) {
	n.remoteJobs.setPeerCall(func(ctx context.Context, id, tool string, args json.RawMessage, agent string) (*mcp.CallToolResult, error) {
		if tool == "job_submit" {
			calls.Add(1)
		}
		res, err := n.peers.callRaw(ctx, id, tool, args, agent)
		if tool == "job_submit" && err == nil && drop.CompareAndSwap(true, false) {
			lost <- struct{}{}
			return nil, errors.New("simulated response loss after target accepted the request")
		}
		return res, err
	})
}
