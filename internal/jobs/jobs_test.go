package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"messh/internal/provider"
	"messh/internal/state"
)

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestSubmitRunsAndReportsEverything(t *testing.T) {
	h := newHarness(t, nil)
	h.writeFile("ws/src/data/in.txt", "payload")
	args := helperArgs("echo")
	args["inputs"] = []string{"ws/src/data/in.txt"}
	args["label"] = "demo"
	id, tk := h.submit(args, agentA)

	st := h.status(id)
	if st.State != StateAwaitingApproval {
		t.Fatalf("state after submit = %s, want awaiting_approval", st.State)
	}
	if h.inh.isHeld() {
		t.Fatal("device must not be kept awake while only awaiting approval")
	}
	if len(st.Inputs) != 1 || st.Inputs[0].SHA256 != sha("payload") || st.Inputs[0].Size != 7 {
		t.Fatalf("inputs = %+v", st.Inputs)
	}
	if st.Workspace != "ws/"+id {
		t.Fatalf("workspace = %s", st.Workspace)
	}

	tk.Approve()
	st = h.waitState(id, StateSucceeded)
	if st.ExitCode == nil || *st.ExitCode != 0 {
		t.Fatalf("exit code = %v", st.ExitCode)
	}
	if !containsLine(st.StdoutTail, "hello from job") || !containsLine(st.StderrTail, "warning on stderr") {
		t.Fatalf("logs: stdout=%v stderr=%v", st.StdoutTail, st.StderrTail)
	}
	if len(st.Outputs) != 1 || st.Outputs[0].Ref != "ws/"+id+"/out.txt" || st.Outputs[0].Size != 6 {
		t.Fatalf("outputs should list only out.txt (inputs excluded): %+v", st.Outputs)
	}
	snap, err := os.ReadFile(filepath.Join(h.paths.Root, "ws", id, "data", "in.txt"))
	if err != nil || string(snap) != "payload" {
		t.Fatalf("input snapshot missing: %q %v", snap, err)
	}
	if _, err := os.Stat(filepath.Join(h.paths.JobsDir(), id, "job.json")); err != nil {
		t.Fatal(err)
	}
	if h.inh.isHeld() {
		t.Fatal("inhibitor still held after the job finished")
	}
}

func TestSnapshotSurvivesReplacementOfSource(t *testing.T) {
	h := newHarness(t, nil)
	src := h.writeFile("ws/src/in.txt", "v1")
	args := helperArgs("cat", "in.txt")
	args["inputs"] = []string{"ws/src/in.txt"}
	id, tk := h.submit(args, agentA)
	// Replace (not edit in place) the source after the snapshot was taken.
	tmp := src + ".new"
	os.WriteFile(tmp, []byte("v2-different"), 0o600)
	if err := os.Rename(tmp, src); err != nil {
		t.Fatal(err)
	}
	tk.Approve()
	st := h.waitState(id, StateSucceeded)
	if !containsLine(st.StdoutTail, "v1") {
		t.Fatalf("job must see the approved bytes, got %v", st.StdoutTail)
	}
}

func TestDeniedAndExpired(t *testing.T) {
	h := newHarness(t, nil)
	id, tk := h.submit(helperArgs("echo"), agentA)
	tk.Deny(nil)
	if st := h.waitState(id, StateFailed); st.Reason != "denied" {
		t.Fatalf("reason = %q", st.Reason)
	}
	id2, tk2 := h.submit(helperArgs("echo"), agentA)
	tk2.Deny(context.DeadlineExceeded)
	if st := h.waitState(id2, StateFailed); st.Reason != "expired" {
		t.Fatalf("reason = %q", st.Reason)
	}
	if h.inh.holds != 0 {
		t.Fatal("denied jobs must never hold the device awake")
	}
}

func TestCancelWhileAwaitingApproval(t *testing.T) {
	h := newHarness(t, nil)
	id, tk := h.submit(helperArgs("echo"), agentA)
	res, err := h.call(toolCancel, map[string]any{"job_id": id}, agentA)
	if err != nil {
		t.Fatal(err)
	}
	var st Status
	decodeResult(t, res, &st)
	if st.State != StateCancelled {
		t.Fatalf("state = %s", st.State)
	}
	if !tk.wasCancelled() {
		t.Fatal("the approval prompt must be withdrawn")
	}
}

func TestCancelRunningKillsWholeTree(t *testing.T) {
	h := newHarness(t, nil)
	id, tk := h.submit(helperArgs("tree"), agentA)
	tk.Approve()
	h.waitState(id, StateRunning)
	parent, child := waitPid(t, filepath.Join(h.paths.Root, "ws", id, "parent.pid")), waitPid(t, filepath.Join(h.paths.Root, "ws", id, "child.pid"))
	if !pidAlive(parent) || !pidAlive(child) {
		t.Fatal("tree should be running")
	}

	// job_wait on a running job returns the running status at its timeout.
	res, err := h.call(toolWait, map[string]any{"job_id": id, "timeout_seconds": 1}, agentA)
	if err != nil {
		t.Fatal(err)
	}
	var st Status
	decodeResult(t, res, &st)
	if st.State != StateRunning {
		t.Fatalf("wait returned %s", st.State)
	}

	if _, err := h.call(toolCancel, map[string]any{"job_id": id}, agentA); err != nil {
		t.Fatal(err)
	}
	h.waitState(id, StateCancelled)
	deadline := time.Now().Add(10 * time.Second)
	for (pidAlive(parent) || pidAlive(child)) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if pidAlive(parent) || pidAlive(child) {
		t.Fatalf("process tree survived cancel: parent alive=%v child alive=%v", pidAlive(parent), pidAlive(child))
	}
}

