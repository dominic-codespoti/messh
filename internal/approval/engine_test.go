package approval

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"messh/internal/provider"
	"messh/internal/state"
)

// fakeSurface lets a test play the person at the host: every prompt is
// published on asked, and the test answers through reply.
type fakeSurface struct {
	asked  chan Prompt
	reply  chan Answer
	closed chan string // IDs of prompts whose context ended before an answer
	fail   error
}

func newFake() *fakeSurface {
	return &fakeSurface{asked: make(chan Prompt, 16), reply: make(chan Answer), closed: make(chan string, 16)}
}

func (f *fakeSurface) Name() string    { return "fake" }
func (f *fakeSurface) Available() bool { return true }
func (f *fakeSurface) Ask(ctx context.Context, p Prompt) (Answer, error) {
	f.asked <- p
	if f.fail != nil {
		return Answer{}, f.fail
	}
	select {
	case a := <-f.reply:
		return a, nil
	case <-ctx.Done():
		f.closed <- p.ID
		return Answer{}, ctx.Err()
	}
}

func (f *fakeSurface) nextPrompt(t *testing.T) Prompt {
	t.Helper()
	select {
	case p := <-f.asked:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("no prompt was shown")
		return Prompt{}
	}
}

func (f *fakeSurface) answer(t *testing.T, a Answer) {
	t.Helper()
	select {
	case f.reply <- a:
	case <-time.After(5 * time.Second):
		t.Fatal("prompt was not waiting for an answer")
	}
}

func (f *fakeSurface) noPrompt(t *testing.T) {
	t.Helper()
	select {
	case p := <-f.asked:
		t.Fatalf("unexpected prompt %q", p.Title)
	case <-time.After(100 * time.Millisecond):
	}
}

func newEngine(t *testing.T, s Surface, mod ...func(*Options)) *Engine {
	t.Helper()
	return newEngineAt(t, state.Paths{Root: t.TempDir()}, s, mod...)
}

func newEngineAt(t *testing.T, paths state.Paths, s Surface, mod ...func(*Options)) *Engine {
	t.Helper()
	opts := Options{Paths: paths, Surface: s, Log: slog.New(slog.DiscardHandler)}
	for _, m := range mod {
		m(&opts)
	}
	e, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.Close)
	return e
}

var (
	argvScope = provider.Scope{Key: "exec:argv:aaa", Label: "this command with any input"}
	anyScope  = provider.Scope{Key: "exec:any", Label: "any command", Broad: true}
)

func mkReq(device, agent, exact string) Request {
	return Request{
		Caller: provider.Caller{DeviceID: "id-" + device, DeviceName: device, Agent: agent},
		Tool:   "run",
		Class:  provider.ClassExec,
		Approval: provider.Approval{
			Title:   "Run a job",
			Details: []provider.Detail{{Label: "Command", Value: "python " + exact}},
			Exact:   exact,
			Scopes:  []provider.Scope{argvScope, anyScope},
		},
	}
}

// decide runs Decide in the background and returns its result channel.
func decide(e *Engine, ctx context.Context, req Request) <-chan struct {
	D   Decision
	Err error
} {
	ch := make(chan struct {
		D   Decision
		Err error
	}, 1)
	go func() {
		d, err := e.Decide(ctx, req)
		ch <- struct {
			D   Decision
			Err error
		}{d, err}
	}()
	return ch
}

func get[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a result")
		panic("unreachable")
	}
}

func TestAllowOnceApprovesOnlyThatRequest(t *testing.T) {
	f := newFake()
	e := newEngine(t, f)
	res := decide(e, t.Context(), mkReq("raspi", "omp", "one"))
	p := f.nextPrompt(t)
	if p.Title != "Run a job" || p.Caller.DeviceName != "raspi" || p.Caller.Agent != "omp" || len(p.Details) != 1 {
		t.Fatalf("prompt = %+v", p)
	}
	if len(p.Scopes) != 3 || p.Scopes[0].Key != "exact:one" {
		t.Fatalf("scopes = %+v, want exact first then the provider's two", p.Scopes)
	}
	f.answer(t, Answer{Allow: true})
	r := get(t, res)
	if r.Err != nil || !r.D.Allowed || r.D.Outcome != OutcomeAllowOnce {
		t.Fatalf("decision = %+v, %v", r.D, r.Err)
	}
	if len(e.Rules()) != 0 {
		t.Fatalf("allow once saved a rule: %+v", e.Rules())
	}

	// The very same request is asked about again.
	res = decide(e, t.Context(), mkReq("raspi", "omp", "one"))
	f.nextPrompt(t)
	f.answer(t, Answer{})
	if r := get(t, res); r.D.Allowed || r.D.Outcome != OutcomeDeny || r.D.Reason != "denied by the user" {
		t.Fatalf("deny decision = %+v", r.D)
	}
	if len(e.Pending()) != 0 {
		t.Fatalf("pending after decisions: %+v", e.Pending())
	}
}

