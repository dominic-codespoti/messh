package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"messh/internal/provider"
	"messh/internal/state"
)

// failWrites is a one-shot, path-scoped atomic-writer failure. It delegates every
// successful operation to the real durable state writer.
type failWrites struct {
	mu        sync.Mutex
	match     func(string, []byte) bool
	remaining int
}

func (f *failWrites) write(path string, data []byte, perm os.FileMode) error {
	f.mu.Lock()
	if f.remaining > 0 && f.match(path, data) {
		f.remaining--
		f.mu.Unlock()
		return errors.New("injected state-write failure")
	}
	f.mu.Unlock()
	return state.WriteFileAtomic(path, data, perm)
}

func (f *failWrites) arm(n int, match func(string, []byte) bool) {
	f.mu.Lock()
	f.remaining, f.match = n, match
	f.mu.Unlock()
}

func newDurableProvider(t *testing.T, paths state.Paths, writer *failWrites, restore func(Job) (provider.Ticket, error)) *Provider {
	t.Helper()
	opts := Options{Paths: paths, Resources: &fakeRes{}, Inhibitor: &fakeInhibitor{}, NoScope: true, PollInterval: 10 * time.Millisecond, SettleTime: time.Millisecond, KillGrace: 100 * time.Millisecond, RestoreApproval: restore}
	if writer != nil {
		opts.WriteState = writer.write
	}
	p, err := New(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

func persistFixtureJob(t *testing.T, paths state.Paths, j Job) {
	t.Helper()
	dir := filepath.Join(paths.JobsDir(), j.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	if err := state.WriteFileAtomic(filepath.Join(dir, "job.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func durableBase(id string, st State) Job {
	now := time.Now().Add(-time.Minute)
	return Job{ID: id, Owner: Owner{DeviceID: agentA.DeviceID, Agent: agentA.Agent}, State: st, Submitted: now, Path: os.Args[0], Args: []string{"ORIGINAL"}, Env: map[string]string{"MESSH_JOBS_HELPER": "env"}, Workspace: "ws", Exact: "approved-exact", Approved: true}
}

func TestSubmissionPersistenceFailureCannotAcceptOrLaunch(t *testing.T) {
	paths := state.Paths{Root: t.TempDir()}
	wf := &failWrites{}
	p := newDurableProvider(t, paths, wf, nil)
	args := helperArgs("count", filepath.Join(paths.Root, "runs"))
	args["request_id"] = "persist-fails"
	raw := mustJSON(args)
	ap, err := p.Approval(context.Background(), toolSubmit, raw, agentA)
	if err != nil {
		t.Fatal(err)
	}
	wf.arm(1, func(path string, _ []byte) bool {
		return strings.Contains(path, filepath.Join("jobs", SubmissionID(agentA.DeviceID, agentA.Agent, "persist-fails")))
	})
	_, err = p.Call(provider.WithTicket(context.Background(), newTicket("persist-fail")), toolSubmit, raw, agentA)
	if err == nil {
		t.Fatal("submission succeeded despite job persistence failure")
	}
	time.Sleep(50 * time.Millisecond)
	if _, err := os.Stat(filepath.Join(paths.Root, "runs")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("job launched after failed commit; run marker stat err=%v", err)
	}
	if _, found, err := p.LookupSubmission(raw, agentA); err != nil || found {
		t.Fatalf("failed acceptance left a receipt: found=%v err=%v", found, err)
	}
	_ = ap
}

func TestSubmissionRetryDeduplicatesAndEnforcesOwnerAndPayload(t *testing.T) {
	h := newHarness(t, nil)
	args := helperArgs("count", filepath.Join(h.paths.Root, "runs"))
	args["request_id"] = "same-key"
	id, ticket := h.submit(args, agentA)
	ticket.Approve()
	id2, _ := h.submit(args, agentA)
	if id2 != id {
		t.Fatalf("lost-ack retry got %s, want %s", id2, id)
	}
	changed := helperArgs("count", filepath.Join(h.paths.Root, "other"))
	changed["request_id"] = "same-key"
	if _, _, err := h.trySubmit(changed, agentA); err == nil {
		t.Fatal("reused request_id with conflicting payload was accepted")
	}
	other := provider.Caller{DeviceID: "dev-b", DeviceName: "other", Agent: "omp"}
	otherArgs := helperArgs("echo")
	otherArgs["request_id"] = "same-key"
	otherID, otherTicket, err := h.trySubmit(otherArgs, other)
	if err != nil {
		t.Fatal(err)
	}
	if otherID == id {
		t.Fatal("request_id namespace was not owner-scoped")
	}
	otherTicket.Approve()
	deadline := time.Now().Add(5 * time.Second)
	var otherStatus Status
	for time.Now().Before(deadline) {
		otherStatus, err = h.p.statusOf(otherID, other, 5)
		if err == nil && otherStatus.State == StateSucceeded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if otherStatus.State != StateSucceeded {
		t.Fatalf("owner-isolated submission did not complete: %s", otherStatus.State)
	}
	if _, found, err := h.p.LookupSubmission(mustJSON(otherArgs), other); err != nil || !found {
		t.Fatalf("owner-specific receipt not found: found=%v err=%v", found, err)
	}
	st := h.waitState(id, StateSucceeded)
	if st.JobID != id {
		t.Fatalf("unexpected job %s", st.JobID)
	}
	b, err := os.ReadFile(filepath.Join(h.paths.Root, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(b), "run"); got != 1 {
		t.Fatalf("same-key calls launched %d executions", got)
	}
}

func TestSubmissionReceiptIsTombstonedAcrossDeletionAndRestart(t *testing.T) {
	h := newHarness(t, nil)
	args := helperArgs("echo")
	args["request_id"] = "delete-key"
	id, ticket := h.submit(args, agentA)
	ticket.Approve()
	h.waitState(id, StateSucceeded)
	if _, err := h.call(toolDelete, map[string]any{"job_id": id}, agentA); err != nil {
		t.Fatal(err)
	}
	h.p.Close()
	p := newDurableProvider(t, h.paths, nil, nil)
	_, found, err := p.LookupSubmission(mustJSON(args), agentA)
	if !found || err == nil {
		t.Fatalf("deleted submission was not reported as a tombstone: found=%v err=%v", found, err)
	}
}

func TestSubmissionReceiptSurvivesRestart(t *testing.T) {
	h := newHarness(t, nil)
	args := helperArgs("echo")
	args["request_id"] = "restart-key"
	id, ticket := h.submit(args, agentA)
	ticket.Approve()
	h.waitState(id, StateSucceeded)
	h.p.Close()
	p := newDurableProvider(t, h.paths, nil, nil)
	res, found, err := p.LookupSubmission(mustJSON(args), agentA)
	if err != nil || !found {
		t.Fatalf("durable receipt not found after restart: found=%v err=%v", found, err)
	}
	var out submitResult
	decodeResult(t, res, &out)
	if out.JobID != id {
		t.Fatalf("receipt resolved to %s, want original ID %s", out.JobID, id)
	}
}
func TestApprovedQueuedJobSurvivesRestartAndRunsOnce(t *testing.T) {
	paths := state.Paths{Root: t.TempDir()}
	marker := filepath.Join(paths.Root, "run-count")
	j := durableBase("queued-approved", StateQueued)
	j.Args = []string{marker}
	j.Env["MESSH_JOBS_HELPER"] = "count"
	j.Queued = &j.Submitted
	persistFixtureJob(t, paths, j)
	p := newDurableProvider(t, paths, nil, nil)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s, e := p.statusOf(j.ID, agentA, 5); e == nil && s.State == StateSucceeded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, err := p.statusOf(j.ID, agentA, 5)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateSucceeded {
		t.Fatalf("approved queued job did not resume: %s (%s)", st.State, st.Reason)
	}
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(b), "run") != 1 {
		t.Fatalf("expected exactly one launch, got %q", b)
	}
}

func TestSubmittedAndApprovalPendingRecoveryUsesTicketAndFreshApproval(t *testing.T) {
	paths := state.Paths{Root: t.TempDir()}
	sub := durableBase("submitted", StateSubmitted)
	sub.Approved = false
	request := helperArgs("echo")
	requestID := "submitted-request"
	request["request_id"] = requestID
	sub.ID = SubmissionID(agentA.DeviceID, agentA.Agent, requestID)
	sub.Submission = mustJSON(request)
	sub.RequestID = requestID
	var err error
	sub.SubmissionHash, err = SubmissionHash(sub.Submission)
	if err != nil {
		t.Fatal(err)
	}
	sub.Args = nil
	sub.Env = map[string]string{"MESSH_JOBS_HELPER": "echo"}
	approvalProvider := newDurableProvider(t, paths, nil, nil)
	approval, err := approvalProvider.Approval(context.Background(), toolSubmit, sub.Submission, agentA)
	if err != nil {
		t.Fatal(err)
	}
	sub.Exact = approval.Exact
	approvalProvider.Close()
	persistFixtureJob(t, paths, sub)
	pending := durableBase("pending", StateAwaitingApproval)
	pending.Approved = false
	persistFixtureJob(t, paths, pending)
	restored := newTicket("restored")
	fresh := newTicket("fresh")
	var mu sync.Mutex
	calls := map[string]int{}
	restore := func(j Job) (provider.Ticket, error) {
		mu.Lock()
		calls[j.ID]++
		mu.Unlock()
		if j.ID != sub.ID {
			return fresh, nil
		}
		if string(j.Submission) != string(sub.Submission) || j.RequestID != sub.RequestID || j.SubmissionHash != sub.SubmissionHash || j.Exact != sub.Exact || j.Path != sub.Path || strings.Join(j.Args, "|") != strings.Join(sub.Args, "|") || len(j.Inputs) != len(sub.Inputs) {
			return nil, errors.New("submitted request or approval metadata changed during recovery")
		}
		return restored, nil
	}
	p := newDurableProvider(t, paths, nil, restore)
	restored.Approve()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s, e := p.statusOf(sub.ID, agentA, 5)
		if e == nil && s.State == StateSucceeded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, err := p.statusOf(sub.ID, agentA, 5)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateSucceeded {
		t.Fatalf("submitted job not restored: %s %s", st.State, st.Reason)
	}
	mu.Lock()
	got := calls[sub.ID] + calls[pending.ID]
	mu.Unlock()
	if got != 2 {
		t.Fatalf("restore approval callback invoked %d times, want submitted+pending tickets", got)
	}
	if s, e := p.statusOf(pending.ID, agentA, 5); e != nil || s.State != StateAwaitingApproval {
		t.Fatalf("pending approval unexpectedly ran: state=%v err=%v", s.State, e)
	}
}

func TestRunningRecoveryUsesCommittedCheckpointAndPreservesIdentityAndArgv(t *testing.T) {
	paths := state.Paths{Root: t.TempDir()}
	j := durableBase("resume", StateRunning)
	j.Args = []string{"ORIGINAL", "unchanged"}
	j.Recovery = &Recovery{Checkpoint: "state.bin", Args: []string{"MESSH_RESUMED"}}
	j.Attempt = 2
	j.Workspace = "ws"
	checkpoint := []byte("committed-state")
	jobDir := filepath.Join(paths.JobsDir(), j.ID)
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(jobDir, "checkpoint-2.bin")
	if err := os.WriteFile(snapshot, checkpoint, 0o600); err != nil {
		t.Fatal(err)
	}
	j.CheckpointSnapshot = "checkpoint-2.bin"
	j.CheckpointSHA256 = sha(string(checkpoint))
	j.Started = &j.Submitted
	if err := os.WriteFile(filepath.Join(jobDir, "stdout.log"), []byte("before-crash\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	persistFixtureJob(t, paths, j)
	p := newDurableProvider(t, paths, nil, nil)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s, e := p.statusOf(j.ID, agentA, 10)
		if e == nil && s.State == StateSucceeded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, err := p.statusOf(j.ID, agentA, 10)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateSucceeded {
		t.Fatalf("checkpoint recovery failed: %s (%s)", st.State, st.Reason)
	}
	if st.Attempt != 3 {
		t.Fatalf("attempt=%d, want 3", st.Attempt)
	}
	if got := strings.Join(st.StdoutTail, "\n"); !strings.Contains(got, "1") || !strings.Contains(got, "before-crash") {
		t.Fatalf("recovery did not preserve prior logs and resumed output: %q", got)
	}
	stored, err := os.ReadFile(filepath.Join(jobDir, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	var after Job
	if err := json.Unmarshal(stored, &after); err != nil {
		t.Fatal(err)
	}
	if strings.Join(after.Args, "|") != "ORIGINAL|unchanged" {
		t.Fatalf("original argv mutated during resume: %q", after.Args)
	}
}

func TestInvalidCheckpointAndNonOptInRunningJobsNeverReplay(t *testing.T) {
	paths := state.Paths{Root: t.TempDir()}
	bad := durableBase("bad-checkpoint", StateRunning)
	bad.Recovery = &Recovery{Checkpoint: "state.bin", Args: []string{"MESSH_RESUMED"}}
	bad.CheckpointSnapshot = "snapshot.bin"
	bad.CheckpointSHA256 = sha("expected")
	badDir := filepath.Join(paths.JobsDir(), bad.ID)
	if err := os.MkdirAll(badDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, bad.CheckpointSnapshot), []byte("tampered"), 0o600); err != nil {
		t.Fatal(err)
	}
	persistFixtureJob(t, paths, bad)
	missing := durableBase("missing-checkpoint", StateRunning)
	missing.Recovery = &Recovery{Checkpoint: "state.bin", Args: []string{"MESSH_RESUMED"}}
	missing.CheckpointSnapshot = "absent.bin"
	missing.CheckpointSHA256 = sha("expected")
	persistFixtureJob(t, paths, missing)
	plain := durableBase("plain-running", StateRunning)
	plain.Recovery = nil
	plain.Args = []string{"count", filepath.Join(paths.Root, "must-not-run")}
	persistFixtureJob(t, paths, plain)
	p := newDurableProvider(t, paths, nil, nil)
	for _, id := range []string{bad.ID, missing.ID, plain.ID} {
		deadline := time.Now().Add(2 * time.Second)
		var st Status
		var err error
		for time.Now().Before(deadline) {
			st, err = p.statusOf(id, agentA, 5)
			if err == nil && st.State.Terminal() {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err != nil {
			t.Fatal(err)
		}
		if st.State == StateQueued || st.State == StateRunning {
			t.Fatalf("unsafe replay for %s: %s", id, st.State)
		}
	}
	if _, err := os.Stat(filepath.Join(paths.Root, "must-not-run")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ordinary command replayed, stat err=%v", err)
	}
}

func TestLifecycleCommitFailuresRetryWithoutLaunchingUncommittedState(t *testing.T) {
	paths := state.Paths{Root: t.TempDir()}
	wf := &failWrites{}
	marker := filepath.Join(paths.Root, "runs")
	var runningWriteWasSafe bool
	wf.arm(1, func(path string, data []byte) bool {
		if strings.HasSuffix(path, "job.json") && strings.Contains(string(data), `"state": "running"`) {
			_, err := os.Stat(marker)
			runningWriteWasSafe = errors.Is(err, os.ErrNotExist)
			return true
		}
		return false
	})
	p := newDurableProvider(t, paths, wf, nil)
	args := helperArgs("count", marker)
	raw := mustJSON(args)
	if _, err := p.Approval(context.Background(), toolSubmit, raw, agentA); err != nil {
		t.Fatal(err)
	}
	ticket := newTicket("commit-retry")
	res, err := p.Call(provider.WithTicket(context.Background(), ticket), toolSubmit, raw, agentA)
	if err != nil {
		t.Fatal(err)
	}
	var out submitResult
	decodeResult(t, res, &out)
	ticket.Approve()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, e := p.statusOf(out.JobID, agentA, 5)
		if e == nil && st.State == StateSucceeded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, err := p.statusOf(out.JobID, agentA, 5)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateSucceeded {
		t.Fatalf("job did not recover after running-state write failure: %s (%s)", st.State, st.Reason)
	}
	wf.mu.Lock()
	writeWasSafe := runningWriteWasSafe
	wf.mu.Unlock()
	if !writeWasSafe {
		t.Fatal("running-state write was not observed before the job became executable")
	}
	b, err := os.ReadFile(marker)
	if err != nil || strings.Count(string(b), "run") != 1 {
		t.Fatalf("job execution count after retry: %q, %v", b, err)
	}
}

func TestTerminalOutcomePersistenceFailureIsRetried(t *testing.T) {
	paths := state.Paths{Root: t.TempDir()}
	wf := &failWrites{}
	p := newDurableProvider(t, paths, wf, nil)
	args := helperArgs("echo")
	raw := mustJSON(args)
	if _, err := p.Approval(context.Background(), toolSubmit, raw, agentA); err != nil {
		t.Fatal(err)
	}
	ticket := newTicket("terminal-retry")
	res, err := p.Call(provider.WithTicket(context.Background(), ticket), toolSubmit, raw, agentA)
	if err != nil {
		t.Fatal(err)
	}
	var out submitResult
	decodeResult(t, res, &out)
	wf.arm(1, func(path string, data []byte) bool {
		return strings.HasSuffix(path, "job.json") && strings.Contains(string(data), `"state": "succeeded"`)
	})
	ticket.Approve()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, e := p.statusOf(out.JobID, agentA, 5)
		if e == nil && st.State == StateSucceeded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, err := p.statusOf(out.JobID, agentA, 5)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateSucceeded || st.Finished == nil {
		t.Fatalf("terminal outcome was lost after transient write failure: %+v", st)
	}
}

func TestRunningCheckpointIsPublishedAndRecoveredFromCrashState(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.PollInterval = 15 * time.Millisecond })
	args := helperArgs("checkpoint", "durable-running-checkpoint")
	args["recovery"] = map[string]any{"checkpoint": "state.bin", "args": []string{"resumed", "replacement-argv"}}
	id, ticket := h.submit(args, agentA)
	ticket.Approve()
	h.waitState(id, StateRunning)
	deadline := time.Now().Add(5 * time.Second)
	var committed Job
	for time.Now().Before(deadline) {
		h.p.mu.Lock()
		j := h.p.jobs[id]
		if j != nil {
			committed = j.Job
		}
		h.p.mu.Unlock()
		if committed.CheckpointSnapshot != "" {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if committed.CheckpointSnapshot == "" {
		t.Fatal("host did not publish a checkpoint while the child remained running")
	}
	b, err := os.ReadFile(filepath.Join(h.paths.JobsDir(), id, committed.CheckpointSnapshot))
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "durable-running-checkpoint" || committed.CheckpointSHA256 != sha(string(b)) {
		t.Fatalf("published checkpoint mismatch: %q hash=%s", b, committed.CheckpointSHA256)
	}
	h.p.Close()
	committed.State = StateRunning
	committed.Attempt = 0
	committed.PID = 0
	committed.PIDToken = ""
	committed.Unit = ""
	committed.Finished = nil
	committed.ExitCode = nil
	committed.Reason = ""
	persistFixtureJob(t, h.paths, committed)
	restarted := newDurableProvider(t, h.paths, nil, nil)
	deadline = time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s, e := restarted.statusOf(id, agentA, 10)
		if e == nil && s.State == StateSucceeded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, err := restarted.statusOf(id, agentA, 10)
	if err != nil {
		t.Fatal(err)
	}
	if st.JobID != id || st.State != StateSucceeded || st.Attempt != 1 {
		t.Fatalf("crash recovery identity/state/attempt: id=%s state=%s attempt=%d reason=%s", st.JobID, st.State, st.Attempt, st.Reason)
	}
	output := strings.Join(st.StdoutTail, "\n")
	if !strings.Contains(output, "before-crash") || !strings.Contains(output, "resumed-checkpoint:durable-running-checkpoint argv:resumed|replacement-argv") {
		t.Fatalf("recovery lost logs/checkpoint/replacement argv: %q", output)
	}
	stored, err := os.ReadFile(filepath.Join(h.paths.JobsDir(), id, "job.json"))
	if err != nil {
		t.Fatal(err)
	}
	var after Job
	if err := json.Unmarshal(stored, &after); err != nil {
		t.Fatal(err)
	}
	if strings.Join(after.Args, "|") != "durable-running-checkpoint" {
		t.Fatalf("original argv changed after recovery: %q", after.Args)
	}
}

func TestCancelledAndTimedOutJobsDoNotRecover(t *testing.T) {
	h := newHarness(t, nil)
	args := helperArgs("sleep")
	args["recovery"] = map[string]any{"checkpoint": "state.bin", "args": []string{"MESSH_RESUMED"}}
	args["timeout_seconds"] = 1
	id, ticket := h.submit(args, agentA)
	ticket.Approve()
	h.waitState(id, StateRunning)
	if _, err := h.call(toolCancel, map[string]any{"job_id": id}, agentA); err != nil {
		t.Fatal(err)
	}
	st := h.waitState(id, StateCancelled)
	if st.Attempt != 0 {
		t.Fatalf("cancelled job entered recovery attempt %d", st.Attempt)
	}
	paths := state.Paths{Root: t.TempDir()}
	expired := durableBase("expired", StateFailed)
	expired.Recovery = &Recovery{Checkpoint: "state.bin", Args: []string{"MESSH_RESUMED"}}
	expired.TimeoutSec = 1
	expired.Reason = "timeout"
	expired.Finished = &expired.Submitted
	persistFixtureJob(t, paths, expired)
	p := newDurableProvider(t, paths, nil, nil)
	got, err := p.statusOf(expired.ID, agentA, 5)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != StateFailed || got.Attempt != 0 {
		t.Fatalf("timed-out job was made resumable: state=%s attempt=%d", got.State, got.Attempt)
	}
}

func TestRecoveredTimeoutUsesOriginalBudget(t *testing.T) {
	paths := state.Paths{Root: t.TempDir()}
	j := durableBase("budget", StateRunning)
	j.TimeoutSec = 1
	j.Started = &j.Submitted
	j.Env["MESSH_JOBS_HELPER"] = "sleep"
	j.Recovery = &Recovery{Checkpoint: "state.bin", Args: []string{"unused"}}
	jobDir := filepath.Join(paths.JobsDir(), j.ID)
	if err := os.MkdirAll(jobDir, 0o700); err != nil {
		t.Fatal(err)
	}
	snapshot := filepath.Join(jobDir, "checkpoint.bin")
	if err := os.WriteFile(snapshot, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	j.CheckpointSnapshot = "checkpoint.bin"
	j.CheckpointSHA256 = sha("ok")
	persistFixtureJob(t, paths, j)
	p := newDurableProvider(t, paths, nil, nil)
	deadline := time.Now().Add(4 * time.Second)
	var got Status
	for time.Now().Before(deadline) {
		got, _ = p.statusOf(j.ID, agentA, 5)
		if got.State.Terminal() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got.State != StateFailed || !strings.Contains(got.Reason, "timed out") {
		t.Fatalf("recovered job received a fresh timeout budget: %s (%s)", got.State, got.Reason)
	}
}

func TestConcurrentSameKeyCallsCommitOnlyOneJob(t *testing.T) {
	h := newHarness(t, nil)
	marker := filepath.Join(h.paths.Root, "concurrent-runs")
	args := helperArgs("count", marker)
	args["request_id"] = "concurrent-key"
	raw := mustJSON(args)
	tickets := make([]*fakeTicket, 2)
	contexts := make([]context.Context, 2)
	for i := range tickets {
		if _, err := h.p.Approval(context.Background(), toolSubmit, raw, agentA); err != nil {
			t.Fatal(err)
		}
		tickets[i] = newTicket("concurrent")
		contexts[i] = provider.WithTicket(context.Background(), tickets[i])
	}
	var wg sync.WaitGroup
	ids := make([]string, 2)
	errs := make([]error, 2)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := h.p.Call(contexts[i], toolSubmit, raw, agentA)
			if err != nil {
				errs[i] = err
				return
			}
			var out submitResult
			b, err := json.Marshal(res.StructuredContent)
			if err == nil {
				err = json.Unmarshal(b, &out)
			}
			errs[i] = err
			ids[i] = out.JobID
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent submit %d: %v", i, err)
		}
		tickets[i].Approve()
	}
	if ids[0] == "" || ids[0] != ids[1] {
		t.Fatalf("concurrent same-key calls returned %q and %q", ids[0], ids[1])
	}
	h.waitState(ids[0], StateSucceeded)
	b, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(b), "run") != 1 {
		t.Fatalf("concurrent submissions launched more than once: %q", b)
	}
}

func TestSubmissionReceiptWriteFailureCannotLeaveAcceptedJob(t *testing.T) {
	paths := state.Paths{Root: t.TempDir()}
	wf := &failWrites{}
	p := newDurableProvider(t, paths, wf, nil)
	marker := filepath.Join(paths.Root, "should-not-run")
	args := helperArgs("count", marker)
	args["request_id"] = "receipt-failure"
	raw := mustJSON(args)
	if _, err := p.Approval(context.Background(), toolSubmit, raw, agentA); err != nil {
		t.Fatal(err)
	}
	wf.arm(1, func(path string, _ []byte) bool { return strings.Contains(path, ".submission-") })
	if _, err := p.Call(provider.WithTicket(context.Background(), newTicket("receipt-failure")), toolSubmit, raw, agentA); err == nil {
		t.Fatal("submission accepted without a durable receipt")
	}
	id := SubmissionID(agentA.DeviceID, agentA.Agent, "receipt-failure")
	if _, err := p.statusOf(id, agentA, 5); err == nil {
		t.Fatal("job remained visible after its receipt commit failed")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("uncommitted job launched: %v", err)
	}
}

func TestUnapprovedQueuedJobCannotRunAfterRestart(t *testing.T) {
	paths := state.Paths{Root: t.TempDir()}
	marker := filepath.Join(paths.Root, "unapproved-run")
	j := durableBase("queued-unapproved", StateQueued)
	j.Approved = false
	j.Args = []string{"count", marker}
	j.Env["MESSH_JOBS_HELPER"] = "count"
	persistFixtureJob(t, paths, j)
	_ = newDurableProvider(t, paths, nil, nil)
	time.Sleep(150 * time.Millisecond)
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unapproved queued command executed: %v", err)
	}
}

func TestApprovalQueueCommitFailureRetries(t *testing.T) {
	paths := state.Paths{Root: t.TempDir()}
	wf := &failWrites{}
	marker := filepath.Join(paths.Root, "approved-run")
	p := newDurableProvider(t, paths, wf, nil)
	args := helperArgs("count", marker)
	raw := mustJSON(args)
	if _, err := p.Approval(context.Background(), toolSubmit, raw, agentA); err != nil {
		t.Fatal(err)
	}
	ticket := newTicket("queue-retry")
	res, err := p.Call(provider.WithTicket(context.Background(), ticket), toolSubmit, raw, agentA)
	if err != nil {
		t.Fatal(err)
	}
	var out submitResult
	decodeResult(t, res, &out)
	wf.arm(1, func(path string, data []byte) bool {
		return strings.HasSuffix(path, "job.json") && strings.Contains(string(data), "\"state\": \"queued\"") && strings.Contains(string(data), "\"approved\": true")
	})
	ticket.Approve()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, e := p.statusOf(out.JobID, agentA, 5)
		if e == nil && st.State == StateSucceeded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	st, err := p.statusOf(out.JobID, agentA, 5)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != StateSucceeded {
		t.Fatalf("approval queue transition was lost: %s (%s)", st.State, st.Reason)
	}
	b, err := os.ReadFile(marker)
	if err != nil || strings.Count(string(b), "run") != 1 {
		t.Fatalf("approved command count=%q err=%v", b, err)
	}
}

func TestPIDCommitFailureCleansUpStartedProcess(t *testing.T) {
	paths := state.Paths{Root: t.TempDir()}
	wf := &failWrites{}
	var pid int
	wf.arm(1, func(path string, data []byte) bool {
		if strings.HasSuffix(path, "job.json") {
			var j Job
			if json.Unmarshal(data, &j) == nil && j.PID > 0 {
				pid = j.PID
				return true
			}
		}
		return false
	})
	p := newDurableProvider(t, paths, wf, nil)
	args := helperArgs("sleep")
	args["timeout_seconds"] = 2
	raw := mustJSON(args)
	if _, err := p.Approval(context.Background(), toolSubmit, raw, agentA); err != nil {
		t.Fatal(err)
	}
	ticket := newTicket("pid-commit")
	res, err := p.Call(provider.WithTicket(context.Background(), ticket), toolSubmit, raw, agentA)
	if err != nil {
		t.Fatal(err)
	}
	var out submitResult
	decodeResult(t, res, &out)
	ticket.Approve()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		st, e := p.statusOf(out.JobID, agentA, 5)
		if e == nil && st.State.Terminal() {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	wf.mu.Lock()
	observedPID := pid
	wf.mu.Unlock()
	if observedPID == 0 {
		t.Fatal("test did not observe a PID commit")
	}
	if pidAlive(observedPID) {
		t.Fatalf("process %d survived failed PID persistence", observedPID)
	}
}

func TestCancelPersistenceFailureIsNotAcknowledged(t *testing.T) {
	paths := state.Paths{Root: t.TempDir()}
	wf := &failWrites{}
	p := newDurableProvider(t, paths, wf, nil)
	args := helperArgs("echo")
	raw := mustJSON(args)
	if _, err := p.Approval(context.Background(), toolSubmit, raw, agentA); err != nil {
		t.Fatal(err)
	}
	ticket := newTicket("cancel-commit")
	res, err := p.Call(provider.WithTicket(context.Background(), ticket), toolSubmit, raw, agentA)
	if err != nil {
		t.Fatal(err)
	}
	var out submitResult
	decodeResult(t, res, &out)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		st, e := p.statusOf(out.JobID, agentA, 5)
		if e == nil && st.State == StateAwaitingApproval {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	wf.arm(100, func(path string, data []byte) bool {
		return strings.HasSuffix(path, "job.json") && strings.Contains(string(data), "\"state\": \"cancelled\"")
	})
	cancelled, callErr := p.Call(context.Background(), toolCancel, mustJSON(map[string]any{"job_id": out.JobID}), agentA)
	if callErr == nil && (cancelled == nil || !cancelled.IsError) {
		t.Fatal("cancel returned a positive acknowledgement while its durable state writes failed")
	}
}

func TestUnapprovedInputSnapshotSurvivesRestartAndRequiresFreshApproval(t *testing.T) {
	h := newHarness(t, nil)
	source := filepath.Join(h.paths.Root, "ws", "source", "input.txt")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("original staged input"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := helperArgs("cat", "input.txt")
	args["request_id"] = "staged-input-recovery"
	args["inputs"] = []string{"ws/source/input.txt"}
	raw := mustJSON(args)
	expectedSubmissionHash, err := SubmissionHash(raw)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := h.submit(args, agentA)
	before := h.waitState(id, StateAwaitingApproval)
	if len(before.Inputs) != 1 || before.Inputs[0].SHA256 != sha("original staged input") {
		t.Fatalf("staged input metadata before restart = %+v", before.Inputs)
	}
	staged := filepath.Join(h.paths.Root, "ws", id, "input.txt")
	if b, err := os.ReadFile(staged); err != nil || string(b) != "original staged input" {
		t.Fatalf("staged input before restart = %q, err=%v", b, err)
	}
	if err := os.WriteFile(source, []byte("replacement source"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.p.Close()

	fresh := newTicket("fresh-input-approval")
	restored := newDurableProvider(t, h.paths, nil, func(j Job) (provider.Ticket, error) {
		submissionHash, hashErr := SubmissionHash(j.Submission)
		if j.ID != id || hashErr != nil || submissionHash != expectedSubmissionHash || j.RequestID != "staged-input-recovery" || len(j.Inputs) != 1 || j.Inputs[0].SHA256 != sha("original staged input") || j.Exact == "" {
			return nil, errors.New("unapproved staged request changed during restart")
		}
		return fresh, nil
	})
	status, err := restored.statusOf(id, agentA, 10)
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateAwaitingApproval {
		t.Fatalf("unapproved request state after restart = %s", status.State)
	}
	if b, err := os.ReadFile(staged); err != nil || string(b) != "original staged input" {
		t.Fatalf("staged snapshot changed across restart = %q, err=%v", b, err)
	}
	fresh.Approve()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status, err = restored.statusOf(id, agentA, 10)
		if err == nil && status.State == StateSucceeded {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	if status.State != StateSucceeded || !strings.Contains(strings.Join(status.StdoutTail, "\n"), "original staged input") {
		t.Fatalf("fresh approval did not run from the persisted snapshot: state=%s output=%q", status.State, status.StdoutTail)
	}
}
