package jobs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"messh/internal/provider"
)

const eventRetention = 128

type Event struct {
	ID       string    `json:"id"`
	Sequence uint64    `json:"sequence"`
	Source   string    `json:"source"`
	JobID    string    `json:"job_id"`
	Owner    Owner     `json:"owner"`
	Type     string    `json:"type"`
	State    State     `json:"state"`
	At       time.Time `json:"at"`
	Reason   string    `json:"reason,omitempty"`
}
type EventPage struct {
	Events     []Event   `json:"events"`
	NextCursor string    `json:"next_cursor,omitempty"`
	HasMore    bool      `json:"has_more"`
	Snapshot   []Summary `json:"snapshot,omitempty"`
	Stale      bool      `json:"stale,omitempty"`
}
type CursorExpiredError struct {
	Snapshot []Summary `json:"snapshot"`
}

func (e *CursorExpiredError) Error() string {
	return "job event cursor expired; recover from the included owner snapshot"
}

type eventCursor struct {
	DeviceID string `json:"d"`
	Agent    string `json:"a"`
	Sequence uint64 `json:"s"`
}

func encodeEventCursor(o Owner, n uint64) string {
	b, _ := json.Marshal(eventCursor{DeviceID: o.DeviceID, Agent: o.Agent, Sequence: n})
	return base64.RawURLEncoding.EncodeToString(b)
}

// Events returns durable lifecycle events owned by caller in sequence order.
// Cursor identity deliberately excludes the mutable display name.
func (p *Provider) Events(ctx context.Context, caller provider.Caller, cursor string, limit int, wait time.Duration) (EventPage, error) {
	if limit <= 0 || limit > eventRetention {
		limit = eventRetention
	}
	if wait < 0 {
		return EventPage{}, errors.New("wait must not be negative")
	}
	if wait > 30*time.Second {
		wait = 30 * time.Second
	}
	var after uint64
	if cursor != "" {
		var c eventCursor
		b, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(b, &c) != nil || c.DeviceID != caller.DeviceID || c.Agent != caller.Agent {
			return EventPage{}, errors.New("invalid owner-scoped event cursor")
		}
		after = c.Sequence
	}
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		page, expired := p.eventPage(caller, after, limit)
		if expired {
			return page, &CursorExpiredError{Snapshot: page.Snapshot}
		}
		if len(page.Events) > 0 || wait == 0 {
			return page, nil
		}
		p.mu.Lock()
		wake := p.eventWake
		p.mu.Unlock()
		select {
		case <-ctx.Done():
			return EventPage{}, ctx.Err()
		case <-wake:
			continue
		case <-deadline.C:
			return page, nil
		}
	}
}
func (p *Provider) eventPage(caller provider.Caller, after uint64, limit int) (EventPage, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var owned []Event
	var snapshot []Summary
	var dropped uint64
	for _, j := range p.jobs {
		if !j.Owner.same(caller) || j.deleting {
			continue
		}
		snapshot = append(snapshot, summaryOf(&j.Job))
		if j.EventDroppedThrough > dropped {
			dropped = j.EventDroppedThrough
		}
		owned = append(owned, j.Events...)
	}
	for _, e := range p.tombstoneEvents {
		if e.Owner.same(caller) {
			owned = append(owned, e)
		}
	}
	if d := p.tombstoneDropped[ownerKey(ownerOf(caller))]; d > dropped {
		dropped = d
	}
	if after < dropped {
		return EventPage{Snapshot: snapshot, Stale: true}, true
	}
	sort.Slice(owned, func(i, j int) bool { return owned[i].Sequence < owned[j].Sequence })
	page := EventPage{Events: make([]Event, 0)}
	for _, e := range owned {
		if e.Sequence <= after {
			continue
		}
		if len(page.Events) == limit {
			page.HasMore = true
			break
		}
		page.Events = append(page.Events, e)
	}
	if len(page.Events) > 0 {
		page.NextCursor = encodeEventCursor(ownerOf(caller), page.Events[len(page.Events)-1].Sequence)
	} else if after > 0 {
		page.NextCursor = encodeEventCursor(ownerOf(caller), after)
	}
	return page, false
}
func (p *Provider) appendEventLocked(j *job, typ string) error {
	oldEvents := j.Events
	oldDropped := j.EventDroppedThrough
	oldState := j.eventState
	p.nextEventSeq++
	e := Event{ID: fmt.Sprintf("%s:%d", j.ID, p.nextEventSeq), Sequence: p.nextEventSeq, Source: "target", JobID: j.ID, Owner: j.Owner, Type: typ, State: j.State, At: time.Now().UTC(), Reason: j.Reason}
	j.Events = append(j.Events, e)
	if len(j.Events) > eventRetention {
		n := len(j.Events) - eventRetention
		if j.Events[n-1].Sequence > j.EventDroppedThrough {
			j.EventDroppedThrough = j.Events[n-1].Sequence
		}
		j.Events = append([]Event(nil), j.Events[n:]...)
	}
	j.eventState = j.State
	if err := p.persistLocked(j); err != nil {
		j.Events = oldEvents
		j.EventDroppedThrough = oldDropped
		j.eventState = oldState
		p.nextEventSeq--
		return err
	}
	close(p.eventWake)
	p.eventWake = make(chan struct{})
	close(j.changed)
	j.changed = make(chan struct{})
	return nil
}