func TestAlwaysSavesRuleForChosenScope(t *testing.T) {
	for _, tc := range []struct {
		name      string
		scope     int
		sameExact bool // does a request with the same exact hash match?
		sameArgv  bool // does one with a different hash but the same scope key match?
	}{
		{"exact", 0, true, false},
		{"argv", 1, true, true},
		{"any", 2, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			e := newEngine(t, f)
			res := decide(e, t.Context(), mkReq("raspi", "omp", "first"))
			f.nextPrompt(t)
			f.answer(t, Answer{Allow: true, Always: true, Scope: tc.scope})
			r := get(t, res)
			if !r.D.Allowed || r.D.Outcome != OutcomeAllowAlways || r.D.RuleID == "" {
				t.Fatalf("decision = %+v", r.D)
			}
			rules := e.Rules()
			if len(rules) != 1 || rules[0].ID != r.D.RuleID || rules[0].DeviceID != "id-raspi" ||
				rules[0].Device != "raspi" || rules[0].Agent != "omp" || rules[0].Class != provider.ClassExec ||
				rules[0].Created.IsZero() || rules[0].Label == "" {
				t.Fatalf("rule = %+v", rules)
			}

			// A different request that shares no scope key: exec:any only appears in
			// the scopes of mkReq requests, so build one without it.
			other := mkReq("raspi", "omp", "other")
			other.Approval.Scopes = []provider.Scope{{Key: "exec:unrelated", Label: "x"}}
			sameArgv := mkReq("raspi", "omp", "second") // shares argv and any scopes

			for _, c := range []struct {
				name string
				req  Request
				want bool
			}{
				{"same exact", mkReq("raspi", "omp", "first"), tc.sameExact},
				{"same scope key", sameArgv, tc.sameArgv},
				{"unrelated", other, false},
			} {
				ch := decide(e, t.Context(), c.req)
				if c.want {
					got := get(t, ch)
					if !got.D.Allowed || got.D.Outcome != OutcomeAllowRule || got.D.RuleID != r.D.RuleID {
						t.Fatalf("%s: decision = %+v, want allowed by rule", c.name, got.D)
					}
					f.noPrompt(t)
					continue
				}
				f.nextPrompt(t)
				f.answer(t, Answer{})
				if got := get(t, ch); got.D.Allowed {
					t.Fatalf("%s: allowed without a matching rule", c.name)
				}
			}
		})
	}
}

func TestRulesAreScopedToDeviceAgentAndClass(t *testing.T) {
	f := newFake()
	e := newEngine(t, f)
	res := decide(e, t.Context(), mkReq("raspi", "omp", "job"))
	f.nextPrompt(t)
	f.answer(t, Answer{Allow: true, Always: true, Scope: 2}) // the broad scope
	get(t, res)

	service := mkReq("raspi", "omp", "job")
	service.Class = provider.ClassService
	for name, req := range map[string]Request{
		"other agent":  mkReq("raspi", "pi", "job"),
		"other device": mkReq("laptop", "omp", "job"),
		"other class":  service,
	} {
		ch := decide(e, t.Context(), req)
		p := f.nextPrompt(t)
		if p.Caller != req.Caller {
			t.Fatalf("%s: prompt for %+v", name, p.Caller)
		}
		f.answer(t, Answer{})
		if get(t, ch).D.Allowed {
			t.Fatalf("%s was allowed by another caller's rule", name)
		}
	}
	// And the rule still serves its own caller.
	if d := get(t, decide(e, t.Context(), mkReq("raspi", "omp", "different"))).D; !d.Allowed {
		t.Fatalf("own caller no longer matched: %+v", d)
	}
}

