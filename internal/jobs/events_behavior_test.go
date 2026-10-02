package jobs

import (
	"context"
	"testing"
	"time"

	"messh/internal/provider"
)

func TestEventsAreOwnerFilteredAndTrackLifecycle(t *testing.T) {
	h := newHarness(t, nil)
	id, ticket := h.submit(helperArgs("echo"), agentA)
	ticket.Approve()
	h.waitState(id, StateSucceeded)
	page, err := h.p.Events(context.Background(), agentA, "", eventRetention, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, event := range page.Events {
		if event.JobID == id {
			seen[event.Type+":"+string(event.State)] = true
		}
	}
	if !seen["submitted:submitted"] || !seen["state_changed:succeeded"] {
		t.Fatalf("lifecycle events missing: %+v", page.Events)
	}
	other := provider.Caller{DeviceID: agentA.DeviceID, Agent: "other"}
	private, err := h.p.Events(context.Background(), other, "", eventRetention, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(private.Events) != 0 || len(private.Snapshot) != 0 {
		t.Fatalf("another agent saw job events: %+v", private)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := h.p.Events(ctx, agentA, "invalid", 1, 0); err == nil {
		t.Fatal("malformed owner cursor was accepted")
	}
}