func waitPid(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				return n
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
	return 0
}

func TestTimeoutKillsJob(t *testing.T) {
	h := newHarness(t, nil)
	args := helperArgs("sleep")
	args["timeout_seconds"] = 1
	id, tk := h.submit(args, agentA)
	tk.Approve()
	st := h.waitState(id, StateFailed)
	if st.Reason != "timed out after 1s" {
		t.Fatalf("reason = %q", st.Reason)
	}
}

func TestExitCodeFailure(t *testing.T) {
	h := newHarness(t, nil)
	id, tk := h.submit(helperArgs("exit3"), agentA)
	tk.Approve()
	st := h.waitState(id, StateFailed)
	if st.ExitCode == nil || *st.ExitCode != 3 || st.Reason != "exited with code 3" {
		t.Fatalf("status = %+v", st)
	}
}

func TestShellJob(t *testing.T) {
	h := newHarness(t, nil)
	id, tk := h.submit(map[string]any{"command": "echo shell-works", "shell": true}, agentA)
	tk.Approve()
	st := h.waitState(id, StateSucceeded)
	if !containsLine(st.StdoutTail, "shell-works") {
		t.Fatalf("stdout = %v", st.StdoutTail)
	}
	// Shell jobs are as broad as it gets, so the bin scope must carry the warning.
	ap, err := h.p.Approval(context.Background(), toolSubmit, mustJSON(map[string]any{"command": "echo x", "shell": true}), agentA)
	if err != nil {
		t.Fatal(err)
	}
	if !ap.Scopes[1].Broad {
		t.Fatal("shell scope must be flagged broad")
	}
}

func TestEnvAndCwd(t *testing.T) {
	h := newHarness(t, nil)
	args := helperArgs("env", "MY_VAR")
	args["env"] = map[string]string{"MESSH_JOBS_HELPER": "env", "MY_VAR": "42"}
	id, tk := h.submit(args, agentA)
	tk.Approve()
	st := h.waitState(id, StateSucceeded)
	if !containsLine(st.StdoutTail, "42") {
		t.Fatalf("stdout = %v", st.StdoutTail)
	}

	args = helperArgs("cwd")
	args["cwd"] = "sub/dir"
	id, tk = h.submit(args, agentA)
	tk.Approve()
	st = h.waitState(id, StateSucceeded)
	want := filepath.Join("ws", id, "sub", "dir")
	if len(st.StdoutTail) == 0 || !strings.HasSuffix(filepath.ToSlash(st.StdoutTail[0]), filepath.ToSlash(want)) {
		t.Fatalf("cwd printed %v, want suffix %s", st.StdoutTail, want)
	}
}

func TestCwdEscapeRejected(t *testing.T) {
	h := newHarness(t, nil)
	for _, cwd := range []string{"..", "../x", "a/../../b", "/abs", `C:\x`, "a\\b", "..\\x"} {
		args := helperArgs("echo")
		args["cwd"] = cwd
		if _, err := h.p.Approval(context.Background(), toolSubmit, mustJSON(args), agentA); err == nil {
			t.Errorf("cwd %q was accepted", cwd)
		}
	}
	for _, ws := range []string{"..", "a/b", "x\\y", ""} {
		if ws == "" {
			continue
		}
		args := helperArgs("echo")
		args["workspace"] = ws
		if _, err := h.p.Approval(context.Background(), toolSubmit, mustJSON(args), agentA); err == nil {
			t.Errorf("workspace %q was accepted", ws)
		}
	}
	args := helperArgs("echo")
	args["inputs"] = []string{"ws/../../etc/passwd"}
	if _, err := h.p.Approval(context.Background(), toolSubmit, mustJSON(args), agentA); err == nil {
		t.Error("escaping input ref was accepted")
	}
	args["inputs"] = []string{"desktop:ws/x/y"}
	if _, err := h.p.Approval(context.Background(), toolSubmit, mustJSON(args), agentA); err == nil {
		t.Error("qualified input ref was accepted")
	}
}

func TestCommandResolution(t *testing.T) {
	h := newHarness(t, nil)
	if _, err := h.p.Approval(context.Background(), toolSubmit, mustJSON(map[string]any{"command": "definitely-not-a-real-binary-xyz"}), agentA); err == nil {
		t.Error("unknown command accepted")
	}
	if _, err := h.p.Approval(context.Background(), toolSubmit, mustJSON(map[string]any{"command": "sub/prog"}), agentA); err == nil {
		t.Error("relative path command accepted")
	}
	ap, err := h.p.Approval(context.Background(), toolSubmit, mustJSON(helperArgs("echo", "a b")), agentA)
	if err != nil {
		t.Fatal(err)
	}
	cmd := ""
	for _, d := range ap.Details {
		if d.Label == "Command" {
			cmd = d.Value
		}
	}
	exe, _ := filepath.Abs(os.Args[0])
	if !strings.Contains(cmd, strconv.Quote(exe)) || !strings.Contains(cmd, `"a b"`) {
		t.Fatalf("approval must show the resolved path and quoted args: %s", cmd)
	}
}

