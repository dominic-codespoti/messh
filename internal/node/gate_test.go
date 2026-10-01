package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/approval"
	"messh/internal/control"
	"messh/internal/provider"
	"messh/internal/state"
)

// scriptSurface plays the person at the host: it records each prompt and
// answers from a queue; with an empty queue the prompt stays open until
// cancelled, as an unattended dialog would.
type scriptSurface struct {
	mu      sync.Mutex
	answers []approval.Answer
	prompts []approval.Prompt
	closed  int
}

func (s *scriptSurface) Name() string    { return "script" }
func (s *scriptSurface) Available() bool { return true }
func (s *scriptSurface) Ask(ctx context.Context, p approval.Prompt) (approval.Answer, error) {
	s.mu.Lock()
	s.prompts = append(s.prompts, p)
	if len(s.answers) > 0 {
		a := s.answers[0]
		s.answers = s.answers[1:]
		s.mu.Unlock()
		return a, nil
	}
	s.mu.Unlock()
	<-ctx.Done()
	s.mu.Lock()
	s.closed++
	s.mu.Unlock()
	return approval.Answer{}, ctx.Err()
}

func (s *scriptSurface) answer(a ...approval.Answer) {
	s.mu.Lock()
	s.answers = append(s.answers, a...)
	s.mu.Unlock()
}

func (s *scriptSurface) shown() []approval.Prompt {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]approval.Prompt(nil), s.prompts...)
}

// gateProv is a fake provider with one tool per gate behaviour.
type gateProv struct {
	mu    sync.Mutex
	calls []string
}

func (p *gateProv) Name() string { return "fake" }

func (p *gateProv) Tools() []provider.Tool {
	def := func(name string) *mcp.Tool {
		return &mcp.Tool{Name: name, Description: name,
			InputSchema: json.RawMessage(`{"type":"object","properties":{"cmd":{"type":"string"},"input":{"type":"string"}}}`)}
	}
	return []provider.Tool{
		{Def: def("fake_info"), Class: provider.ClassInfo},
		{Def: def("fake_run"), Class: provider.ClassExec},
		{Def: def("fake_job"), Class: provider.ClassExec},
		{Def: def("fake_svc"), Class: provider.ClassService},
	}
}

type fakeArgs struct{ Cmd, Input string }

func (p *gateProv) Approval(_ context.Context, tool string, args json.RawMessage, _ provider.Caller) (provider.Approval, error) {
	var a fakeArgs
	if err := json.Unmarshal(args, &a); err != nil {
		return provider.Approval{}, err
	}
	if a.Cmd == "invalid" {
		return provider.Approval{}, errors.New("cmd is not allowed")
	}
	sum := sha256.Sum256(args)
	return provider.Approval{
		Title:    "Run " + tool,
		Details:  []provider.Detail{{Label: "Command", Value: a.Cmd}},
		Exact:    tool + ":" + hex.EncodeToString(sum[:]),
		Scopes:   []provider.Scope{{Key: tool + ":cmd:" + a.Cmd, Label: "this command with any input"}},
		Deferred: tool == "fake_job",
	}, nil
}

func (p *gateProv) Call(ctx context.Context, tool string, args json.RawMessage, c provider.Caller) (*mcp.CallToolResult, error) {
	p.mu.Lock()
	p.calls = append(p.calls, tool+" from "+c.DeviceName+"/"+c.Agent)
	p.mu.Unlock()
	if tool == "fake_job" {
		tk, ok := provider.TicketFrom(ctx)
		if !ok {
			return nil, errors.New("deferred call without a ticket")
		}
		go func() {
			// The job runs once approved, long after the call returned.
			if ok, _ := tk.Wait(context.Background()); ok {
				p.mu.Lock()
				p.calls = append(p.calls, "job ran")
				p.mu.Unlock()
			}
		}()
		return provider.JSONResult(map[string]string{"job": "queued", "ticket": tk.ID()})
	}
	var a fakeArgs
	json.Unmarshal(args, &a)
	if a.Cmd == "fail" {
		return provider.ErrorResult("command failed"), nil
	}
	return provider.JSONResult(map[string]string{"ran": a.Cmd})
}

func (p *gateProv) callLog() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.calls...)
}

// bareProv has an exec tool but no approval gate.
type bareProv struct{}