func TestUnansweredRequestExpires(t *testing.T) {
	f := newFake()
	e := newEngine(t, f, func(o *Options) { o.Timeout = 50 * time.Millisecond })
	r := get(t, decide(e, t.Context(), mkReq("raspi", "omp", "slow")))
	if r.Err != nil || r.D.Allowed || r.D.Outcome != OutcomeExpired || r.D.Reason != "timed out waiting for approval" {
		t.Fatalf("decision = %+v, %v", r.D, r.Err)
	}
	f.nextPrompt(t)
	select {
	case <-f.closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the prompt stayed open after the request expired")
	}
	if len(e.Rules()) != 0 || len(e.Pending()) != 0 {
		t.Fatalf("expiry left state behind: rules %v pending %v", e.Rules(), e.Pending())
	}
}

func TestCancellingTheCallClosesItsPrompt(t *testing.T) {
	f := newFake()
	e := newEngine(t, f)
	ctx, cancel := context.WithCancel(t.Context())
	res := decide(e, ctx, mkReq("raspi", "omp", "gone"))
	p := f.nextPrompt(t)
	cancel()
	r := get(t, res)
	if !errors.Is(r.Err, context.Canceled) || r.D.Allowed || r.D.Outcome != OutcomeCancelled {
		t.Fatalf("decision = %+v, %v", r.D, r.Err)
	}
	if id := get(t, f.closed); id != p.ID {
		t.Fatalf("closed prompt %s, shown %s", id, p.ID)
	}
	if len(e.Pending()) != 0 {
		t.Fatal("cancelled request still pending")
	}
}

func TestPromptsAreShownOneAtATime(t *testing.T) {
	f := newFake()
	e := newEngine(t, f)
	a := decide(e, t.Context(), mkReq("raspi", "omp", "a"))
	first := f.nextPrompt(t)
	b := decide(e, t.Context(), mkReq("raspi", "omp", "b"))
	waitUntil(t, "second request to queue", func() bool { return len(e.Pending()) == 2 })
	f.noPrompt(t) // b must wait for a's prompt to be answered

	pend := e.Pending()
	if !pend[0].Shown || pend[1].Shown {
		t.Fatalf("shown flags = %v %v, want only the first shown", pend[0].Shown, pend[1].Shown)
	}
	f.answer(t, Answer{Allow: true})
	if !get(t, a).D.Allowed {
		t.Fatal("a not allowed")
	}
	second := f.nextPrompt(t)
	if second.Title == "" || second.ID == first.ID {
		t.Fatalf("second prompt = %+v", second)
	}
	f.answer(t, Answer{})
	if get(t, b).D.Allowed {
		t.Fatal("b allowed")
	}
}

func TestQueuedBehindPromptReportsWaiting(t *testing.T) {
	f := newFake()
	e := newEngine(t, f)
	// Queue three before the surface starts answering: hold the first prompt
	// open, then add two more and check the next prompt sees one still behind it.
	r1 := decide(e, t.Context(), mkReq("raspi", "omp", "1"))
	f.nextPrompt(t)
	r2 := decide(e, t.Context(), mkReq("raspi", "omp", "2"))
	r3 := decide(e, t.Context(), mkReq("raspi", "omp", "3"))
	waitUntil(t, "requests to queue", func() bool { return len(e.Pending()) == 3 })
	f.answer(t, Answer{Allow: true})
	get(t, r1)
	p2 := f.nextPrompt(t)
	if p2.Waiting != 1 {
		t.Fatalf("second prompt Waiting = %d, want 1", p2.Waiting)
	}
	f.answer(t, Answer{Allow: true})
	if p3 := f.nextPrompt(t); p3.Waiting != 0 {
		t.Fatalf("last prompt Waiting = %d, want 0", p3.Waiting)
	}
	f.answer(t, Answer{Allow: true})
	get(t, r2)
	get(t, r3)
}

func TestPendingCapIsPerDeviceAndAgent(t *testing.T) {
	e := newEngine(t, Headless{}, func(o *Options) { o.MaxPendingPerAgent = 2 })
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	for _, x := range []string{"1", "2"} {
		decide(e, ctx, mkReq("raspi", "omp", x))
	}
	waitUntil(t, "two pending", func() bool { return len(e.Pending()) == 2 })

	if _, err := e.Decide(ctx, mkReq("raspi", "omp", "3")); err == nil {
		t.Fatal("third pending request from the same agent was accepted")
	}
	tk := e.Submit(mkReq("raspi", "omp", "4"))
	if ok, err := tk.Wait(ctx); ok || err == nil {
		t.Fatalf("over-cap ticket = %v, %v; want denied", ok, err)
	}
	// Other agents and devices are not starved by it.
	decide(e, ctx, mkReq("raspi", "pi", "5"))
	decide(e, ctx, mkReq("laptop", "omp", "6"))
	waitUntil(t, "unrelated callers to queue", func() bool { return len(e.Pending()) == 4 })
	if recs, _ := e.Audit(100); !hasDecision(recs, OutcomeDeny, "too many") {
		t.Fatalf("cap rejection missing from audit: %+v", recs)
	}
}