func TestApprovalHashAndScopes(t *testing.T) {
	h := newHarness(t, nil)
	src := h.writeFile("ws/src/in.txt", "one")
	exact := func(a map[string]any) (string, []provider.Scope) {
		t.Helper()
		ap, err := h.p.Approval(context.Background(), toolSubmit, mustJSON(a), agentA)
		if err != nil {
			t.Fatal(err)
		}
		return ap.Exact, ap.Scopes
	}
	base := func() map[string]any {
		a := helperArgs("echo", "x")
		a["inputs"] = []string{"ws/src/in.txt"}
		return a
	}
	e0, s0 := exact(base())
	if e1, _ := exact(base()); e1 != e0 {
		t.Fatal("hash must be stable for the same request")
	}

	mutations := map[string]func(map[string]any){
		"args":      func(a map[string]any) { a["args"] = []string{"y"} },
		"env":       func(a map[string]any) { a["env"] = map[string]string{"MESSH_JOBS_HELPER": "echo", "X": "1"} },
		"workspace": func(a map[string]any) { a["workspace"] = "named" },
		"cwd":       func(a map[string]any) { a["cwd"] = "sub" },
		"timeout":   func(a map[string]any) { a["timeout_seconds"] = 5 },
		"gpus":      func(a map[string]any) { a["resources"] = map[string]any{"gpus": []int{0}} },
		"vram":      func(a map[string]any) { a["resources"] = map[string]any{"vram_mb": 100} },
		"inputs":    func(a map[string]any) { a["inputs"] = []string{} },
		"shell":     func(a map[string]any) { a["shell"] = true; a["args"] = []string{} },
	}
	for name, mut := range mutations {
		a := base()
		mut(a)
		if e, _ := exact(a); e == e0 {
			t.Errorf("changing %s did not change the approval hash", name)
		}
	}
	if e, _ := exact(func() map[string]any { a := base(); a["label"] = "other"; return a }()); e != e0 {
		t.Error("the label is cosmetic and must not change the hash")
	}

	// Input content, not metadata, is what counts.
	os.WriteFile(src, []byte("two"), 0o600)
	e2, s2 := exact(base())
	if e2 == e0 {
		t.Fatal("changing input content must change the hash")
	}
	os.WriteFile(src, []byte("one"), 0o600)
	os.Chtimes(src, time.Now().Add(time.Hour), time.Now().Add(time.Hour))
	if e3, _ := exact(base()); e3 != e0 {
		t.Fatal("same content with a new mtime must keep the hash")
	}

	// Scopes: narrowest first; the command scope ignores inputs, the binary scope ignores args.
	if len(s0) != 3 || !strings.HasPrefix(s0[0].Key, "exec:cmd:") || !strings.HasPrefix(s0[1].Key, "exec:bin:") || s0[2].Key != "exec:*" || !s0[2].Broad {
		t.Fatalf("scopes = %+v", s0)
	}
	if s2[0].Key != s0[0].Key {
		t.Error("command scope must not depend on input content")
	}
	a := base()
	a["args"] = []string{"other"}
	_, s4 := exact(a)
	if s4[0].Key == s0[0].Key {
		t.Error("command scope must depend on args")
	}
	if s4[1].Key != s0[1].Key {
		t.Error("binary scope must not depend on args")
	}
}

