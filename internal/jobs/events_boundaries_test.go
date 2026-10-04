package jobs

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"messh/internal/provider"
	"messh/internal/state"
)

func eventJob(t *testing.T, h *harness, caller provider.Caller) *job {
	t.Helper()
	id, _ := h.submit(helperArgs("wait"), caller)
	h.p.mu.Lock()
	defer h.p.mu.Unlock()
	return h.p.jobs[id]
}

func appendTestEvents(t *testing.T, p *Provider, j *job, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		p.mu.Lock()
		err := p.appendEventLocked(j, "boundary")
		p.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestEventsCursorUsesOwnerFloorsAndStableIdentity(t *testing.T) {
	h := newHarness(t, nil)
	other := provider.Caller{DeviceID: "dev-b", DeviceName: "peer", Agent: "omp"}
	owned := eventJob(t, h, agentA)
	foreign := eventJob(t, h, other)
	page, err := h.p.Events(context.Background(), agentA, "", eventRetention, 0)
	if err != nil || len(page.Events) == 0 {
		t.Fatalf("initial events: %+v, %v", page, err)
	}
	cursor := page.NextCursor
	appendTestEvents(t, h.p, foreign, 3)
	if _, err := h.p.Events(context.Background(), agentA, cursor, eventRetention, 0); err != nil {
		t.Fatalf("global sequence gaps expired owner cursor: %v", err)
	}
	renamed := agentA
	renamed.DeviceName = "renamed device"
	if _, err := h.p.Events(context.Background(), renamed, cursor, eventRetention, 0); err != nil {
		t.Fatalf("display-name change invalidated cursor: %v", err)
	}

	// Keep one old retained job while pruning a different owned job. Its newer
	// owner floor must expire the cursor even though the first job has old events.
	pruned := eventJob(t, h, agentA)
	stale := page.NextCursor
	appendTestEvents(t, h.p, pruned, eventRetention+1)
	var expired *CursorExpiredError
	_, err = h.p.Events(context.Background(), agentA, stale, eventRetention, 0)
	if !errors.As(err, &expired) {
		t.Fatalf("cursor before pruned floor: got %v, want expiration", err)
	}
	if owned == pruned {
		t.Fatal("test jobs unexpectedly identical")
	}
}

func TestEventsAppendFailurePreservesRetainedHistoryAndFloor(t *testing.T) {
	paths := state.Paths{Root: t.TempDir()}
	writer := &failWrites{}
	p := newDurableProvider(t, paths, writer, nil)
	t.Cleanup(p.Close)
	id := "boundary-job"
	persistFixtureJob(t, paths, Job{ID: id, Owner: Owner{DeviceID: agentA.DeviceID, Agent: agentA.Agent}, State: StateSubmitted})
	// Reload so this is a real provider-owned job with its normal retained ring.
	p.Close()
	p = newDurableProvider(t, paths, writer, nil)
	j := p.jobs[id]
	if j == nil {
		t.Fatal("fixture job was not restored")
	}
	appendTestEvents(t, p, j, eventRetention)
	before, err := p.Events(context.Background(), agentA, "", eventRetention, 0)
	if err != nil || len(before.Events) != eventRetention {
		t.Fatalf("before append: %d events, %v", len(before.Events), err)
	}
	priorFloor, priorSeq := j.EventDroppedThrough, p.nextEventSeq
	jobFile := filepath.Join(paths.JobsDir(), id, "job.json")
	writer.arm(1, func(path string, _ []byte) bool { return strings.Contains(path, jobFile) })
	p.mu.Lock()
	err = p.appendEventLocked(j, "failure")
	p.mu.Unlock()
	if err == nil {
		t.Fatal("append unexpectedly succeeded despite injected durable write failure")
	}
	if len(j.Events) != eventRetention || j.EventDroppedThrough != priorFloor || p.nextEventSeq != priorSeq {
		t.Fatalf("failed append changed ring/floor/sequence: len=%d floor=%d seq=%d", len(j.Events), j.EventDroppedThrough, p.nextEventSeq)
	}
	after, err := p.Events(context.Background(), agentA, before.NextCursor, eventRetention, 0)
	if err != nil || len(after.Events) != 0 {
		t.Fatalf("failed append changed visible history: %+v, %v", after, err)
	}
	p.mu.Lock()
	err = p.appendEventLocked(j, "recovered")
	p.mu.Unlock()
	if err != nil {
		t.Fatalf("append did not recover after writer restored: %v", err)
	}
	got, err := p.Events(context.Background(), agentA, before.NextCursor, eventRetention, 0)
	if err != nil || len(got.Events) != 1 || got.Events[0].Type != "recovered" {
		t.Fatalf("recovered cursor: %+v, %v", got, err)
	}
}

func TestEventsDeletionFloorSurvivesRestartAndRemainsOwnerScoped(t *testing.T) {
	h := newHarness(t, nil)
	owned := eventJob(t, h, agentA)
	appendTestEvents(t, h.p, owned, eventRetention+1)
	stale := encodeEventCursor(owned.Owner, 1)
	h.p.mu.Lock()
	err := h.p.appendDeletionEventLocked(owned)
	h.p.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	paths := h.paths
	deletedSequence := h.p.tombstoneEvents[len(h.p.tombstoneEvents)-1].Sequence
	h.p.Close()
	restored := newDurableProvider(t, paths, nil, nil)
	page, err := restored.Events(context.Background(), agentA, stale, eventRetention, 0)
	var expired *CursorExpiredError
	if !errors.As(err, &expired) {
		t.Fatalf("deleted owner persisted floor not enforced: page=%+v err=%v", page, err)
	}
	other := provider.Caller{DeviceID: "dev-b", Agent: "omp"}
	private, err := restored.Events(context.Background(), other, stale, eventRetention, 0)
	if err == nil || private.Events != nil {
		t.Fatalf("foreign caller accepted owner cursor: %+v, %v", private, err)
	}
	fresh, err := restored.Events(context.Background(), agentA, encodeEventCursor(owned.Owner, deletedSequence-1), eventRetention, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range fresh.Events {
		if e.Type == "deleted" && e.JobID == owned.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("restored owner cannot see deleted event: %+v", fresh.Events)
	}
}