func TestDeferredTicketWaitsForTheDecision(t *testing.T) {
	f := newFake()
	e := newEngine(t, f)
	tk := e.Submit(mkReq("raspi", "omp", "job"))
	waited := make(chan struct {
		ok  bool
		err error
	}, 1)
	go func() {
		ok, err := tk.Wait(t.Context())
		waited <- struct {
			ok  bool
			err error
		}{ok, err}
	}()
	f.nextPrompt(t)
	select {
	case <-waited:
		t.Fatal("Wait returned before an answer")
	case <-time.After(100 * time.Millisecond):
	}
	f.answer(t, Answer{Allow: true})
	if w := get(t, waited); !w.ok || w.err != nil {
		t.Fatalf("Wait = %v, %v", w.ok, w.err)
	}
	if ok, err := tk.Wait(t.Context()); !ok || err != nil {
		t.Fatal("approved ticket stopped being approved")
	}
	tk.Cancel() // after the decision: harmless
	if ok, _ := tk.Wait(t.Context()); !ok {
		t.Fatal("Cancel after approval revoked it")
	}
}

func TestTicketCancelWithdrawsTheRequest(t *testing.T) {
	f := newFake()
	e := newEngine(t, f)
	tk := e.Submit(mkReq("raspi", "omp", "job"))
	f.nextPrompt(t)
	tk.Cancel()
	tk.Cancel()
	ok, err := tk.Wait(t.Context())
	var denied *DeniedError
	if ok || !errors.As(err, &denied) || denied.Decision.Outcome != OutcomeCancelled {
		t.Fatalf("Wait = %v, %v", ok, err)
	}
	get(t, f.closed)
	if len(e.Pending()) != 0 {
		t.Fatal("cancelled ticket still pending")
	}
}

func TestSubmitMatchingRuleIsAlreadyApproved(t *testing.T) {
	f := newFake()
	e := newEngine(t, f)
	res := decide(e, t.Context(), mkReq("raspi", "omp", "a"))
	f.nextPrompt(t)
	f.answer(t, Answer{Allow: true, Always: true, Scope: 1})
	get(t, res)

	tk := e.Submit(mkReq("raspi", "omp", "b"))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if ok, err := tk.Wait(ctx); !ok || err != nil {
		t.Fatalf("Wait = %v, %v", ok, err)
	}
	f.noPrompt(t)
}

func TestAutoRunsWithoutPromptAndIsAudited(t *testing.T) {
	f := newFake()
	e := newEngine(t, f)
	req := mkReq("raspi", "omp", "read")
	req.Class = provider.ClassService
	req.Approval.Auto = "services.json: voicestudio auto_read"
	d, err := e.Decide(t.Context(), req)
	if err != nil || !d.Allowed || d.Outcome != OutcomeAllowAuto || d.Auto != req.Approval.Auto {
		t.Fatalf("decision = %+v, %v", d, err)
	}
	f.noPrompt(t)
	recs, err := e.Audit(10)
	if err != nil || len(recs) != 1 || recs[0].Decision != OutcomeAllowAuto || recs[0].Auto != req.Approval.Auto {
		t.Fatalf("audit = %+v, %v", recs, err)
	}
	if tk := e.Submit(req); func() bool { ok, _ := tk.Wait(t.Context()); return !ok }() {
		t.Fatal("Auto ticket not approved")
	}
}