// A same-size rewrite that keeps the mtime (coarse filesystem clocks, or a
// tool restoring timestamps) right after hashing must not reuse the old hash.
func TestRacilyCleanInputIsRehashed(t *testing.T) {
	h := newHarness(t, nil)
	src := h.writeFile("ws/src/in.txt", "one")
	fi, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	rewrite := func(content string) {
		t.Helper()
		if err := os.WriteFile(src, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(src, fi.ModTime(), fi.ModTime()); err != nil {
			t.Fatal(err)
		}
		if st, err := os.Stat(src); err != nil || st.Size() != fi.Size() || !st.ModTime().Equal(fi.ModTime()) {
			t.Fatalf("could not restore size and mtime: %v", err)
		}
	}
	args := helperArgs("echo")
	args["inputs"] = []string{"ws/src/in.txt"}
	exact := func() string {
		t.Helper()
		ap, err := h.p.Approval(context.Background(), toolSubmit, mustJSON(args), agentA)
		if err != nil {
			t.Fatal(err)
		}
		return ap.Exact
	}

	e0 := exact()
	rewrite("two")
	if exact() == e0 {
		t.Fatal("approval hash reused a cached hash for same-size, same-mtime new content")
	}

	// Pre-launch check: snapshot (in place), then change the bytes but not the metadata.
	j := &job{Job: Job{Workspace: "src"}}
	req := &request{Inputs: []Input{{Ref: "ws/src/in.txt"}}}
	if err := h.p.snapshotInputs(j, req); err != nil {
		t.Fatal(err)
	}
	j.Inputs = req.Inputs
	if err := h.p.verifyInputs(j); err != nil {
		t.Fatalf("unchanged input rejected: %v", err)
	}
	rewrite("six")
	if err := h.p.verifyInputs(j); err == nil || !strings.Contains(err.Error(), "changed after it was approved") {
		t.Fatalf("same-size, same-mtime change slipped past the pre-launch check: %v", err)
	}
}

func TestInputChangedBetweenApprovalAndCall(t *testing.T) {
	h := newHarness(t, nil)
	src := h.writeFile("ws/src/in.txt", "approved")
	args := helperArgs("echo")
	args["inputs"] = []string{"ws/src/in.txt"}
	raw := mustJSON(args)
	if _, err := h.p.Approval(context.Background(), toolSubmit, raw, agentA); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(src, []byte("swapped!"), 0o600) // after the owner saw the hash
	tk := newTicket("t")
	res, err := h.p.Call(provider.WithTicket(context.Background(), tk), toolSubmit, raw, agentA)
	if err != nil {
		t.Fatal(err)
	}
	var out submitResult
	decodeResult(t, res, &out)
	st := h.waitState(out.JobID, StateFailed)
	if !strings.Contains(st.Reason, "changed between approval and execution") {
		t.Fatalf("reason = %q", st.Reason)
	}
	if !tk.wasCancelled() {
		t.Fatal("the stale prompt must be withdrawn")
	}
}

func TestCallWithoutApprovalIsRefused(t *testing.T) {
	h := newHarness(t, nil)
	raw := mustJSON(helperArgs("echo"))
	if _, err := h.p.Call(context.Background(), toolSubmit, raw, agentA); err == nil {
		t.Fatal("job_submit without a ticket must fail")
	}
	// A ticket without a prior Approval (nothing was shown to anyone) must not run either.
	res, err := h.p.Call(provider.WithTicket(context.Background(), newTicket("t")), toolSubmit, raw, agentA)
	if err != nil {
		t.Fatal(err)
	}
	var out submitResult
	decodeResult(t, res, &out)
	h.waitState(out.JobID, StateFailed)
}

func TestOwnership(t *testing.T) {
	h := newHarness(t, nil)
	id, tk := h.submit(helperArgs("sleep"), agentA)
	tk.Approve()
	h.waitState(id, StateRunning)

	others := []provider.Caller{
		{DeviceID: "dev-a", DeviceName: "laptop", Agent: "pi"},   // same device, other agent
		{DeviceID: "dev-b", DeviceName: "phone", Agent: "omp"},   // same agent name, other device
		{DeviceID: "dev-b", DeviceName: "phone", Agent: "other"}, // nothing in common
	}
	for _, o := range others {
		for _, tool := range []string{toolStatus, toolWait, toolLogs, toolCancel, toolDelete} {
			if _, err := h.call(tool, map[string]any{"job_id": id}, o); err == nil {
				t.Errorf("%s by %+v succeeded on someone else's job", tool, o)
			}
		}
		res, err := h.call(toolList, map[string]any{}, o)
		if err != nil {
			t.Fatal(err)
		}
		var l struct{ Jobs []Summary }
		decodeResult(t, res, &l)
		if len(l.Jobs) != 0 {
			t.Errorf("%+v sees %d foreign jobs", o, len(l.Jobs))
		}
	}
	if h.state(id) != StateRunning {
		t.Fatal("foreign cancel attempts must not touch the job")
	}
	res, err := h.call(toolList, map[string]any{"state": "running"}, agentA)
	if err != nil {
		t.Fatal(err)
	}
	var l struct{ Jobs []Summary }
	decodeResult(t, res, &l)
	if len(l.Jobs) != 1 || l.Jobs[0].JobID != id {
		t.Fatalf("owner list = %+v", l.Jobs)
	}
}

func TestSleepInhibitorHeldOnlyWhileRunning(t *testing.T) {
	h := newHarness(t, nil)
	a, tka := h.submit(helperArgs("sleep"), agentA)
	b, tkb := h.submit(helperArgs("sleep"), agentA)
	if h.inh.isHeld() {
		t.Fatal("held before anything runs")
	}
	tka.Approve()
	h.waitState(a, StateRunning)
	if !h.inh.isHeld() {
		t.Fatal("not held while a job runs")
	}
	tkb.Approve()
	h.waitState(b, StateRunning)
	h.call(toolCancel, map[string]any{"job_id": a}, agentA)
	h.waitState(a, StateCancelled)
	if !h.inh.isHeld() {
		t.Fatal("released while another job still runs")
	}
	h.call(toolCancel, map[string]any{"job_id": b}, agentA)
	h.waitState(b, StateCancelled)
	if h.inh.isHeld() {
		t.Fatal("still held after the last job ended")
	}
	if h.inh.holds != 1 || h.inh.releases != 1 {
		t.Fatalf("holds=%d releases=%d, want 1/1", h.inh.holds, h.inh.releases)
	}
}

func approveAll(t *testing.T, h *harness, args map[string]any) string {
	t.Helper()
	id, tk := h.submit(args, agentA)
	tk.Approve()
	return id
}

func sleepWith(res map[string]any) map[string]any {
	a := helperArgs("sleep")
	a["resources"] = res
	return a
}

func TestFIFOAndGPUExclusivity(t *testing.T) {
	h := newHarness(t, nil)
	a := approveAll(t, h, sleepWith(map[string]any{"gpus": []int{0}}))
	h.waitState(a, StateRunning)
	b := approveAll(t, h, sleepWith(map[string]any{"gpus": []int{0, 1}})) // waits for GPU 0
	h.waitState(b, StateQueued)
	c := approveAll(t, h, sleepWith(map[string]any{"gpus": []int{1}})) // GPU 1 is free, but b is ahead and wants it
	h.waitState(c, StateQueued)
	d := approveAll(t, h, sleepWith(map[string]any{"cpus": 2})) // CPU-only job is not stuck behind the GPU line
	h.waitState(d, StateRunning)

	time.Sleep(200 * time.Millisecond) // several scheduler passes
	if s := h.state(c); s != StateQueued {
		t.Fatalf("c jumped the GPU queue: %s", s)
	}
	if st := h.status(b); st.QueuePosition != 1 || !strings.Contains(st.WaitingFor, "GPU 0") {
		t.Fatalf("b: position=%d waiting=%q", st.QueuePosition, st.WaitingFor)
	}
	if st := h.status(c); st.QueuePosition != 2 {
		t.Fatalf("c position = %d", st.QueuePosition)
	}

	h.call(toolCancel, map[string]any{"job_id": a}, agentA)
	h.waitState(b, StateRunning)
	time.Sleep(150 * time.Millisecond)
	if s := h.state(c); s != StateQueued {
		t.Fatalf("c must wait while b holds GPU 1: %s", s)
	}
	h.call(toolCancel, map[string]any{"job_id": b}, agentA)
	h.waitState(c, StateRunning)
	for _, id := range []string{c, d} {
		h.call(toolCancel, map[string]any{"job_id": id}, agentA)
		h.waitState(id, StateCancelled)
	}
}

func TestCUDAVisibleDevicesFollowsClaim(t *testing.T) {
	h := newHarness(t, nil)
	a := helperArgs("env", "CUDA_VISIBLE_DEVICES")
	a["resources"] = map[string]any{"gpus": []int{1, 0}}
	id := approveAll(t, h, a)
	st := h.waitState(id, StateSucceeded)
	if !containsLine(st.StdoutTail, "0,1") {
		t.Fatalf("CUDA_VISIBLE_DEVICES = %v", st.StdoutTail)
	}
	id = approveAll(t, h, helperArgs("env", "CUDA_VISIBLE_DEVICES"))
	st = h.waitState(id, StateSucceeded)
	if os.Getenv("CUDA_VISIBLE_DEVICES") == "" && len(st.StdoutTail) != 0 {
		t.Fatalf("unclaimed job must not get CUDA_VISIBLE_DEVICES set: %v", st.StdoutTail)
	}
}

func TestVRAMMemAndImpossibleClaims(t *testing.T) {
	h := newHarness(t, nil)
	// More than the card has: can never run.
	id := approveAll(t, h, sleepWith(map[string]any{"gpus": []int{0}, "vram_mb": 20000}))
	if st := h.waitState(id, StateFailed); !strings.Contains(st.Reason, "VRAM") {
		t.Fatalf("reason = %q", st.Reason)
	}
	id = approveAll(t, h, sleepWith(map[string]any{"gpus": []int{7}}))
	if st := h.waitState(id, StateFailed); !strings.Contains(st.Reason, "does not exist") {
		t.Fatalf("reason = %q", st.Reason)
	}
	id = approveAll(t, h, sleepWith(map[string]any{"mem_mb": 99999}))
	if st := h.waitState(id, StateFailed); !strings.Contains(st.Reason, "RAM") {
		t.Fatalf("reason = %q", st.Reason)
	}
	id = approveAll(t, h, sleepWith(map[string]any{"cpus": 99}))
	if st := h.waitState(id, StateFailed); !strings.Contains(st.Reason, "CPU") {
		t.Fatalf("reason = %q", st.Reason)
	}

	// Fits the card but not the free VRAM right now: waits, then starts once memory frees up.
	h.res.setFree(0, 4000)
	id = approveAll(t, h, sleepWith(map[string]any{"gpus": []int{0}, "vram_mb": 8000}))
	h.waitState(id, StateQueued)
	time.Sleep(150 * time.Millisecond)
	if st := h.status(id); st.State != StateQueued || !strings.Contains(st.WaitingFor, "VRAM") {
		t.Fatalf("status = %s %q", st.State, st.WaitingFor)
	}
	h.res.setFree(0, 12000)
	h.waitState(id, StateRunning)
	h.call(toolCancel, map[string]any{"job_id": id}, agentA)
	h.waitState(id, StateCancelled)

	// RAM is checked against what is available.
	h.res.mu.Lock()
	h.res.snap.MemAvailMB = 1000
	h.res.mu.Unlock()
	id = approveAll(t, h, sleepWith(map[string]any{"mem_mb": 5000}))
	h.waitState(id, StateQueued)
	h.res.mu.Lock()
	h.res.snap.MemAvailMB = 9000
	h.res.mu.Unlock()
	h.waitState(id, StateRunning)
	h.call(toolCancel, map[string]any{"job_id": id}, agentA)
	h.waitState(id, StateCancelled)

	// A probe failure for a job that needs the probe fails it with the reason.
	h.res.mu.Lock()
	h.res.err = fmt.Errorf("nvidia-smi exploded")
	h.res.mu.Unlock()
	id = approveAll(t, h, sleepWith(map[string]any{"gpus": []int{0}}))
	if st := h.waitState(id, StateFailed); !strings.Contains(st.Reason, "nvidia-smi exploded") {
		t.Fatalf("reason = %q", st.Reason)
	}
	// Jobs without hardware claims never depend on the probe.
	id = approveAll(t, h, helperArgs("echo"))
	h.waitState(id, StateSucceeded)
}

func TestVRAMReservationBlocksDoubleBooking(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.SettleTime = time.Hour })
	// Two unpinned 9 GB jobs on a 15 GB-free card: the first one's memory may not show in the probe yet.
	a := approveAll(t, h, sleepWith(map[string]any{"vram_mb": 9000}))
	h.waitState(a, StateRunning)
	b := approveAll(t, h, sleepWith(map[string]any{"vram_mb": 9000}))
	h.waitState(b, StateQueued)
	time.Sleep(150 * time.Millisecond)
	if s := h.state(b); s != StateQueued {
		t.Fatalf("second job oversubscribed the GPU: %s", s)
	}
}