func (bareProv) Name() string { return "bare" }
func (bareProv) Tools() []provider.Tool {
	return []provider.Tool{{Class: provider.ClassExec, Def: &mcp.Tool{Name: "bare_run", Description: "x",
		InputSchema: json.RawMessage(`{"type":"object"}`)}}}
}
func (bareProv) Call(context.Context, string, json.RawMessage, provider.Caller) (*mcp.CallToolResult, error) {
	return provider.JSONResult("ran")
}

func startGateNode(t *testing.T, name string, surface approval.Surface, provs ...provider.Provider) *Node {
	t.Helper()
	n, err := Start(t.Context(), Options{
		Paths:           state.Paths{Root: t.TempDir()},
		Name:            name,
		MeshAddr:        "127.0.0.1:0",
		LocalAddr:       "127.0.0.1:0",
		Logger:          slog.New(slog.DiscardHandler),
		ApprovalSurface: surface,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	for _, p := range provs {
		n.register(p)
	}
	n.rebuildTools()
	return n
}

func callTool(t *testing.T, s *mcp.ClientSession, name string, args any) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func resultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func waitForTool(t *testing.T, s *mcp.ClientSession, name string) {
	t.Helper()
	waitFor(t, name+" to reach the gateway", func() bool {
		for tool, err := range s.Tools(t.Context(), nil) {
			if err == nil && tool.Name == name {
				return true
			}
		}
		return false
	})
}

func TestPeerCallsAreApprovedOnTheHost(t *testing.T) {
	surface := &scriptSurface{}
	fake := &gateProv{}
	desktop := startGateNode(t, "desktop", surface, fake)
	raspi := startGateNode(t, "raspi", &scriptSurface{})
	pair(t, desktop, raspi)
	s := agentSession(t, raspi)
	waitForTool(t, s, "desktop__fake_run")

	// Info tools never prompt.
	if res := callTool(t, s, "desktop__fake_info", map[string]any{}); res.IsError {
		t.Fatalf("info tool: %s", resultText(res))
	}
	if res := callTool(t, s, "desktop__node_info", map[string]any{}); res.IsError {
		t.Fatalf("node_info: %s", resultText(res))
	}
	if len(surface.shown()) != 0 {
		t.Fatalf("info tools prompted: %+v", surface.shown())
	}

	// Denied on the host: the agent gets a readable error and nothing runs.
	surface.answer(approval.Answer{})
	res := callTool(t, s, "desktop__fake_run", map[string]any{"cmd": "make"})
	if !res.IsError || !strings.Contains(resultText(res), "denied on desktop: denied by the user") {
		t.Fatalf("denied call = error %v: %q", res.IsError, resultText(res))
	}
	if calls := fake.callLog(); len(calls) != 1 { // only the earlier info call
		t.Fatalf("a denied call ran: %v", calls)
	}
	p := surface.shown()[0]
	if p.Title != "Run fake_run" || p.Caller.DeviceName != "raspi" || p.Caller.DeviceID != raspi.ID() || p.Caller.Agent != cliAgent ||
		len(p.Details) != 1 || p.Details[0].Value != "make" || len(p.Scopes) != 2 {
		t.Fatalf("prompt = %+v", p)
	}

	// Allowed once: runs, and the same call is asked about again.
	surface.answer(approval.Answer{Allow: true})
	res = callTool(t, s, "desktop__fake_run", map[string]any{"cmd": "make"})
	if res.IsError || !strings.Contains(resultText(res), `"ran": "make"`) {
		t.Fatalf("allowed call: %s", resultText(res))
	}
	surface.answer(approval.Answer{Allow: true, Always: true, Scope: 1})
	if res = callTool(t, s, "desktop__fake_run", map[string]any{"cmd": "make"}); res.IsError {
		t.Fatalf("always call: %s", resultText(res))
	}
	if n := len(surface.shown()); n != 3 {
		t.Fatalf("%d prompts so far, want 3 (allow once does not remember)", n)
	}

	// The saved rule covers the command with other input, with no prompt.
	if res = callTool(t, s, "desktop__fake_run", map[string]any{"cmd": "make", "input": "other"}); res.IsError {
		t.Fatalf("rule-covered call: %s", resultText(res))
	}
	if n := len(surface.shown()); n != 3 {
		t.Fatalf("rule-covered call prompted (%d prompts)", n)
	}
	// A different command, and a different tool, are still asked about.
	surface.answer(approval.Answer{}, approval.Answer{})
	if res = callTool(t, s, "desktop__fake_run", map[string]any{"cmd": "rm"}); !res.IsError {
		t.Fatal("different command ran on another command's rule")
	}
	if res = callTool(t, s, "desktop__fake_svc", map[string]any{"cmd": "make"}); !res.IsError {
		t.Fatal("service-class call ran on an exec rule")
	}

	// A command the provider cannot describe is refused without any prompt.
	before := len(surface.shown())
	res = callTool(t, s, "desktop__fake_run", map[string]any{"cmd": "invalid"})
	if !res.IsError || !strings.Contains(resultText(res), "cmd is not allowed") || len(surface.shown()) != before {
		t.Fatalf("undescribable call: error=%v %q prompts %d->%d", res.IsError, resultText(res), before, len(surface.shown()))
	}

	// The tool's own failure is audited as such.
	surface.answer(approval.Answer{Allow: true})
	callTool(t, s, "desktop__fake_run", map[string]any{"cmd": "fail"})

	if got := fake.callLog(); len(got) != 5 || got[0] != "fake_info from raspi/cli" || got[1] != "fake_run from raspi/cli" {
		t.Fatalf("provider calls = %v", got)
	}

	recs, err := desktop.approvals.Audit(100)
	if err != nil {
		t.Fatal(err)
	}
	var decisions, completions []string
	for _, r := range recs {
		if r.Device != "raspi" || r.DeviceID != raspi.ID() || r.Agent != cliAgent || r.Tool == "" || r.Exact == "" {
			t.Fatalf("audit record missing the caller or request: %+v", r)
		}
		switch r.Kind {
		case approval.KindDecision:
			decisions = append(decisions, r.Decision)
		case approval.KindCompletion:
			ok := "ok"
			if r.OK == nil || !*r.OK {
				ok = "failed"
			}
			completions = append(completions, ok)
		}
	}
	if got := strings.Join(decisions, ","); got != "deny,allow-once,allow-always,allow-rule,deny,deny,allow-once" {
		t.Fatalf("audit decisions = %s", got)
	}
	if got := strings.Join(completions, ","); got != "ok,ok,ok,failed" {
		t.Fatalf("audit completions = %s", got)
	}
}

func TestGateRefusesGatedClassWithoutGate(t *testing.T) {
	surface := &scriptSurface{}
	desktop := startGateNode(t, "desktop", surface, bareProv{})
	res := desktop.dispatch(t.Context(), "bare_run", json.RawMessage(`{}`), provider.Caller{DeviceID: "x", DeviceName: "x"})
	if !res.IsError || !strings.Contains(resultText(res), "no approval gate") || len(surface.shown()) != 0 {
		t.Fatalf("result = %v %q", res.IsError, resultText(res))
	}
}

func TestDeferredCallsStartWhenApproved(t *testing.T) {
	surface := &scriptSurface{}
	fake := &gateProv{}
	desktop := startGateNode(t, "desktop", surface, fake)
	raspi := startGateNode(t, "raspi", &scriptSurface{})
	pair(t, desktop, raspi)
	s := agentSession(t, raspi)
	waitForTool(t, s, "desktop__fake_job")

	// The call returns at once even though nobody has answered yet.
	res := callTool(t, s, "desktop__fake_job", map[string]any{"cmd": "train"})
	if res.IsError {
		t.Fatalf("deferred call: %s", resultText(res))
	}
	waitFor(t, "the prompt", func() bool { return len(desktop.approvals.Pending()) == 1 })
	if got := fake.callLog(); len(got) != 1 {
		t.Fatalf("job ran before approval: %v", got)
	}
	if err := desktop.approvals.Resolve(desktop.approvals.Pending()[0].ID, approval.Answer{Allow: true}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the job to start", func() bool { return len(fake.callLog()) == 2 })
}

func TestControlAPIResolvesPendingApprovals(t *testing.T) {
	fake := &gateProv{}
	desktop := startGateNode(t, "desktop", approval.Headless{}, fake)
	raspi := startGateNode(t, "raspi", &scriptSurface{})
	pair(t, desktop, raspi)
	s := agentSession(t, raspi)
	waitForTool(t, s, "desktop__fake_run")

	api := &control.Client{Base: "http://" + desktop.LocalAddr(), Token: desktop.controlToken, HTTP: http.DefaultClient}
	bad := &control.Client{Base: api.Base, Token: "not-the-token", HTTP: http.DefaultClient}

	done := make(chan *mcp.CallToolResult, 1)
	go func() { done <- callTool(t, s, "desktop__fake_run", map[string]any{"cmd": "build"}) }()

	var pending []approval.Pending
	waitFor(t, "the request to be listed", func() bool {
		return api.Do(t.Context(), http.MethodGet, "/v1/approvals", nil, &pending) == nil && len(pending) == 1
	})
	if p := pending[0]; p.Device != "raspi" || p.Agent != cliAgent || p.Class != "exec" || len(p.Scopes) != 2 {
		t.Fatalf("pending = %+v", p)
	}
	if err := bad.Do(t.Context(), http.MethodGet, "/v1/approvals", nil, nil); err == nil {
		t.Fatal("approvals listed without the control token")
	}
	if err := bad.Do(t.Context(), http.MethodPost, "/v1/approvals/"+pending[0].ID, control.ApprovalAnswer{Decision: "allow"}, nil); err == nil {
		t.Fatal("approval answered without the control token")
	}
	if len(desktop.approvals.Pending()) != 1 {
		t.Fatal("an unauthorised answer resolved the request")
	}
	if err := api.Do(t.Context(), http.MethodPost, "/v1/approvals/"+pending[0].ID, control.ApprovalAnswer{Decision: "bogus"}, nil); err == nil {
		t.Fatal("bogus decision accepted")
	}
	if err := api.Do(t.Context(), http.MethodPost, "/v1/approvals/"+pending[0].ID, control.ApprovalAnswer{Decision: "always", Scope: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if res := <-done; res.IsError {
		t.Fatalf("call after approval: %s", resultText(res))
	}
	if err := api.Do(t.Context(), http.MethodPost, "/v1/approvals/"+pending[0].ID, control.ApprovalAnswer{Decision: "allow"}, nil); err == nil {
		t.Fatal("answering an already-decided request succeeded")
	}

	var rules []control.Rule
	if err := api.Do(t.Context(), http.MethodGet, "/v1/rules", nil, &rules); err != nil || len(rules) != 1 ||
		rules[0].Device != "raspi" || rules[0].Agent != cliAgent || rules[0].Class != provider.ClassExec {
		t.Fatalf("rules = %+v, %v", rules, err)
	}
	var audit []control.AuditRecord
	if err := api.Do(t.Context(), http.MethodGet, "/v1/audit?n=10", nil, &audit); err != nil || len(audit) != 2 ||
		audit[0].Decision != approval.OutcomeAllowAlways || audit[0].Surface != "control" || audit[1].Kind != approval.KindCompletion {
		t.Fatalf("audit = %+v, %v", audit, err)
	}
	if err := api.Do(t.Context(), http.MethodDelete, "/v1/rules/"+rules[0].ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := api.Do(t.Context(), http.MethodDelete, "/v1/rules/"+rules[0].ID, nil, nil); err == nil {
		t.Fatal("removing a removed rule succeeded")
	}
	if err := api.Do(t.Context(), http.MethodGet, "/v1/rules", nil, &rules); err != nil || len(rules) != 0 {
		t.Fatalf("rules after removal = %+v, %v", rules, err)
	}
}

func TestCallerHangupWithdrawsThePrompt(t *testing.T) {
	surface := &scriptSurface{}
	desktop := startGateNode(t, "desktop", surface, &gateProv{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan *mcp.CallToolResult, 1)
	go func() {
		done <- desktop.dispatch(ctx, "fake_run", json.RawMessage(`{"cmd":"x"}`), provider.Caller{DeviceID: "id", DeviceName: "laptop", Agent: "a"})
	}()
	waitFor(t, "the prompt", func() bool { return len(surface.shown()) == 1 })
	cancel()
	if res := <-done; !res.IsError {
		t.Fatal("cancelled call succeeded")
	}
	waitFor(t, "the dialog to close", func() bool {
		surface.mu.Lock()
		defer surface.mu.Unlock()
		return surface.closed == 1
	})
	if len(desktop.approvals.Pending()) != 0 {
		t.Fatal("request still pending")
	}
}
