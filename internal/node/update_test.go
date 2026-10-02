package node

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/control"
	"messh/internal/identity"
	"messh/internal/provider"
	"messh/internal/schedule"
	"messh/internal/state"
)

func maintenanceNode() *Node {
	return &Node{id: &identity.Identity{ID: "test-node"}, executable: "test-executable"}
}

func TestUpdateCountsScheduledCallsWaitingForCapacity(t *testing.T) {
	n := maintenanceNode()
	n.ctx, n.cancel = context.WithCancel(t.Context())
	defer n.cancel()
	n.log = slog.New(slog.DiscardHandler)
	path := filepath.Join(t.TempDir(), "schedules.json")
	store, err := schedule.Open(path, schedule.Options{})
	if err != nil {
		t.Fatal(err)
	}
	stored, err := store.Add(schedule.Schedule{
		Agent: "foreign-agent", Device: n.ID(), DeviceName: "host", Tool: "node_info", Spec: schedule.Spec{In: "1h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sc := &scheduler{n: n, store: store, sem: make(chan struct{}, 1)}
	sc.sem <- struct{}{}
	if !n.beginWork() {
		t.Fatal("claim admission refused")
	}
	fire, err := store.Claim("", stored.ID)
	if err != nil {
		t.Fatal(err)
	}
	sc.start(fire)
	n.endWork()
	if _, err := n.prepareUpdate(time.Now()); err == nil {
		t.Fatal("scheduled call waiting for capacity did not block maintenance")
	}
	n.cancel()
	n.wg.Wait()
	record, err := store.Get("", stored.ID)
	if err != nil || !record.Running || len(record.Runs) != 0 {
		t.Fatalf("canceled scheduled fire lost its durable in-flight identity: %+v (%v)", record, err)
	}
	recovered, err := schedule.Open(path, schedule.Options{})
	if err != nil {
		t.Fatal(err)
	}
	record, err = recovered.Get("", stored.ID)
	if err != nil || record.Running || len(record.Runs) != 1 || record.Runs[0].OK || !strings.Contains(record.Runs[0].Error, "not replayed") {
		t.Fatalf("restart did not classify the ambiguous ordinary fire without replay: %+v (%v)", record, err)
	}
	reopened, err := schedule.Open(path, schedule.Options{})
	if err != nil {
		t.Fatal(err)
	}
	record, err = reopened.Get("", stored.ID)
	if err != nil || record.Running || len(record.Runs) != 1 {
		t.Fatalf("classified fire was replayed again after restart: %+v (%v)", record, err)
	}
	lease, err := n.prepareUpdate(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if sc.runDue() {
		t.Fatal("scheduler admitted a claim while maintenance was held")
	}
	if !n.consumeUpdateLease(lease.Lease, false, time.Now()) || !sc.runDue() {
		t.Fatal("scheduler did not resume after maintenance abort")
	}
}

func TestUpdateLeaseDefersAcceptedOutboxDelivery(t *testing.T) {
	b := startGateNode(t, "target", &scriptSurface{})
	a := startGateNode(t, "source", &scriptSurface{})
	pair(t, b, a)
	if _, err := a.paths.AddAgent("agent"); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{}, 1)
	release := make(chan struct{}, 1)
	defer func() {
		select {
		case release <- struct{}{}:
		default:
		}
	}()
	var first atomic.Bool
	first.Store(true)
	var deliverAfterAbort atomic.Bool
	a.remoteJobs.setPeerCall(func(ctx context.Context, id, tool string, args json.RawMessage, agent string) (*mcp.CallToolResult, error) {
		if tool == "job_submit" && first.CompareAndSwap(true, false) {
			entered <- struct{}{}
			select {
			case <-release:
				return nil, errors.New("simulated transport failure before target delivery")
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if tool == "job_submit" && !deliverAfterAbort.Load() {
			return nil, errors.New("target delivery withheld until lease abort")
		}
		return a.peers.callRaw(ctx, id, tool, args, agent)
	})
	args := json.RawMessage(`{"command":"echo maintenance","shell":true,"request_id":"maintenance-held-delivery"}`)
	accepted, err := a.Call(t.Context(), b.ID(), "job_submit", args, "agent")
	if err != nil {
		t.Fatal(err)
	}
	var submission struct {
		JobID string `json:"job_id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(resultText(accepted)), &submission); err != nil {
		t.Fatal(err)
	}
	if submission.JobID == "" || submission.State != "pending_delivery" {
		t.Fatalf("remote submission was not durably queued: %s", resultText(accepted))
	}
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("outbox did not start its admitted delivery attempt")
	}
	if _, err := a.prepareUpdate(time.Now()); err == nil || !strings.Contains(err.Error(), "active requests") {
		t.Fatalf("update preparation did not account for the active outbox delivery: %v", err)
	}
	release <- struct{}{}
	waitFor(t, "outbox retry scheduled and attempt released", func() bool {
		a.remoteJobs.mu.Lock()
		defer a.remoteJobs.mu.Unlock()
		record := a.remoteJobs.records[submission.JobID]
		return record != nil && record.Attempt > 0 && record.NextTry.After(time.Now()) && !a.remoteJobs.busy[submission.JobID]
	})
	var lease control.UpdatePreparation
	waitFor(t, "outbox attempt to drain before update lease", func() bool {
		var prepareErr error
		lease, prepareErr = a.prepareUpdate(time.Now())
		return prepareErr == nil
	})
	a.remoteJobs.mu.Lock()
	queued := a.remoteJobs.records[submission.JobID]
	if queued == nil {
		a.remoteJobs.mu.Unlock()
		t.Fatal("accepted outbox record disappeared")
	}
	queued.NextTry = time.Now().Add(-time.Second)
	queued.Updated = time.Now()
	before := *queued
	err = a.remoteJobs.saveLocked()
	a.remoteJobs.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	a.remoteJobs.signal()
	a.remoteJobs.deliver(submission.JobID)
	timer := time.NewTimer(1200 * time.Millisecond)
	select {
	case <-timer.C:
	case <-t.Context().Done():
		timer.Stop()
		t.Fatal(t.Context().Err())
	}
	after := a.remoteJobs.find(b.ID(), "agent", submission.JobID)
	if after == nil || after.State != "pending_delivery" || after.Attempt != before.Attempt || !after.NextTry.Equal(before.NextTry) || after.Cancel || after.Delete {
		t.Fatalf("refused admission changed accepted outbox work: before=%+v after=%+v", before, after)
	}
	if got := len(b.approvals.Pending()); got != 0 {
		t.Fatalf("target received a job through the maintenance lease: approval tickets=%d", got)
	}
	if !a.consumeUpdateLease(lease.Lease, false, time.Now()) {
		t.Fatal("could not abort update lease")
	}
	deliverAfterAbort.Store(true)
	a.remoteJobs.signal()
	waitFor(t, "accepted job delivered after lease abort", func() bool {
		record := a.remoteJobs.find(b.ID(), "agent", submission.JobID)
		return record != nil && record.State == "awaiting_approval" && len(b.approvals.Pending()) == 1
	})
	record := a.remoteJobs.find(b.ID(), "agent", submission.JobID)
	if record == nil || record.ID != submission.JobID {
		t.Fatalf("target acceptance changed durable job identity: %+v", record)
	}
}

func TestUpdateLeaseAdmissionExpiryAndAbort(t *testing.T) {
	n := maintenanceNode()
	now := time.Now()
	if !n.beginWork() {
		t.Fatal("initial admission rejected")
	}
	if _, err := n.prepareUpdate(now); err == nil {
		t.Fatal("prepared while work was admitted")
	}
	n.endWork()
	lease, err := n.prepareUpdate(now)
	if err != nil {
		t.Fatal(err)
	}
	if n.beginWork() {
		t.Fatal("admitted work during lease")
	}
	if n.consumeUpdateLease("foreign", true, now) || n.consumeUpdateLease("foreign", false, now) {
		t.Fatal("foreign lease changed maintenance")
	}
	if !n.consumeUpdateLease(lease.Lease, false, now) || !n.beginWork() {
		t.Fatal("abort did not reopen admission")
	}
	n.endWork()
	lease, err = n.prepareUpdate(now.Add(-updateLeaseDuration))
	if err != nil {
		t.Fatal(err)
	}
	if n.consumeUpdateLease(lease.Lease, true, now) {
		t.Fatal("expired lease authorized shutdown")
	}
	if !n.beginWork() {
		t.Fatal("expired lease blocked admission")
	}
	n.endWork()
	lease, err = n.prepareUpdate(now)
	if err != nil || !n.consumeUpdateLease(lease.Lease, true, now) {
		t.Fatalf("stop: %v", err)
	}
	n.maintenance.expires = now.Add(-time.Hour)
	if n.beginWork() {
		t.Fatal("stopping node reopened on expiry")
	}
}

func TestUpdateAdmissionRace(t *testing.T) {
	// Keep an admitted operation live until prepare has answered: there is no
	// legal outcome in which both preparation and admission succeed.
	for range 100 {
		n := maintenanceNode()
		start := make(chan struct{})
		admitted := make(chan bool, 1)
		go func() {
			<-start
			admitted <- n.beginWork()
		}()
		close(start)
		lease, err := n.prepareUpdate(time.Now())
		got := <-admitted
		if got == (err == nil) {
			t.Fatalf("admitted=%v prepare error=%v", got, err)
		}
		if got {
			n.endWork()
			if _, err := n.prepareUpdate(time.Now()); err != nil {
				t.Fatalf("failed preparation retained a hold: %v", err)
			}
		} else if !n.consumeUpdateLease(lease.Lease, false, time.Now()) {
			t.Fatal("could not release winning lease")
		}
	}
}

func TestUpdateRacesRealJobSubmission(t *testing.T) {
	n := startGateNode(t, "host", &scriptSurface{})
	caller := provider.Caller{DeviceID: "foreign-device", Agent: "worker"}
	for range 16 {
		start := make(chan struct{})
		result := make(chan *mcp.CallToolResult, 1)
		go func() {
			<-start
			result <- n.dispatch(t.Context(), "job_submit", json.RawMessage(`{"command":"echo must-not-run","shell":true}`), caller)
		}()
		close(start)
		lease, err := n.prepareUpdate(time.Now())
		res := <-result
		if err == nil {
			if !res.IsError {
				t.Fatal("job committed after maintenance acquired admission")
			}
			if !n.consumeUpdateLease(lease.Lease, false, time.Now()) {
				t.Fatal("could not abort lease")
			}
			continue
		}
		if res.IsError {
			t.Fatalf("failed preparation disrupted submission: %s", resultText(res))
		}
		var submitted struct {
			JobID string `json:"job_id"`
		}
		if err := json.Unmarshal([]byte(resultText(res)), &submitted); err != nil {
			t.Fatal(err)
		}
		if _, err := n.prepareUpdate(time.Now()); err == nil {
			t.Fatal("committed job no longer blocked update")
		}
		args, _ := json.Marshal(map[string]string{"job_id": submitted.JobID})
		if res := n.dispatch(t.Context(), "job_cancel", args, caller); res.IsError {
			t.Fatalf("owner could not cancel after failed preparation: %s", resultText(res))
		}
	}
}

func TestUpdateRefusesForeignPendingJob(t *testing.T) {
	n := startGateNode(t, "host", &scriptSurface{})
	caller := provider.Caller{DeviceID: "other-device", Agent: "other-agent"}
	res := n.dispatch(t.Context(), "job_submit", json.RawMessage(`{"command":"echo must-not-run","shell":true}`), caller)
	if res.IsError {
		t.Fatalf("submit: %s", resultText(res))
	}
	var submitted struct {
		JobID string `json:"job_id"`
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(resultText(res)), &submitted); err != nil || submitted.State != "awaiting_approval" {
		t.Fatalf("job did not await approval: %s (%v)", resultText(res), err)
	}
	if _, err := n.prepareUpdate(time.Now()); err == nil {
		t.Fatal("foreign pending job did not block maintenance")
	}
	args, _ := json.Marshal(map[string]string{"job_id": submitted.JobID})
	if res := n.dispatch(t.Context(), "job_cancel", args, provider.Caller{DeviceID: n.ID(), Agent: "cli"}); !res.IsError {
		t.Fatal("maintenance bypassed job ownership")
	}
	if res := n.dispatch(t.Context(), "job_cancel", args, caller); res.IsError {
		t.Fatalf("owner cancellation: %s", resultText(res))
	}
	waitFor(t, "maintenance after owner cancels", func() bool {
		_, err := n.prepareUpdate(time.Now())
		return err == nil
	})
}

func TestUpdateControlAuthenticationAndGracefulStop(t *testing.T) {
	n := startGateNode(t, "host", &scriptSurface{})
	agentToken, err := n.paths.AddAgent("untrusted")
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", "wrong", agentToken} {
		for _, methodPath := range [][2]string{{"POST", "/v1/update/prepare"}, {"DELETE", "/v1/update/prepare"}, {"POST", "/v1/update/stop"}} {
			r := httptest.NewRequest(methodPath[0], methodPath[1], strings.NewReader(`{"lease":"bad"}`))
			r.Header.Set("Authorization", "Bearer "+token)
			w := httptest.NewRecorder()
			n.localHandler().ServeHTTP(w, r)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s returned %d", methodPath[0], methodPath[1], w.Code)
			}
		}
	}
	client, err := control.Dial(n.paths)
	if err != nil {
		t.Fatal(err)
	}
	var lease control.UpdatePreparation
	waitFor(t, "prepare", func() bool {
		lease, err = client.PrepareUpdate(t.Context())
		return err == nil
	})
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	run, err := n.paths.LoadRunInfo()
	if err != nil || lease.ID != n.ID() || lease.Executable != executable || run.Executable != executable {
		t.Fatalf("untruthful node identity: lease=%+v run=%+v err=%v", lease, run, err)
	}
	var status control.Status
	if err := client.Do(t.Context(), "GET", "/v1/status", nil, &status); err != nil || status.ID != n.ID() {
		t.Fatalf("status unavailable during maintenance: %v", err)
	}
	if err := client.StopUpdate(t.Context(), "foreign-node-lease"); err == nil {
		t.Fatal("foreign lease stopped node")
	}
	if err := client.StopUpdate(t.Context(), lease.Lease); err != nil {
		t.Fatalf("graceful stop response: %v", err)
	}
	select {
	case <-n.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("valid stop did not finish shutdown")
	}
}

func TestUpdateTracksStreamsButNotIdleMCP(t *testing.T) {
	for _, path := range []string{"/llm/host/model/v1/chat/completions", "/v1/files/download", "/mcp"} {
		t.Run(path, func(t *testing.T) {
			n := maintenanceNode()
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			h := n.admitHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(entered)
				<-release
			}))
			go func() {
				defer close(done)
				h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", path, nil))
			}()
			<-entered
			_, err := n.prepareUpdate(time.Now())
			close(release)
			<-done
			if err == nil {
				t.Fatal("prepared during an active streamed request")
			}
			if _, err := n.prepareUpdate(time.Now()); err != nil {
				t.Fatal(err)
			}
		})
	}
	n := maintenanceNode()
	var prepareErr error
	n.admitHTTP(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, prepareErr = n.prepareUpdate(time.Now())
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/mcp", nil))
	if prepareErr != nil {
		t.Fatalf("idle MCP server-push stream blocked maintenance: %v", prepareErr)
	}
}

func TestUpdateStopFlushesBeforeCancel(t *testing.T) {
	n := maintenanceNode()
	ctx, cancel := context.WithCancel(t.Context())
	n.cancel = cancel
	lease, err := n.prepareUpdate(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	w := &flushOrderWriter{ResponseRecorder: httptest.NewRecorder(), ctx: ctx}
	n.apiUpdateStop(w, httptest.NewRequest("POST", "/v1/update/stop", strings.NewReader(`{"lease":"`+lease.Lease+`"}`)))
	if !w.Flushed || w.cancelledAtFlush || ctx.Err() == nil {
		t.Fatalf("shutdown/flush ordering: flushed=%v canceledAtFlush=%v canceled=%v", w.Flushed, w.cancelledAtFlush, ctx.Err())
	}
}

type flushOrderWriter struct {
	*httptest.ResponseRecorder
	ctx              context.Context
	cancelledAtFlush bool
}

func (w *flushOrderWriter) Flush() {
	w.cancelledAtFlush = w.ctx.Err() != nil
	w.ResponseRecorder.Flush()
}

func TestUpdateStartupGateSurvivesCandidateAndRollback(t *testing.T) {
	original := startGateNode(t, "host", &scriptSurface{})
	opts, wantID := original.opts, original.ID()
	if _, err := original.paths.AddAgent("scheduler"); err != nil {
		t.Fatal(err)
	}
	client, err := control.Dial(original.paths)
	if err != nil {
		t.Fatal(err)
	}
	var lease control.UpdatePreparation
	waitFor(t, "prepare startup transaction", func() bool {
		lease, err = client.PrepareUpdate(t.Context())
		return err == nil
	})
	if err := original.paths.SaveUpdateStartup(lease, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := client.StopUpdate(t.Context(), lease.Lease); err != nil {
		t.Fatal(err)
	}
	original.Close()

	// Persist an already-due schedule while stopped, so the very first
	// scheduler pass would claim it if startup installed the gate too late.
	store, err := schedule.Open(opts.Paths.SchedulesFile(), schedule.Options{
		Now: func() time.Time { return time.Now().Add(-61 * time.Minute) },
	})
	if err != nil {
		t.Fatal(err)
	}
	scheduled, err := store.Add(schedule.Schedule{
		Agent: "scheduler", Device: wantID, DeviceName: "host", Tool: "node_info",
		Spec: schedule.Spec{In: "1h"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"candidate", "restored"} {
		n, err := Start(t.Context(), opts)
		if err != nil {
			t.Fatalf("%s startup: %v", phase, err)
		}
		t.Cleanup(n.Close)
		if n.ID() != wantID || n.Name() != original.Name() {
			t.Fatalf("%s changed persisted identity/configuration", phase)
		}
		client, err = control.Dial(opts.Paths)
		if err != nil {
			t.Fatal(err)
		}
		var status control.Status
		if err := client.Do(t.Context(), "GET", "/v1/status", nil, &status); err != nil || status.ID != wantID {
			t.Fatalf("%s owner status: %+v (%v)", phase, status, err)
		}
		transport := n.pinnedTransport(wantID)
		httpClient := &http.Client{Transport: transport}
		for _, request := range []struct{ method, path, body string }{
			{"POST", "/mcp", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"job_submit","arguments":{}}}`},
			{"GET", "/v1/files/download", ""},
		} {
			req, err := http.NewRequestWithContext(t.Context(), request.method, "https://"+n.MeshAddr()+request.path, strings.NewReader(request.body))
			if err != nil {
				t.Fatal(err)
			}
			response, err := httpClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if response.StatusCode != http.StatusServiceUnavailable {
				t.Fatalf("%s admitted %s: HTTP %d", phase, request.path, response.StatusCode)
			}
		}
		transport.CloseIdleConnections()
		if n.sched == nil || n.sched.runDue() {
			t.Fatalf("%s admitted scheduled work", phase)
		}
		record, err := n.sched.store.Get("", scheduled.ID)
		if err != nil || record.Running || len(record.Runs) != 0 || !record.Enabled {
			t.Fatalf("%s touched due schedule: %+v (%v)", phase, record, err)
		}
		if err := client.AbortUpdate(t.Context(), strings.Repeat("0", 64)); err == nil {
			t.Fatal("foreign token released startup admission")
		}
		if n.beginWork() {
			n.endWork()
			t.Fatal("foreign release reopened admission")
		}
		if phase == "candidate" {
			if err := client.StopUpdate(t.Context(), lease.Lease); err != nil {
				t.Fatalf("candidate did not inherit stop lease: %v", err)
			}
			n.Close()
			continue
		}
		if err := client.AbortUpdate(t.Context(), lease.Lease); err != nil {
			t.Fatalf("restored node did not inherit release lease: %v", err)
		}
		marker, err := opts.Paths.LoadUpdateStartup()
		if err != nil || marker != nil {
			t.Fatalf("commit retained marker: %+v (%v)", marker, err)
		}
		if !n.sched.runDue() {
			t.Fatal("commit did not restore scheduler admission")
		}
		record = waitForRuns(t, n, scheduled.ID, 1)
		answeredBy(t, record.Runs[0], wantID)
	}
}

func TestUpdateStartupExpiryAndForeignMarker(t *testing.T) {
	n := startGateNode(t, "host", &scriptSurface{})
	opts := n.opts
	n.Close()
	now := time.Now()
	lease, err := n.prepareUpdate(now.Add(-3 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if err := opts.Paths.SaveUpdateStartup(lease, now.Add(-3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	restarted, err := Start(t.Context(), opts)
	if err != nil {
		t.Fatalf("expired startup transaction bricked node: %v", err)
	}
	t.Cleanup(restarted.Close)
	if restarted.ID() != n.ID() || !restarted.beginWork() {
		t.Fatal("expired transaction did not restore original node admission")
	}
	restarted.endWork()
	restarted.Close()
	if marker, err := opts.Paths.LoadUpdateStartup(); err != nil || marker != nil {
		t.Fatalf("expired transaction was not removed: %+v (%v)", marker, err)
	}

	for _, mismatch := range []string{"identity", "executable"} {
		foreign := lease
		if mismatch == "identity" {
			foreign.ID = "different-node"
		} else {
			foreign.Executable = filepath.Join(t.TempDir(), "different-executable")
		}
		// Even an expired foreign transaction must fail closed.
		if err := opts.Paths.SaveUpdateStartup(foreign, now.Add(-3*time.Minute)); err != nil {
			t.Fatal(err)
		}
		if unexpected, err := Start(t.Context(), opts); err == nil {
			unexpected.Close()
			t.Fatalf("%s mismatch admitted startup", mismatch)
		}
		if err := opts.Paths.RemoveUpdateStartup(lease); !errors.Is(err, state.ErrUpdateStartupChanged) {
			t.Fatalf("%s mismatch removed foreign marker: %v", mismatch, err)
		}
		actual, err := opts.Paths.LoadUpdateStartup()
		if err != nil || actual == nil || actual.ID != foreign.ID || actual.Executable != foreign.Executable {
			t.Fatalf("foreign marker changed: %+v (%v)", actual, err)
		}
		if err := opts.Paths.RemoveUpdateStartup(foreign); err != nil {
			t.Fatal(err)
		}
	}
}

func TestUpdateStartupBoundAndTransactionReplacement(t *testing.T) {
	n := startGateNode(t, "host", &scriptSurface{})
	n.Close()
	now := time.Now()
	lease, err := n.prepareUpdate(now)
	if err != nil {
		t.Fatal(err)
	}
	unbounded := lease
	unbounded.Expires = now.Add(state.UpdateStartupDuration + time.Nanosecond)
	if err := n.paths.SaveUpdateStartup(unbounded, now); err == nil {
		t.Fatal("persisted a lease beyond the bounded lifetime")
	}
	if err := n.paths.SaveUpdateStartup(lease, now); err != nil {
		t.Fatal(err)
	}
	other := lease
	other.Lease = strings.Repeat("1", 64)
	if err := n.paths.SaveUpdateStartup(other, now); !errors.Is(err, state.ErrUpdateStartupChanged) {
		t.Fatalf("overwrote an existing transaction: %v", err)
	}
	if err := n.paths.RemoveUpdateStartup(lease); err != nil {
		t.Fatal(err)
	}
	if err := n.paths.SaveUpdateStartup(other, now); err != nil {
		t.Fatal(err)
	}
	if n.consumeUpdateLease(lease.Lease, false, now) || n.consumeUpdateLease(lease.Lease, true, now) {
		t.Fatal("old transaction released a replacement transaction")
	}
	if err := n.paths.RemoveUpdateStartup(lease); !errors.Is(err, state.ErrUpdateStartupChanged) {
		t.Fatalf("old transaction removed replacement: %v", err)
	}
	if err := n.paths.RemoveUpdateStartup(other); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateRecoveryHoldPreventsAdmissionRace(t *testing.T) {
	n := maintenanceNode()
	n.paths = state.Paths{Root: t.TempDir()}
	var err error
	n.executable, err = os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	lease, err := n.prepareUpdate(now)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.paths.SaveUpdateStartup(lease, now); err != nil {
		t.Fatal(err)
	}
	foreign := lease
	foreign.Lease = strings.Repeat("0", 64)
	if release, err := n.paths.HoldUpdateStartup(foreign); err == nil {
		release()
		t.Fatal("foreign transaction acquired recovery hold")
	}
	release, err := n.paths.HoldUpdateStartup(lease)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if n.consumeUpdateLease(lease.Lease, false, now) || n.beginWork() {
		t.Fatal("abort reopened admission during recovery stop")
	}
	n.maintenance.mu.Lock()
	n.maintenance.expireLocked(lease.Expires)
	retained := n.maintenance.lease == lease.Lease
	n.maintenance.mu.Unlock()
	if !retained {
		t.Fatal("expiry reopened admission during recovery stop")
	}
	release()
	if !n.consumeUpdateLease(lease.Lease, false, now) || !n.beginWork() {
		t.Fatal("released recovery hold did not restore normal commit")
	}
	n.endWork()
}
