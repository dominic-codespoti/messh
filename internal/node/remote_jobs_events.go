package node

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"messh/internal/jobs"
	"messh/internal/provider"
)

const originEventRetention = 128

// Origin events share the outbox's atomic commit. They describe delivery and
// locally observed target states, never claim to be the target's own journal.
func (o *remoteJobOutbox) appendOriginEventsLocked() func() {
	type restore struct {
		record *remoteJobRecord
		events []jobs.Event
		floor  uint64
		state  string
	}
	var changed []restore
	sequence := o.nextOriginEventSequence
	for _, record := range o.records {
		state := record.State
		if record.Deleted {
			state = "deleted"
		} else if record.Cancel && !isFinalRemoteJob(state) {
			state = "cancellation_pending"
		}
		if state == "" || state == record.OriginEventState {
			continue
		}
		changed = append(changed, restore{record, record.OriginEvents, record.OriginEventFloor, record.OriginEventState})
		typ := "target_status_observed"
		switch {
		case state == "pending_delivery":
			typ = "delivery_pending"
		case state == "cancellation_pending":
			typ = "cancellation_requested"
		case state == "deleted":
			typ = "deleted"
		case record.OriginEventState == "pending_delivery":
			typ = "delivery_confirmed"
		case record.OriginEventState == "":
			typ = "observed_snapshot"
		}
		o.nextOriginEventSequence++
		event := jobs.Event{ID: fmt.Sprintf("origin:%s:%s:%d", o.n.id.ID, record.ID, o.nextOriginEventSequence), Sequence: o.nextOriginEventSequence, JobID: record.ID, Owner: jobs.Owner{DeviceID: o.n.id.ID, DeviceName: o.n.name, Agent: record.Agent}, Source: "origin", Type: typ, State: jobs.State(state), At: record.Updated.UTC()}
		if len(record.OriginEvents) >= originEventRetention {
			record.OriginEventFloor = record.OriginEvents[0].Sequence
			record.OriginEvents = append(append([]jobs.Event(nil), record.OriginEvents[1:]...), event)
		} else {
			record.OriginEvents = append(record.OriginEvents, event)
		}
		record.OriginEventState = state
	}
	return func() {
		o.nextOriginEventSequence = sequence
		for _, old := range changed {
			old.record.OriginEvents = old.events
			old.record.OriginEventFloor = old.floor
			old.record.OriginEventState = old.state
		}
	}
}

type remoteEventCursor struct {
	Version        int    `json:"v"`
	OriginDeviceID string `json:"origin"`
	TargetDeviceID string `json:"target"`
	Agent          string `json:"agent"`
	Fingerprint    string `json:"credential"`
	OriginSequence uint64 `json:"origin_seq"`
	TargetCursor   string `json:"target_cursor,omitempty"`
}

