package jobs

import (
	"errors"
	"time"
)

// commitAwaitingApproval keeps accepted work restartable when storage is
// temporarily unavailable. The state remains Submitted until the durable
// awaiting transition succeeds, so observers cannot see an uncommitted phase.
func (p *Provider) commitAwaitingApproval(j *job) bool {
	for {
		p.mu.Lock()
		if j.State.Terminal() || p.ctx.Err() != nil {
			p.mu.Unlock()
			return false
		}
		oldState := j.State
		j.State = StateAwaitingApproval
		if err := p.touchLocked(j); err == nil {
			p.mu.Unlock()
			return true
		} else {
			j.State = oldState
			j.waiting = "awaiting approval state could not be persisted: " + err.Error()
			p.mu.Unlock()
		}
		if !p.retryTransition() {
			return false
		}
	}
}

// commitApproval retries a consumed positive decision as a pending commit.
// Approved and Queued become visible only once their joint transition is
// durably recorded; shutdown leaves the old unapproved record intact.
func (p *Provider) commitApproval(j *job) bool {
	for {
		p.mu.Lock()
		if j.State.Terminal() || p.ctx.Err() != nil {
			p.mu.Unlock()
			return false
		}
		oldState, oldQueued, oldApproved := j.State, j.Queued, j.Approved
		now := time.Now()
		p.nextSeq++
		j.seq = p.nextSeq
		j.State, j.Queued, j.Approved = StateQueued, &now, true
		if err := p.touchLocked(j); err == nil {
			j.waiting = ""
			p.kick()
			p.mu.Unlock()
			return true
		} else {
			j.State, j.Queued, j.Approved = oldState, oldQueued, oldApproved
			j.waiting = "approval received but queue state could not be persisted: " + err.Error()
			p.mu.Unlock()
		}
		if !p.retryTransition() {
			return false
		}
	}
}

func (p *Provider) retryTransition() bool {
	t := time.NewTimer(100 * time.Millisecond)
	defer t.Stop()
	select {
	case <-p.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// commitKillIntentLocked makes cancellation or timeout durable before signalling
// the process. It enters and returns with Provider.mu held.
func (p *Provider) commitKillIntentLocked(j *job, reason string, kind killKind) error {
	for {
		if j.State != StateRunning || j.kill != killNone {
			return nil
		}
		oldReason := j.Reason
		j.Reason = reason
		if err := p.touchLocked(j); err == nil {
			j.kill = kind
			return nil
		} else {
			j.Reason = oldReason
			j.waiting = reason + " could not be persisted: " + err.Error()
		}
		p.mu.Unlock()
		if !p.retryTransition() {
			p.mu.Lock()
			return errors.New(j.waiting)
		}
		p.mu.Lock()
		if p.closing {
			return errors.New("node is shutting down before kill intent could be persisted")
		}
	}
}
