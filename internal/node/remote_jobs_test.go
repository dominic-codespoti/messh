package node

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/identity"
	"messh/internal/jobs"
	"messh/internal/roster"
	"messh/internal/state"
)

func TestRemoteJobOutboxPersistsIdentityCancellationAndStaleCache(t *testing.T) {
	root := t.TempDir()
	paths := state.Paths{Root: root}
	ro, err := roster.Load(paths.RosterFile())
	if err != nil {
		t.Fatal(err)
	}
	target, agent := "peer-device", "agent-a"
	if err := ro.Pair(target, "peer", "127.0.0.1:1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := paths.AddAgent("agent-a"); err != nil {
		t.Fatal(err)
	}
	n := &Node{paths: paths, id: &identity.Identity{ID: "source-device"}, roster: ro, log: slog.New(slog.DiscardHandler)}
	out, err := newRemoteJobOutbox(n)
	if err != nil {
		t.Fatal(err)
	}
	args := json.RawMessage(`{"command":"echo","args":["one"],"request_id":"stable-1"}`)
	first, err := out.submit(context.Background(), target, agent, args)
	if err != nil {
		t.Fatal(err)
	}
	var submitted struct {
		JobID string `json:"job_id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(resultText(first)), &submitted); err != nil {
		t.Fatal(err)
	}
	if submitted.JobID != jobs.SubmissionID(n.id.ID, agent, "stable-1") || submitted.State != "pending_delivery" {
		t.Fatalf("unexpected receipt: %+v", submitted)
	}
	same, err := out.submit(context.Background(), target, agent, args)
	if err != nil {
		t.Fatal(err)
	}
	var repeated struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal([]byte(resultText(same)), &repeated); err != nil {
		t.Fatal(err)
	}
	if repeated.JobID != submitted.JobID {
		t.Fatalf("retry id %q differs from %q", repeated.JobID, submitted.JobID)
	}
	changed := json.RawMessage(`{"command":"echo","args":["different"],"request_id":"stable-1"}`)
	conflict, err := out.submit(context.Background(), target, agent, changed)
	if err != nil {
		t.Fatal(err)
	}
	if !conflict.IsError {
		t.Fatal("same request_id with a different payload was accepted")
	}
	if _, ok := out.cancelPending(target, agent, submitted.JobID); !ok {
		t.Fatal("offline cancellation was not retained")
	}
	restarted, err := newRemoteJobOutbox(n)
	if err != nil {
		t.Fatal(err)
	}
	restarted.deliver(submitted.JobID) // certainly unsent: cancellation must suppress submission
	rec := restarted.find(target, agent, submitted.JobID)
	if rec == nil || rec.State != "cancelled" {
		t.Fatalf("restart lost unsent cancellation: %+v", rec)
	}
	if _, err := os.Stat(filepath.Join(root, "remote-jobs.json")); err != nil {
		t.Fatalf("outbox was not durably written: %v", err)
	}
	rec.AgentFingerprint, _ = restarted.agentFingerprint(agent)
	rec.Cached = json.RawMessage(`{"job_id":"` + submitted.JobID + `","state":"running"}`)
	rec.State = "running"
	rec.Cancel = false
	restarted.mu.Lock()
	restarted.records[rec.ID] = rec
	if err := restarted.saveLocked(); err != nil {
		restarted.mu.Unlock()
		t.Fatal(err)
	}
	restarted.mu.Unlock()
	stale, err := restarted.stale(target, agent, "job_status", json.RawMessage(`{"job_id":"`+submitted.JobID+`"}`), errors.New("peer unavailable"))
	if err != nil {
		t.Fatal(err)
	}
	var cached map[string]any
	if err := json.Unmarshal([]byte(resultText(stale)), &cached); err != nil {
		t.Fatal(err)
	}
	if cached["state"] != "running" || cached["stale"] != true || cached["stale_reason"] == nil {
		t.Fatalf("cached result was not identified as stale: %#v", cached)
	}
}
func TestRemoteJobOutboxPersistsTargetRejectionAndRetriesTransportFailure(t *testing.T) {
	root := t.TempDir()
	paths := state.Paths{Root: root}
	ro, err := roster.Load(paths.RosterFile())
	if err != nil {
		t.Fatal(err)
	}
	const target, agent = "peer-device", "agent-a"
	if err := ro.Pair(target, "peer", "127.0.0.1:1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := paths.AddAgent(agent); err != nil {
		t.Fatal(err)
	}
	n := &Node{ctx: context.Background(), paths: paths, id: &identity.Identity{ID: "source-device"}, roster: ro, log: slog.New(slog.DiscardHandler)}

	t.Run("plaintext target rejection survives restart", func(t *testing.T) {
		args := json.RawMessage(`{"command":"echo","args":["one"],"request_id":"target-rejection"}`)
		out, err := newRemoteJobOutbox(n)
		if err != nil {
			t.Fatal(err)
		}
		_, err = out.submit(context.Background(), target, agent, args)
		if err != nil {
			t.Fatal(err)
		}
		reason := "authorization rejected by target: operation is not permitted"
		out.setPeerCall(func(context.Context, string, string, json.RawMessage, string) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: reason}}}, nil
		})
		id := jobs.SubmissionID(n.id.ID, agent, "target-rejection")
		out.deliverOne(id)

		restarted, err := newRemoteJobOutbox(n)
		if err != nil {
			t.Fatal(err)
		}
		result, err := restarted.submit(context.Background(), target, agent, args)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			JobID     string `json:"job_id"`
			State     string `json:"state"`
			Error     string `json:"error"`
			Workspace string `json:"workspace"`
		}
		if err := json.Unmarshal([]byte(resultText(result)), &got); err != nil {
			t.Fatalf("persisted rejection was not valid JSON: %v (%q)", err, resultText(result))
		}
		if result.IsError || got.JobID != id || got.State != "failed" || got.Error != reason || got.Workspace != "ws/"+id {
			t.Fatalf("restarted rejection receipt = %+v (IsError=%v)", got, result.IsError)
		}
	})

	t.Run("transport failure stays retryable across restart", func(t *testing.T) {
		retryArgs := json.RawMessage(`{"command":"echo","args":["two"],"request_id":"transport-retry"}`)
		out, err := newRemoteJobOutbox(n)
		if err != nil {
			t.Fatal(err)
		}
		_, err = out.submit(context.Background(), target, agent, retryArgs)
		if err != nil {
			t.Fatal(err)
		}
		id := jobs.SubmissionID(n.id.ID, agent, "transport-retry")
		out.setPeerCall(func(context.Context, string, string, json.RawMessage, string) (*mcp.CallToolResult, error) {
			return nil, errors.New("connection reset while target was unreachable")
		})
		out.deliverOne(id)
		if rec := out.find(target, agent, id); rec == nil || rec.State != "pending_delivery" {
			t.Fatalf("transport failure became terminal: %+v", rec)
		}

		restarted, err := newRemoteJobOutbox(n)
		if err != nil {
			t.Fatal(err)
		}
		if rec := restarted.find(target, agent, id); rec == nil || rec.State != "pending_delivery" {
			t.Fatalf("restart lost pending transport failure: %+v", rec)
		}
		var accepted atomic.Int32
		restarted.setPeerCall(func(context.Context, string, string, json.RawMessage, string) (*mcp.CallToolResult, error) {
			accepted.Add(1)
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: ` {"job_id":"` + id + `","state":"awaiting_approval","workspace":"ws/` + id + `"}`}}}, nil
		})
		restarted.deliverOne(id)
		rec := restarted.find(target, agent, id)
		if accepted.Load() != 1 || rec == nil || rec.State != "awaiting_approval" {
			t.Fatalf("pending submission was not accepted on retry: calls=%d record=%+v", accepted.Load(), rec)
		}
	})
}