func encodeRemoteEventCursor(cursor remoteEventCursor) string {
	raw, _ := json.Marshal(cursor)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func (o *remoteJobOutbox) originEventPage(deviceID, agent, fingerprint string, after uint64, limit int) (jobs.EventPage, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	page := jobs.EventPage{Events: make([]jobs.Event, 0)}
	var events []jobs.Event
	var discardedThrough uint64
	for _, record := range o.records {
		if record.DeviceID != deviceID || record.Agent != agent || record.AgentFingerprint != fingerprint {
			continue
		}
		discardedThrough = max(discardedThrough, record.OriginEventFloor)
		for _, event := range record.OriginEvents {
			if event.Sequence > after {
				events = append(events, event)
			}
		}
	}
	if after > 0 && after < discardedThrough {
		for _, record := range o.records {
			if record.DeviceID != deviceID || record.Agent != agent || record.AgentFingerprint != fingerprint || record.Deleted {
				continue
			}
			state := record.State
			if record.Cancel && !isFinalRemoteJob(state) {
				state = "cancellation_pending"
			}
			page.Snapshot = append(page.Snapshot, jobs.Summary{JobID: record.ID, State: jobs.State(state), Workspace: "ws/" + remoteJobWorkspace(record)})
		}
		return page, true
	}
	sort.Slice(events, func(i, j int) bool { return events[i].Sequence < events[j].Sequence })
	if len(events) > limit {
		page.HasMore = true
		events = events[:limit]
	}
	page.Events = events
	return page, false
}

func eventError(code, message string, page jobs.EventPage) (*mcp.CallToolResult, error) {
	result, err := provider.JSONResult(map[string]any{"code": code, "error": message, "snapshot": page.Snapshot, "next_cursor": page.NextCursor})
	if result != nil {
		result.IsError = true
	}
	return result, err
}

func (o *remoteJobOutbox) events(ctx context.Context, deviceID, agent, fingerprint string, raw json.RawMessage) (*mcp.CallToolResult, error) {
	var input struct {
		Cursor string `json:"cursor"`
		Limit  int    `json:"limit"`
		WaitMS int    `json:"wait_ms"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&input); err != nil {
		return eventError("invalid_event_request", err.Error(), jobs.EventPage{})
	}
	if input.Limit == 0 {
		input.Limit = originEventRetention
	}
	if input.Limit < 1 || input.Limit > originEventRetention || input.WaitMS < 0 || input.WaitMS > 30000 {
		return eventError("invalid_event_request", "limit must be 1..128 and wait_ms 0..30000", jobs.EventPage{})
	}
	cursor := remoteEventCursor{Version: 1, OriginDeviceID: o.n.id.ID, TargetDeviceID: deviceID, Agent: agent, Fingerprint: fingerprint}
	if input.Cursor != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(input.Cursor)
		var supplied remoteEventCursor
		if err != nil || json.Unmarshal(decoded, &supplied) != nil || supplied.Version != 1 || supplied.OriginDeviceID != cursor.OriginDeviceID || supplied.TargetDeviceID != deviceID || supplied.Agent != agent || supplied.Fingerprint != fingerprint {
			return eventError("invalid_event_cursor", "cursor does not belong to this authenticated origin, target and agent", jobs.EventPage{})
		}
		cursor = supplied
	}
	deadline := time.Now().Add(time.Duration(input.WaitMS) * time.Millisecond)
	startingOriginSequence := cursor.OriginSequence
	for {
		page, expired := o.originEventPage(deviceID, agent, fingerprint, cursor.OriginSequence, input.Limit)
		if expired {
			// Recover by rereading the current snapshot and restarting this stream.
			cursor.OriginSequence = 0
			cursor.TargetCursor = ""
			page.NextCursor = encodeRemoteEventCursor(cursor)
			return eventError("cursor_expired", "origin event history was pruned; recover from snapshot and restart with next_cursor", page)
		}
		page.Snapshot = nil
		if len(page.Events) > 0 {
			cursor.OriginSequence = page.Events[len(page.Events)-1].Sequence
		}
		remaining := input.Limit - len(page.Events)
		if remaining == 0 {
			page.HasMore = true // The target stream has not yet been queried.
			page.NextCursor = encodeRemoteEventCursor(cursor)
			return provider.JSONResult(page)
		}
		waitMS := 0
		if len(page.Events) == 0 && input.WaitMS > 0 {
			waitMS = int(min(time.Until(deadline), time.Second) / time.Millisecond)
			waitMS = max(waitMS, 0)
		}
		targetResult, err := o.peerCall(ctx, deviceID, "job_events", mustJSON(map[string]any{"cursor": cursor.TargetCursor, "limit": remaining, "wait_ms": waitMS}), agent)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			page.Stale = true
			page.NextCursor = encodeRemoteEventCursor(cursor)
			return provider.JSONResult(page)
		}
		var targetPage struct {
			jobs.EventPage
			Code  string `json:"code"`
			Error string `json:"error"`
		}
		if len(targetResult.Content) == 0 {
			return eventError("invalid_event_response", "target returned no event page", page)
		}
		text, ok := targetResult.Content[0].(*mcp.TextContent)
		if !ok || json.Unmarshal([]byte(text.Text), &targetPage) != nil {
			if targetResult.IsError {
				return targetResult, nil
			}
			return eventError("invalid_event_response", "target returned an invalid event page", page)
		}
		if targetPage.Code == "cursor_expired" {
			cursor.OriginSequence = startingOriginSequence
			cursor.TargetCursor = ""
			page.Snapshot = targetPage.Snapshot
			page.NextCursor = encodeRemoteEventCursor(cursor)
			return eventError("cursor_expired", targetPage.Error, page)
		}
		if targetResult.IsError {
			return targetResult, nil
		}
		page.Events = append(page.Events, targetPage.Events...)
		page.HasMore = page.HasMore || targetPage.HasMore
		if targetPage.NextCursor != "" {
			cursor.TargetCursor = targetPage.NextCursor
		}
		page.NextCursor = encodeRemoteEventCursor(cursor)
		if len(page.Events) > 0 || input.WaitMS == 0 || !time.Now().Before(deadline) {
			return provider.JSONResult(page)
		}
	}
}