func TestAuditRecordsDecisionAndCompletion(t *testing.T) {
	f := newFake()
	e := newEngine(t, f)
	req := mkReq("raspi", "omp", "audited")
	res := decide(e, t.Context(), req)
	f.nextPrompt(t)
	f.answer(t, Answer{Allow: true})
	d := get(t, res).D
	e.Complete(d, req, false, "exit status 3", 1500*time.Millisecond)

	res = decide(e, t.Context(), mkReq("laptop", "pi", "denied-one"))
	f.nextPrompt(t)
	f.answer(t, Answer{})
	get(t, res)

	recs, err := e.Audit(10)
	if err != nil || len(recs) != 3 {
		t.Fatalf("audit = %+v, %v", recs, err)
	}
	dec, comp, den := recs[0], recs[1], recs[2]
	if dec.Kind != KindDecision || dec.ID != d.ID || dec.DeviceID != "id-raspi" || dec.Device != "raspi" || dec.Agent != "omp" ||
		dec.Tool != "run" || dec.Class != "exec" || dec.Title != "Run a job" || dec.Exact != "audited" ||
		dec.Decision != OutcomeAllowOnce || dec.Surface != "fake" || dec.Time.IsZero() {
		t.Fatalf("decision record = %+v", dec)
	}
	if comp.Kind != KindCompletion || comp.ID != d.ID || comp.OK == nil || *comp.OK || comp.Error != "exit status 3" || comp.DurationMS != 1500 {
		t.Fatalf("completion record = %+v", comp)
	}
	if den.Decision != OutcomeDeny || den.Agent != "pi" || den.Device != "laptop" || den.Reason == "" {
		t.Fatalf("deny record = %+v", den)
	}
	if last, _ := e.Audit(1); len(last) != 1 || last[0].ID != den.ID {
		t.Fatalf("Audit(1) = %+v", last)
	}
}

func TestAuditRollsOver(t *testing.T) {
	f := newFake()
	e := newEngine(t, f)
	e.audit.max = 600
	for i := range 6 {
		d, err := e.Decide(t.Context(), withAuto(mkReq("raspi", "omp", "r"+string(rune('a'+i)))))
		if err != nil || !d.Allowed {
			t.Fatal(d, err)
		}
	}
	if _, err := os.Stat(e.audit.path + ".1"); err != nil {
		t.Fatalf("audit did not roll over: %v", err)
	}
	if fi, _ := os.Stat(e.audit.path); fi.Size() > 600 {
		t.Fatalf("current audit file is %d bytes, cap 600", fi.Size())
	}
	// History spans the roll-over.
	if recs, _ := e.Audit(100); len(recs) < 4 {
		t.Fatalf("only %d records readable across the roll-over", len(recs))
	}
}

func TestRulesPersistAcrossRestarts(t *testing.T) {
	paths := state.Paths{Root: t.TempDir()}
	f := newFake()
	e := newEngineAt(t, paths, f)
	res := decide(e, t.Context(), mkReq("raspi", "omp", "first"))
	f.nextPrompt(t)
	f.answer(t, Answer{Allow: true, Always: true, Scope: 1})
	rule := get(t, res).D.RuleID
	e.Close()

	f2 := newFake()
	e2 := newEngineAt(t, paths, f2)
	if rs := e2.Rules(); len(rs) != 1 || rs[0].ID != rule {
		t.Fatalf("rules after restart = %+v", rs)
	}
	d, err := e2.Decide(t.Context(), mkReq("raspi", "omp", "second"))
	if err != nil || !d.Allowed || d.Outcome != OutcomeAllowRule {
		t.Fatalf("decision after restart = %+v, %v", d, err)
	}
	f2.noPrompt(t)

	if err := e2.RemoveRule("nope"); !errors.Is(err, ErrNoRule) {
		t.Fatalf("RemoveRule(unknown) = %v", err)
	}
	if err := e2.RemoveRule(rule); err != nil {
		t.Fatal(err)
	}
	e3 := newEngineAt(t, paths, newFake())
	if len(e3.Rules()) != 0 {
		t.Fatal("removed rule came back after restart")
	}
}

