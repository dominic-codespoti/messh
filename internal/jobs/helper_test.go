package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/provider"
	"messh/internal/state"
)

// The test binary doubles as the job program: when MESSH_JOBS_HELPER is set
// it behaves as the named helper instead of running tests.
func TestMain(m *testing.M) {
	if mode := os.Getenv("MESSH_JOBS_HELPER"); mode != "" {
		os.Exit(helperMain(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func helperMain(mode string, args []string) int {
	switch mode {
	case "echo":
		fmt.Println("hello from job")
		fmt.Fprintln(os.Stderr, "warning on stderr")
		os.WriteFile("out.txt", []byte("result"), 0o644)
		return 0
	case "exit3":
		return 3
	case "sleep":
		time.Sleep(time.Minute)
		return 0
	case "tree":
		// Spawn a grandchild that outlives nothing: both must die with the job.
		c := exec.Command(os.Args[0])
		c.Env = append(os.Environ(), "MESSH_JOBS_HELPER=sleep")
		if err := c.Start(); err != nil {
			return 9
		}
		os.WriteFile("child.pid", []byte(strconv.Itoa(c.Process.Pid)), 0o644)
		os.WriteFile("parent.pid", []byte(strconv.Itoa(os.Getpid())), 0o644)
		time.Sleep(time.Minute)
		return 0
	case "env":
		fmt.Println(os.Getenv(args[0]))
		return 0
	case "cwd":
		wd, _ := os.Getwd()
		fmt.Println(wd)
		return 0
	case "cat":
		b, err := os.ReadFile(args[0])
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		fmt.Print(string(b))
		return 0
	case "lines":
		n, _ := strconv.Atoi(args[0])
		for i := range n {
			fmt.Printf("line %05d xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx\n", i)
		}
		return 0
	}
	return 2
}

// --- fakes ---

type fakeTicket struct {
	id        string
	approved  chan bool
	denyErr   chan error
	mu        sync.Mutex
	cancelled bool
}

func newTicket(id string) *fakeTicket {
	return &fakeTicket{id: id, approved: make(chan bool, 1), denyErr: make(chan error, 1)}
}

func (t *fakeTicket) ID() string { return t.id }

func (t *fakeTicket) Wait(ctx context.Context) (bool, error) {
	select {
	case ok := <-t.approved:
		if ok {
			return true, nil
		}
		return false, <-t.denyErr
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (t *fakeTicket) Cancel() {
	t.mu.Lock()
	t.cancelled = true
	t.mu.Unlock()
}

func (t *fakeTicket) wasCancelled() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cancelled
}

func (t *fakeTicket) Approve() { t.approved <- true }

// Deny rejects; a non-nil err models an expiry or similar failure.
func (t *fakeTicket) Deny(err error) {
	t.denyErr <- err
	t.approved <- false
}

type fakeInhibitor struct {
	mu       sync.Mutex
	held     bool
	holds    int
	releases int
}

func (f *fakeInhibitor) Hold() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held = true
	f.holds++
	return nil
}

func (f *fakeInhibitor) Release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held = false
	f.releases++
}

func (f *fakeInhibitor) isHeld() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.held
}

type fakeRes struct {
	mu   sync.Mutex
	snap Snapshot
	err  error
}

func (f *fakeRes) Snapshot(context.Context) (Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snap, f.err
}

func (f *fakeRes) setFree(gpu int, free int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	gs := append([]GPUState(nil), f.snap.GPUs...)
	for i := range gs {
		if gs[i].Index == gpu {
			gs[i].FreeMB = free
		}
	}
	f.snap.GPUs = gs
}

// --- harness ---

type harness struct {
	t     *testing.T
	p     *Provider
	paths state.Paths
	inh   *fakeInhibitor
	res   *fakeRes
}

var agentA = provider.Caller{DeviceID: "dev-a", DeviceName: "laptop", Agent: "omp"}

func newHarness(t *testing.T, tweak func(*Options)) *harness {
	t.Helper()
	paths := state.Paths{Root: t.TempDir()}
	h := &harness{
		t: t, paths: paths, inh: &fakeInhibitor{},
		res: &fakeRes{snap: Snapshot{
			CPUThreads: 16, MemTotalMB: 32000, MemAvailMB: 24000,
			GPUs: []GPUState{
				{Index: 0, Name: "RTX 4070 Ti SUPER", TotalMB: 16000, FreeMB: 15000, FreeKnown: true},
				{Index: 1, Name: "RTX 4070 Ti SUPER", TotalMB: 16000, FreeMB: 15000, FreeKnown: true},
			},
		}},
	}
	opts := Options{
		Paths: paths, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Resources: h.res, Inhibitor: h.inh, NoScope: true,
		PollInterval: 40 * time.Millisecond, KillGrace: 500 * time.Millisecond, SettleTime: time.Millisecond,
		MaxConcurrent: 8, CPUBudget: 8,
	}
	if tweak != nil {
		tweak(&opts)
	}
	p, err := New(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	h.p = p
	return h
}

// args builds job_submit arguments that run the helper in the given mode.
func helperArgs(mode string, helperArgs ...string) map[string]any {
	return map[string]any{
		"command": os.Args[0],
		"args":    helperArgs,
		"env":     map[string]string{"MESSH_JOBS_HELPER": mode},
	}
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// submit mimics the node: Approval, then Call with a ticket.
func (h *harness) submit(args map[string]any, c provider.Caller) (string, *fakeTicket) {
	h.t.Helper()
	id, tk, err := h.trySubmit(args, c)
	if err != nil {
		h.t.Fatalf("submit: %v", err)
	}
	return id, tk
}

func (h *harness) trySubmit(args map[string]any, c provider.Caller) (string, *fakeTicket, error) {
	raw := mustJSON(args)
	ap, err := h.p.Approval(context.Background(), toolSubmit, raw, c)
	if err != nil {
		return "", nil, err
	}
	if !ap.Deferred || ap.Exact == "" {
		h.t.Fatalf("approval must be deferred with an exact hash: %+v", ap)
	}
	tk := newTicket("t-" + ap.Exact[:8])
	res, err := h.p.Call(provider.WithTicket(context.Background(), tk), toolSubmit, raw, c)
	if err != nil {
		return "", nil, err
	}
	var out submitResult
	decodeResult(h.t, res, &out)
	return out.JobID, tk, nil
}

func decodeResult(t *testing.T, res *mcp.CallToolResult, v any) {
	t.Helper()
	if res == nil || res.IsError {
		t.Fatalf("tool error: %+v", res)
	}
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func (h *harness) status(id string) Status {
	h.t.Helper()
	st, err := h.p.statusOf(id, agentA, 20)
	if err != nil {
		h.t.Fatal(err)
	}
	return st
}

func (h *harness) state(id string) State {
	h.t.Helper()
	h.p.mu.Lock()
	defer h.p.mu.Unlock()
	j := h.p.jobs[id]
	if j == nil {
		h.t.Fatalf("no job %s", id)
	}
	return j.State
}

func (h *harness) waitState(id string, want State) Status {
	h.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if h.state(id) == want {
			return h.status(id)
		}
		time.Sleep(15 * time.Millisecond)
	}
	st := h.status(id)
	h.t.Fatalf("job %s: want state %s, still %s (%s)", id, want, st.State, st.Reason)
	return st
}

func (h *harness) call(tool string, args any, c provider.Caller) (*mcp.CallToolResult, error) {
	return h.p.Call(context.Background(), tool, mustJSON(args), c)
}

func (h *harness) writeFile(ref string, content string) string {
	h.t.Helper()
	p := filepath.Join(h.paths.Root, filepath.FromSlash(ref))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		h.t.Fatal(err)
	}
	return p
}

func containsLine(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}