// A job takes its queue position when it is approved. Approvals resolve on
// their own goroutines, so each one must have landed before the next is
// given, or back-to-back approvals may queue in either order. Approving c
// before the earlier-submitted b also proves submission order does not count.
func TestConcurrencyCapIsFIFO(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.MaxConcurrent = 1 })
	a := approveAll(t, h, helperArgs("sleep"))
	h.waitState(a, StateRunning)
	b, tkb := h.submit(helperArgs("sleep"), agentA)
	c := approveAll(t, h, helperArgs("sleep"))
	h.waitState(c, StateQueued)
	tkb.Approve()
	h.waitState(b, StateQueued)
	if st := h.status(b); st.QueuePosition != 2 {
		t.Fatalf("b position = %d, want 2 (behind c)", st.QueuePosition)
	}
	h.call(toolCancel, map[string]any{"job_id": a}, agentA)
	h.waitState(c, StateRunning)
	time.Sleep(100 * time.Millisecond)
	if st := h.status(b); st.State != StateQueued || !strings.Contains(st.WaitingFor, "free slot") {
		t.Fatalf("cap exceeded: b is %s (%q)", st.State, st.WaitingFor)
	}
}

func TestRestartMarksLiveJobsInterruptedAndKillsOrphans(t *testing.T) {
	paths := state.Paths{Root: t.TempDir()}
	write := func(j Job) {
		dir := filepath.Join(paths.JobsDir(), j.ID)
		os.MkdirAll(dir, 0o700)
		b, _ := json.Marshal(j)
		os.WriteFile(filepath.Join(dir, "job.json"), b, 0o600)
	}
	// Two real stray processes: one matches its recorded identity, one is a reused PID.
	start := func() *exec.Cmd {
		c := exec.Command(os.Args[0])
		c.Env = append(os.Environ(), "MESSH_JOBS_HELPER=sleep")
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Process.Kill(); c.Wait() })
		return c
	}
	orphan, bystander := start(), start()
	time.Sleep(200 * time.Millisecond)
	now := time.Now()
	base := func(id string, st State) Job {
		return Job{ID: id, Owner: Owner{DeviceID: "dev-a", Agent: "omp"}, State: st, Submitted: now, Path: "x", Workspace: id}
	}
	for i, st := range []State{StateSubmitted, StateAwaitingApproval, StateQueued} {
		write(base(fmt.Sprintf("job-live%d", i), st))
	}
	r := base("job-running", StateRunning)
	r.PID, r.PIDToken = orphan.Process.Pid, tokenFor(orphan.Process.Pid)
	write(r)
	rb := base("job-reused", StateRunning)
	rb.PID, rb.PIDToken = bystander.Process.Pid, "not-its-start-time"
	write(rb)
	write(base("job-done", StateSucceeded))

	p, err := New(context.Background(), Options{Paths: paths, Resources: &fakeRes{}, Inhibitor: &fakeInhibitor{}, NoScope: true})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, id := range []string{"job-live0", "job-live1", "job-live2", "job-running", "job-reused"} {
		st, err := p.statusOf(id, provider.Caller{DeviceID: "dev-a", Agent: "omp"}, 5)
		if err != nil {
			t.Fatal(err)
		}
		if st.State != StateInterrupted || st.Reason != "node restarted" || st.Finished == nil {
			t.Errorf("%s: %s %q", id, st.State, st.Reason)
		}
	}
	if st, _ := p.statusOf("job-done", provider.Caller{DeviceID: "dev-a", Agent: "omp"}, 5); st.State != StateSucceeded {
		t.Errorf("finished job changed to %s", st.State)
	}
	deadline := time.Now().Add(5 * time.Second)
	for pidAlive(orphan.Process.Pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if pidAlive(orphan.Process.Pid) {
		t.Error("leftover process with matching identity was not killed")
	}
	if !pidAlive(bystander.Process.Pid) {
		t.Error("process with a reused PID was killed")
	}
	// The interruption is persisted.
	b, _ := os.ReadFile(filepath.Join(paths.JobsDir(), "job-live0", "job.json"))
	if !strings.Contains(string(b), `"interrupted"`) {
		t.Errorf("job.json = %s", b)
	}
}

