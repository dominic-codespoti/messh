package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type tombstoneJournal struct {
	Events  []Event           `json:"events"`
	Dropped map[string]uint64 `json:"dropped"`
}

func (p *Provider) persistTombstonesLocked() error {
	b, err := json.MarshalIndent(tombstoneJournal{Events: p.tombstoneEvents, Dropped: p.tombstoneDropped}, "", "  ")
	if err != nil {
		return err
	}
	return p.writeState(filepath.Join(p.paths.JobsDir(), "event-tombstones.json"), b, 0o600)
}
func (p *Provider) appendDeletionEventLocked(j *job) error {
	oldEvents := p.tombstoneEvents
	oldDropped := make(map[string]uint64, len(p.tombstoneDropped))
	for k, v := range p.tombstoneDropped {
		oldDropped[k] = v
	}
	p.nextEventSeq++
	if j.EventDroppedThrough > p.tombstoneDropped[ownerKey(j.Owner)] {
		p.tombstoneDropped[ownerKey(j.Owner)] = j.EventDroppedThrough
	}
	e := Event{ID: fmt.Sprintf("%s:%d", j.ID, p.nextEventSeq), Sequence: p.nextEventSeq, Source: "target", JobID: j.ID, Owner: j.Owner, Type: "deleted", State: j.State, At: time.Now().UTC(), Reason: j.Reason}
	p.tombstoneEvents = append(p.tombstoneEvents, e)
	for len(p.tombstoneEvents) > eventRetention {
		old := p.tombstoneEvents[0]
		k := ownerKey(old.Owner)
		if old.Sequence > p.tombstoneDropped[k] {
			p.tombstoneDropped[k] = old.Sequence
		}
		p.tombstoneEvents = p.tombstoneEvents[1:]
	}
	if err := p.persistTombstonesLocked(); err != nil {
		p.tombstoneEvents = oldEvents
		p.tombstoneDropped = oldDropped
		p.nextEventSeq--
		return err
	}
	close(p.eventWake)
	p.eventWake = make(chan struct{})
	return nil
}
func (p *Provider) loadTombstones() error {
	b, err := os.ReadFile(filepath.Join(p.paths.JobsDir(), "event-tombstones.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var j tombstoneJournal
	if err = json.Unmarshal(b, &j); err != nil {
		return fmt.Errorf("decode job event tombstones: %w", err)
	}
	p.tombstoneEvents = j.Events
	p.tombstoneDropped = j.Dropped
	if p.tombstoneDropped == nil {
		p.tombstoneDropped = map[string]uint64{}
	}
	if len(p.tombstoneEvents) > eventRetention {
		p.tombstoneEvents = append([]Event(nil), p.tombstoneEvents[len(p.tombstoneEvents)-eventRetention:]...)
	}
	for _, e := range p.tombstoneEvents {
		if e.Sequence > p.nextEventSeq {
			p.nextEventSeq = e.Sequence
		}
	}
	for _, n := range p.tombstoneDropped {
		if n > p.nextEventSeq {
			p.nextEventSeq = n
		}
	}
	return nil
}