func TestCorruptRulesFileIsAnError(t *testing.T) {
	paths := state.Paths{Root: t.TempDir()}
	if err := os.WriteFile(paths.RulesFile(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Options{Paths: paths}); err == nil {
		t.Fatal("a corrupt rules file must not silently become 'no rules'")
	}
}

func TestHeadlessRequestsAreResolvedExternally(t *testing.T) {
	e := newEngine(t, Headless{})
	res := decide(e, t.Context(), mkReq("raspi", "omp", "remote"))
	waitUntil(t, "request to be pending", func() bool { return len(e.Pending()) == 1 })
	p := e.Pending()[0]
	if p.Device != "raspi" || p.Agent != "omp" || p.Class != "exec" || len(p.Scopes) != 3 || !p.Scopes[2].Broad || len(p.Details) != 1 {
		t.Fatalf("pending = %+v", p)
	}
	if err := e.Resolve("nosuch", Answer{Allow: true}); !errors.Is(err, ErrNotPending) {
		t.Fatalf("Resolve(unknown) = %v", err)
	}
	if err := e.Resolve(p.ID, Answer{Allow: true, Always: true, Scope: 9}); err == nil {
		t.Fatal("out-of-range scope accepted")
	}
	if len(e.Pending()) != 1 {
		t.Fatal("a rejected answer must leave the request pending")
	}
	if err := e.Resolve(p.ID, Answer{Allow: true, Always: true, Scope: 1}); err != nil {
		t.Fatal(err)
	}
	if d := get(t, res).D; !d.Allowed || d.Outcome != OutcomeAllowAlways {
		t.Fatalf("decision = %+v", d)
	}
	if recs, _ := e.Audit(5); len(recs) != 1 || recs[0].Surface != "control" {
		t.Fatalf("audit = %+v", recs)
	}
}

func TestResolveClosesTheOpenPrompt(t *testing.T) {
	f := newFake()
	e := newEngine(t, f)
	res := decide(e, t.Context(), mkReq("raspi", "omp", "x"))
	p := f.nextPrompt(t)
	if err := e.Resolve(p.ID, Answer{}); err != nil {
		t.Fatal(err)
	}
	get(t, f.closed)
	if get(t, res).D.Allowed {
		t.Fatal("denied request was allowed")
	}
}

func TestFailedPromptDenies(t *testing.T) {
	f := newFake()
	f.fail = errors.New("no display")
	e := newEngine(t, f)
	d := get(t, decide(e, t.Context(), mkReq("raspi", "omp", "x"))).D
	if d.Allowed || d.Outcome != OutcomeDeny {
		t.Fatalf("decision = %+v", d)
	}
}

func TestRequestsWithoutIdentityOrHashAreRefused(t *testing.T) {
	e := newEngine(t, Headless{})
	noHash := mkReq("raspi", "omp", "")
	noDevice := mkReq("raspi", "omp", "x")
	noDevice.Caller.DeviceID = ""
	info := mkReq("raspi", "omp", "x")
	info.Class = provider.ClassInfo
	for name, req := range map[string]Request{"no exact hash": noHash, "no device": noDevice, "info class": info} {
		if _, err := e.Decide(t.Context(), req); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestCloseDeniesPending(t *testing.T) {
	f := newFake()
	e := newEngine(t, f)
	res := decide(e, t.Context(), mkReq("raspi", "omp", "x"))
	f.nextPrompt(t)
	e.Close()
	if d := get(t, res).D; d.Allowed || d.Outcome != OutcomeCancelled {
		t.Fatalf("decision = %+v", d)
	}
	if _, err := e.Decide(t.Context(), mkReq("raspi", "omp", "y")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Decide after Close = %v", err)
	}
}

func TestSanitizeKeepsUntrustedTextOnOneLine(t *testing.T) {
	got := Sanitize("a\nFrom: desktop (agent \"user\")\x00\x1b[2J\u202e", 200)
	for _, r := range got {
		if r == '\n' || r == '\r' || r < ' ' || r == '\u202e' {
			t.Fatalf("Sanitize left %q in %q", r, got)
		}
	}
	if got := Sanitize("abcdef", 3); got != "abc..." {
		t.Fatalf("Sanitize cut = %q", got)
	}
}

func TestChainFallsBackWhenASurfaceFails(t *testing.T) {
	f := newFake()
	broken := newFake()
	broken.fail = errors.New("no display")
	s := Chain(broken, f)
	go func() {
		f.nextPrompt(t)
		f.answer(t, Answer{Allow: true})
	}()
	a, err := s.Ask(t.Context(), Prompt{Title: "x"})
	if err != nil || !a.Allow || a.Surface != "fake" {
		t.Fatalf("answer = %+v, %v", a, err)
	}
}

func withAuto(r Request) Request {
	r.Approval.Auto = "test"
	return r
}

func hasDecision(recs []AuditRecord, decision, reasonPart string) bool {
	for _, r := range recs {
		if r.Decision == decision && strings.Contains(r.Reason, reasonPart) {
			return true
		}
	}
	return false
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