func TestCloseKillsRunningJobs(t *testing.T) {
	h := newHarness(t, nil)
	id, tk := h.submit(helperArgs("tree"), agentA)
	tk.Approve()
	h.waitState(id, StateRunning)
	child := waitPid(t, filepath.Join(h.paths.Root, "ws", id, "child.pid"))
	waiting, _ := h.submit(helperArgs("echo"), agentA)
	h.p.Close()
	if s := h.state(id); s != StateInterrupted {
		t.Fatalf("running job state after Close = %s", s)
	}
	if s := h.state(waiting); s != StateInterrupted {
		t.Fatalf("pending job state after Close = %s", s)
	}
	if pidAlive(child) {
		t.Fatal("grandchild survived Close")
	}
	if h.inh.isHeld() {
		t.Fatal("inhibitor held after Close")
	}
}

func TestLogCapKeepsHeadAndTail(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.HeadBytes, o.SegmentBytes = 1024, 2048 })
	id := approveAll(t, h, helperArgs("lines", "400"))
	st := h.waitState(id, StateSucceeded)
	if !containsLine(st.StdoutTail, "line 00399") || !st.LogsOmitted {
		t.Fatalf("tail must end at the last line and flag omission: %v omitted=%v", st.StdoutTail[len(st.StdoutTail)-1:], st.LogsOmitted)
	}
	lineLen := len(fmt.Sprintf("line %05d xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\n", 0))
	total := int64(400 * lineLen)

	read := func(args map[string]any) logChunk {
		args["job_id"] = id
		res, err := h.call(toolLogs, args, agentA)
		if err != nil {
			t.Fatal(err)
		}
		var out struct{ Stdout logChunk }
		decodeResult(t, res, &out)
		return out.Stdout
	}
	head := read(map[string]any{"stream": "stdout", "offset": 0, "max_bytes": 100000})
	if !strings.HasPrefix(head.Text, "line 00000") || head.NextOffset != 1024 || head.TotalBytes != total {
		t.Fatalf("head chunk: next=%d total=%d (want 1024/%d) text=%.20q", head.NextOffset, head.TotalBytes, total, head.Text)
	}
	more := read(map[string]any{"stream": "stdout", "offset": head.NextOffset, "max_bytes": 100000})
	if more.Skipped == 0 || more.Offset <= head.NextOffset || more.NextOffset != total {
		t.Fatalf("rolled-away stretch must be skipped, not misreported: %+v", more)
	}
	if !strings.Contains(more.Text[max(0, len(more.Text)-100):], "line 00399 ") {
		t.Fatalf("tail text ends %q", more.Text[max(0, len(more.Text)-40):])
	}
	tail := read(map[string]any{"stream": "stdout", "tail_lines": 3})
	if n := len(strings.Split(strings.TrimSpace(tail.Text), "\n")); n != 3 {
		t.Fatalf("tail_lines=3 returned %d lines", n)
	}
	if tail.Offset+int64(len(tail.Text)) != tail.TotalBytes || !tail.More {
		t.Fatalf("tail offsets must line up with the stream: %+v", tail)
	}

	entries, _ := os.ReadDir(filepath.Join(h.paths.JobsDir(), id))
	var disk int64
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "stdout.log") {
			info, _ := e.Info()
			disk += info.Size()
		}
	}
	if disk > 1024+2*2048 {
		t.Fatalf("stdout logs use %d bytes on disk, cap is %d", disk, 1024+2*2048)
	}
}

