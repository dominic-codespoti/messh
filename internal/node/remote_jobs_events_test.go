package node

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"messh/internal/identity"
	"messh/internal/jobs"
	"messh/internal/state"
)

func TestOriginEventReplaySurvivesRestartAndOtherOwnerRetention(t *testing.T) {
	paths := state.Paths{Root: t.TempDir()}
	for _, agent := range []string{"agent-a", "agent-b"} {
		if _, err := paths.AddAgent(agent); err != nil {
			t.Fatal(err)
		}
	}
	n := &Node{paths: paths, id: &identity.Identity{ID: "origin"}, log: slog.New(slog.DiscardHandler)}
	out, err := newRemoteJobOutbox(n)
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range []string{"agent-a", "agent-b"} {
		args := json.RawMessage(`{"command":"echo","request_id":"one"}`)
		result, err := out.submit(context.Background(), "target", agent, args)
		if err != nil || result.IsError {
			t.Fatalf("submit %s: %v %v", agent, result, err)
		}
	}
	a := out.records[jobs.SubmissionID(n.id.ID, "agent-a", "one")]
	b := out.records[jobs.SubmissionID(n.id.ID, "agent-b", "one")]
	first := a.OriginEvents[0]
	for i := 0; i < originEventRetention+2; i++ {
		b.State = "running"
		if i%2 == 0 {
			b.State = "queued"
		}
		b.Updated = time.Now()
		if err := out.saveLocked(); err != nil {
			t.Fatal(err)
		}
	}
	a.State = "cancellation_pending"
	a.Updated = time.Now()
	if err := out.saveLocked(); err != nil {
		t.Fatal(err)
	}
	cancelled := a.OriginEvents[len(a.OriginEvents)-1]
	restarted, err := newRemoteJobOutbox(n)
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, _ := restarted.agentFingerprint("agent-a")
	page, expired := restarted.originEventPage("target", "agent-a", fingerprint, first.Sequence, 128)
	if expired || len(page.Events) != 1 || page.Events[0].ID != cancelled.ID || page.Events[0].State != "cancellation_pending" || page.Events[0].Source != "origin" {
		t.Fatalf("owner replay after restart and unrelated pruning: expired=%v page=%+v", expired, page)
	}
	bFingerprint, _ := restarted.agentFingerprint("agent-b")
	_, expired = restarted.originEventPage("target", "agent-b", bFingerprint, 1, 128)
	if !expired {
		t.Fatal("pruned owning stream did not expire")
	}
}

func TestOriginEventFailedCommitRestoresRetainedHistory(t *testing.T) {
	n := &Node{paths: state.Paths{Root: t.TempDir()}, id: &identity.Identity{ID: "origin"}, log: slog.New(slog.DiscardHandler)}
	out, err := newRemoteJobOutbox(n)
	if err != nil {
		t.Fatal(err)
	}
	record := &remoteJobRecord{ID: "job", DeviceID: "target", Agent: "agent", State: "queued", Updated: time.Now()}
	out.records[record.ID] = record
	for i := 0; i < originEventRetention; i++ {
		record.State = "running"
		if i%2 == 0 {
			record.State = "queued"
		}
		if err := out.saveLocked(); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(out.path)
	if err != nil {
		t.Fatal(err)
	}
	first, last := record.OriginEvents[0], record.OriginEvents[len(record.OriginEvents)-1]
	sequence := out.nextOriginEventSequence
	blocked := filepath.Join(n.paths.Root, "blocked")
	if err := os.Mkdir(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	out.path = blocked
	record.State = "failed"
	if err := out.saveLocked(); err == nil {
		t.Fatal("commit to directory unexpectedly succeeded")
	}
	if len(record.OriginEvents) != originEventRetention || record.OriginEvents[0].ID != first.ID || record.OriginEvents[len(record.OriginEvents)-1].ID != last.ID || record.OriginEventFloor != 0 || out.nextOriginEventSequence != sequence {
		t.Fatalf("failed commit damaged replay history: %+v", record)
	}
	persisted, err := os.ReadFile(filepath.Join(n.paths.Root, "remote-jobs.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(persisted) != string(before) {
		t.Fatal("failed commit changed durable outbox")
	}
}
