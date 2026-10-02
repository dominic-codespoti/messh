package jobs

import "testing"

func TestActiveCountIncludesEveryOwnerAndNonterminalState(t *testing.T) {
	p := &Provider{jobs: make(map[string]*job)}
	for _, state := range []State{StateSubmitted, StateAwaitingApproval, StateQueued, StateRunning} {
		p.jobs[string(state)] = &job{Job: Job{ID: string(state), State: state, Owner: Owner{DeviceID: "foreign-device", Agent: string(state)}}}
	}
	for _, state := range []State{StateSucceeded, StateFailed, StateCancelled, StateInterrupted} {
		p.jobs[string(state)] = &job{Job: Job{ID: string(state), State: state, Owner: Owner{DeviceID: "local-device", Agent: "cli"}}}
	}
	if got := p.ActiveCount(); got != 4 {
		t.Fatalf("active jobs across owners = %d, want 4", got)
	}
	for _, j := range p.jobs {
		j.State = StateCancelled
	}
	if got := p.ActiveCount(); got != 0 {
		t.Fatalf("terminal jobs block maintenance: %d", got)
	}
}