func TestDelete(t *testing.T) {
	h := newHarness(t, nil)
	running, tk := h.submit(helperArgs("sleep"), agentA)
	if _, err := h.call(toolDelete, map[string]any{"job_id": running}, agentA); err == nil {
		t.Fatal("deleting an unfinished job must fail")
	}
	tk.Approve()
	h.waitState(running, StateRunning)
	if _, err := h.call(toolDelete, map[string]any{"job_id": running}, agentA); err == nil {
		t.Fatal("deleting a running job must fail")
	}
	h.call(toolCancel, map[string]any{"job_id": running}, agentA)
	h.waitState(running, StateCancelled)

	// Two jobs sharing a workspace: the files stay until the last one goes.
	a := helperArgs("echo")
	a["workspace"] = "shared"
	id1 := approveAll(t, h, a)
	h.waitState(id1, StateSucceeded)
	id2 := approveAll(t, h, a)
	h.waitState(id2, StateSucceeded)
	ws := filepath.Join(h.paths.Root, "ws", "shared")
	res, err := h.call(toolDelete, map[string]any{"job_id": id1}, agentA)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		WorkspaceRemoved bool `json:"workspace_removed"`
	}
	decodeResult(t, res, &out)
	if out.WorkspaceRemoved {
		t.Fatal("shared workspace removed while another job uses it")
	}
	if _, err := os.Stat(filepath.Join(ws, "out.txt")); err != nil {
		t.Fatal("shared output lost")
	}
	if _, err := os.Stat(filepath.Join(h.paths.JobsDir(), id1)); !os.IsNotExist(err) {
		t.Fatal("job record not removed")
	}
	res, err = h.call(toolDelete, map[string]any{"job_id": id2}, agentA)
	if err != nil {
		t.Fatal(err)
	}
	decodeResult(t, res, &out)
	if !out.WorkspaceRemoved {
		t.Fatal("workspace should go with the last job")
	}
	if _, err := os.Stat(ws); !os.IsNotExist(err) {
		t.Fatal("workspace still on disk")
	}
	if _, err := h.p.statusOf(id2, agentA, 1); err == nil {
		t.Fatal("deleted job still known")
	}
}

func TestResourcesTool(t *testing.T) {
	h := newHarness(t, nil)
	id := approveAll(t, h, sleepWith(map[string]any{"gpus": []int{1}, "cpus": 2}))
	h.waitState(id, StateRunning)
	res, err := h.call(toolResources, map[string]any{}, agentA)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		CPUClaimed int       `json:"cpu_threads_claimed"`
		Running    int       `json:"jobs_running"`
		GPUs       []gpuView `json:"gpus"`
	}
	decodeResult(t, res, &out)
	if out.CPUClaimed != 2 || out.Running != 1 || len(out.GPUs) != 2 || out.GPUs[1].ClaimedBy != id || out.GPUs[0].ClaimedBy != "" || out.GPUs[1].TotalMB != 16000 {
		t.Fatalf("resources = %+v", out)
	}
	// Another agent sees the claim but not whose it is.
	res, _ = h.call(toolResources, map[string]any{}, provider.Caller{DeviceID: "dev-b", Agent: "x"})
	decodeResult(t, res, &out)
	if out.GPUs[1].ClaimedBy == id || out.GPUs[1].ClaimedBy == "" {
		t.Fatalf("claim owner leaked or hidden: %q", out.GPUs[1].ClaimedBy)
	}
	h.call(toolCancel, map[string]any{"job_id": id}, agentA)
	h.waitState(id, StateCancelled)
}

func TestToolSetAndClasses(t *testing.T) {
	h := newHarness(t, nil)
	want := map[string]provider.Class{
		"job_submit": provider.ClassExec, "job_status": provider.ClassInfo, "job_wait": provider.ClassInfo,
		"job_logs": provider.ClassInfo, "job_cancel": provider.ClassInfo, "job_list": provider.ClassInfo,
		"job_delete": provider.ClassInfo, "job_resources": provider.ClassInfo,
	}
	got := h.p.Tools()
	if len(got) != len(want) {
		t.Fatalf("%d tools", len(got))
	}
	for _, tl := range got {
		if want[tl.Def.Name] != tl.Class {
			t.Errorf("%s class = %s", tl.Def.Name, tl.Class)
		}
		var schema map[string]any
		if err := json.Unmarshal(tl.Def.InputSchema.(json.RawMessage), &schema); err != nil || schema["type"] != "object" {
			t.Errorf("%s schema invalid: %v", tl.Def.Name, err)
		}
	}
}

func TestCopyFallbackHashesWhatItCopies(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	os.WriteFile(src, []byte("copy me"), 0o600)
	dst := filepath.Join(dir, "dst")
	sum, err := copyHashing(context.Background(), src, dst)
	if err != nil || sum != sha("copy me") {
		t.Fatalf("sum=%s err=%v", sum, err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "copy me" {
		t.Fatalf("dst = %q", b)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := copyHashing(ctx, src, filepath.Join(dir, "dst2")); err == nil {
		t.Fatal("a cancelled copy must stop")
	}
}

func TestInputsInSameWorkspaceAndCollisions(t *testing.T) {
	h := newHarness(t, nil)
	h.writeFile("ws/shared/data.txt", "already here")
	a := helperArgs("cat", "data.txt")
	a["workspace"] = "shared"
	a["inputs"] = []string{"ws/shared/data.txt"}
	id := approveAll(t, h, a)
	st := h.waitState(id, StateSucceeded)
	if !containsLine(st.StdoutTail, "already here") || len(st.Inputs) != 1 {
		t.Fatalf("status = %+v", st)
	}
	for _, o := range st.Outputs {
		if strings.HasSuffix(o.Ref, "data.txt") {
			t.Fatalf("an untouched in-place input is not an output: %+v", st.Outputs)
		}
	}

	// Two sources that would land on the same path are refused.
	h.writeFile("ws/a/x.txt", "1")
	h.writeFile("ws/b/x.txt", "2")
	b := helperArgs("echo")
	b["inputs"] = []string{"ws/a/x.txt", "ws/b/x.txt"}
	id2, _ := h.submit(b, agentA)
	if st := h.waitState(id2, StateFailed); !strings.Contains(st.Reason, "same place") {
		t.Fatalf("reason = %q", st.Reason)
	}

	// A workspace that already holds a different file at the input's place is not overwritten.
	h.writeFile("ws/target/x.txt", "different")
	c := helperArgs("echo")
	c["workspace"] = "target"
	c["inputs"] = []string{"ws/a/x.txt"}
	id3, _ := h.submit(c, agentA)
	if st := h.waitState(id3, StateFailed); !strings.Contains(st.Reason, "different file") {
		t.Fatalf("reason = %q", st.Reason)
	}
	if b, _ := os.ReadFile(filepath.Join(h.paths.Root, "ws", "target", "x.txt")); string(b) != "different" {
		t.Fatal("existing workspace file was overwritten")
	}
}
