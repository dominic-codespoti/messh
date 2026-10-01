package approval

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

// notifier is a fake notification surface: it shows every prompt at once
// (Parallel), and records "more options" requests.
type notifier struct {
	*fakeSurface
	mu      sync.Mutex
	details []string
}

func newNotifier() *notifier { return &notifier{fakeSurface: newFake()} }

func (n *notifier) Parallel() bool { return true }
func (n *notifier) ShowDetails(id string) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.details = append(n.details, id)
	return nil
}

func (n *notifier) detailsFor() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]string(nil), n.details...)
}

func TestActivateAnswersOnlyWithTheRequestsOwnNonce(t *testing.T) {
	n := newNotifier()
	e := newEngine(t, n)
	r1 := decide(e, t.Context(), mkReq("raspi", "omp", "one"))
	r2 := decide(e, t.Context(), mkReq("raspi", "omp", "two"))
	p1, p2 := n.nextPrompt(t), n.nextPrompt(t)
	if strings.HasSuffix(p1.Details[0].Value, "two") { // parallel prompts arrive in any order
		p1, p2 = p2, p1
	}
	if p1.Nonce == "" || p1.Nonce == p2.Nonce || len(p1.Nonce) != 32 {
		t.Fatalf("nonces = %q, %q: want distinct 128-bit values", p1.Nonce, p2.Nonce)
	}
	// Nothing short of the exact id + nonce pair may answer.
	for name, tc := range map[string]struct{ id, nonce string }{
		"wrong nonce":         {p1.ID, strings.Repeat("0", 32)},
		"empty nonce":         {p1.ID, ""},
		"other request nonce": {p1.ID, p2.Nonce},
		"prefix of nonce":     {p1.ID, p1.Nonce[:31]},
		"nonce as id":         {p1.Nonce, p1.Nonce},
	} {
		if err := e.Activate(tc.id, tc.nonce, ActionOnce); err == nil {
			t.Errorf("%s: Activate succeeded", name)
		}
	}
	if err := e.Activate("deadbeef", p1.Nonce, ActionOnce); !errors.Is(err, ErrNotPending) {
		t.Errorf("unknown id: %v", err)
	}
	if err := e.Activate(p1.ID, p1.Nonce, "allow"); !errors.Is(err, ErrBadAction) {
		t.Errorf("bad action: %v", err)
	}
	if len(e.Pending()) != 2 {
		t.Fatalf("a rejected activation resolved something: %d pending", len(e.Pending()))
	}

	if err := e.Activate(p1.ID, p1.Nonce, ActionOnce); err != nil {
		t.Fatal(err)
	}
	if d := get(t, r1).D; !d.Allowed || d.Outcome != OutcomeAllowOnce || d.Surface != SurfaceToast {
		t.Fatalf("decision = %+v", d)
	}
	// The nonce dies with the request.
	if err := e.Activate(p1.ID, p1.Nonce, ActionDeny); !errors.Is(err, ErrNotPending) {
		t.Fatalf("reusing a resolved request's nonce: %v", err)
	}
	if err := e.Activate(p2.ID, p2.Nonce, ActionDeny); err != nil {
		t.Fatal(err)
	}
	if d := get(t, r2).D; d.Allowed || d.Outcome != OutcomeDeny {
		t.Fatalf("deny = %+v", d)
	}
}

func TestActivateAlwaysSavesTheNarrowestScope(t *testing.T) {
	n := newNotifier()
	e := newEngine(t, n)
	r := decide(e, t.Context(), mkReq("raspi", "omp", "one"))
	p := n.nextPrompt(t)
	if err := e.Activate(p.ID, p.Nonce, ActionAlways); err != nil {
		t.Fatal(err)
	}
	if d := get(t, r).D; !d.Allowed || d.Outcome != OutcomeAllowAlways || d.Scope != 0 {
		t.Fatalf("decision = %+v", d)
	}
	rules := e.Rules()
	if len(rules) != 1 || rules[0].Key != ExactKey("one") || rules[0].Agent != "omp" {
		t.Fatalf("rules = %+v, want one rule for exactly this request", rules)
	}
	// The exact request is now allowed without a prompt; a different one is still asked.
	if d, err := e.Decide(t.Context(), mkReq("raspi", "omp", "one")); err != nil || d.Outcome != OutcomeAllowRule {
		t.Fatalf("repeat = %+v, %v", d, err)
	}
	_ = decide(e, t.Context(), mkReq("raspi", "omp", "two"))
	n.nextPrompt(t)
}

func TestActivateOptionsOpensDetailsAndMayRepeat(t *testing.T) {
	n := newNotifier()
	e := newEngine(t, n)
	r := decide(e, t.Context(), mkReq("raspi", "omp", "one"))
	p := n.nextPrompt(t)
	for range 2 {
		if err := e.Activate(p.ID, p.Nonce, ActionOptions); err != nil {
			t.Fatal(err)
		}
	}
	if got := n.detailsFor(); len(got) != 2 || got[0] != p.ID {
		t.Fatalf("details requests = %v", got)
	}
	if err := e.Activate(p.ID, strings.Repeat("1", 32), ActionOptions); err == nil || len(n.detailsFor()) != 2 {
		t.Fatal("options opened a dialog for a wrong nonce")
	}
	if len(e.Pending()) != 1 {
		t.Fatal("opening the options view resolved the request")
	}
	// The dialog's own answer arrives through Ask like any other.
	n.answer(t, Answer{Allow: true, Always: true, Scope: 1, Surface: "windows"})
	if d := get(t, r).D; d.Outcome != OutcomeAllowAlways || d.Scope != 1 || d.Surface != "windows" {
		t.Fatalf("decision = %+v", d)
	}
	if rules := e.Rules(); len(rules) != 1 || rules[0].Key != argvScope.Key {
		t.Fatalf("rules = %+v, want the scope chosen in the dialog", rules)
	}
}

func TestActivateOptionsWithoutADetailSurface(t *testing.T) {
	f := newFake()
	e := newEngine(t, f)
	_ = decide(e, t.Context(), mkReq("raspi", "omp", "one"))
	p := f.nextPrompt(t)
	if err := e.Activate(p.ID, p.Nonce, ActionOptions); !errors.Is(err, ErrNotShown) {
		t.Fatalf("options on a surface without details = %v", err)
	}
}

func TestActivateAfterExpiry(t *testing.T) {
	n := newNotifier()
	e := newEngine(t, n, func(o *Options) { o.Timeout = 50 * time.Millisecond })
	r := decide(e, t.Context(), mkReq("raspi", "omp", "one"))
	p := n.nextPrompt(t)
	if d := get(t, r).D; d.Outcome != OutcomeExpired {
		t.Fatalf("decision = %+v", d)
	}
	if err := e.Activate(p.ID, p.Nonce, ActionOnce); !errors.Is(err, ErrNotPending) {
		t.Fatalf("activating an expired request: %v", err)
	}
	if p.Expires.IsZero() {
		t.Fatal("prompt has no expiry for the toast to carry")
	}
}

func TestPromptCarriesPortAndExpiry(t *testing.T) {
	n := newNotifier()
	e := newEngine(t, n)
	e.SetPort(7520)
	before := time.Now()
	_ = decide(e, t.Context(), mkReq("raspi", "omp", "one"))
	p := n.nextPrompt(t)
	if p.Port != 7520 || p.Expires.Before(before.Add(DefaultTimeout-time.Second)) || p.Expires.After(time.Now().Add(DefaultTimeout)) {
		t.Fatalf("port %d, expires %s", p.Port, p.Expires)
	}
}

func TestParallelSurfaceShowsEveryRequestAtOnce(t *testing.T) {
	n := newNotifier()
	e := newEngine(t, n)
	var results []<-chan struct {
		D   Decision
		Err error
	}
	for _, exact := range []string{"a", "b", "c"} {
		results = append(results, decide(e, t.Context(), mkReq("raspi", "omp", exact)))
	}
	seen := map[string]bool{}
	for range 3 {
		p := n.nextPrompt(t) // all three arrive with nobody having answered
		if p.Waiting != 0 {
			t.Errorf("Waiting = %d on a parallel surface", p.Waiting)
		}
		seen[p.ID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("prompts = %v", seen)
	}
	for _, p := range e.Pending() {
		if err := e.Resolve(p.ID, Answer{Allow: true}); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range results {
		get(t, r)
	}
}

func TestCloseEndsEveryParallelPrompt(t *testing.T) {
	n := newNotifier()
	e := newEngine(t, n)
	for _, exact := range []string{"a", "b"} {
		_ = decide(e, t.Context(), mkReq("raspi", "omp", exact))
		n.nextPrompt(t)
	}
	done := make(chan struct{})
	go func() { e.Close(); close(done) }()
	get(t, done)
	for range 2 {
		select {
		case <-n.closed:
		case <-time.After(5 * time.Second):
			t.Fatal("a prompt stayed open after Close")
		}
	}
}

func TestSyntheticTestRequestSavesNothing(t *testing.T) {
	f := newFake()
	e := newEngine(t, f)
	type res struct {
		d   Decision
		err error
	}
	ch := make(chan res, 1)
	go func() { d, err := e.Test(t.Context(), "desktop"); ch <- res{d, err} }()
	p := f.nextPrompt(t)
	if !strings.HasPrefix(p.Title, "TEST") || len(p.Scopes) != 2 {
		t.Fatalf("prompt = %+v", p)
	}
	f.answer(t, Answer{Allow: true, Always: true, Scope: 1})
	r := get(t, ch)
	if r.err != nil || r.d.Outcome != OutcomeAllowAlways || r.d.Scope != 1 || r.d.RuleID != "" || !strings.Contains(r.d.Reason, "no rule") {
		t.Fatalf("test result = %+v, %v", r.d, r.err)
	}
	if len(e.Rules()) != 0 {
		t.Fatalf("a test request saved rules: %+v", e.Rules())
	}
	// Even with a rule in place, a test is still prompted.
	go func() { d, err := e.Test(t.Context(), "desktop"); ch <- res{d, err} }()
	f.nextPrompt(t)
	f.answer(t, Answer{})
	if r := get(t, ch); r.d.Outcome != OutcomeDeny {
		t.Fatalf("second test = %+v", r.d)
	}
	recs, err := e.Audit(10)
	if err != nil || len(recs) != 2 || recs[0].Tool != "approval_test" {
		t.Fatalf("audit = %+v, %v", recs, err)
	}
}

// failing is a surface whose prompt cannot be shown.
type failing struct{}

func (failing) Name() string    { return "broken" }
func (failing) Available() bool { return true }
func (failing) Ask(context.Context, Prompt) (Answer, error) {
	return Answer{}, errors.New("no notification service")
}

func TestChainFallsThroughAndSaysWhy(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	next := newFake()
	c := Chain(failing{}, next)
	c.(LogSetter).SetLogger(log)
	e := newEngine(t, c, func(o *Options) { o.Log = log })
	r := decide(e, t.Context(), mkReq("raspi", "omp", "one"))
	next.nextPrompt(t)
	next.answer(t, Answer{Allow: true})
	if d := get(t, r).D; !d.Allowed || d.Surface != "fake" {
		t.Fatalf("decision = %+v", d)
	}
	if got := logs.String(); !strings.Contains(got, "broken") || !strings.Contains(got, "no notification service") {
		t.Fatalf("the fallback was not explained in the log:\n%s", got)
	}
}

func TestChainRoutesDetailsAndParallelism(t *testing.T) {
	n := newNotifier()
	c := Chain(n, newFake())
	if !c.(Parallel).Parallel() {
		t.Fatal("a chain led by a parallel surface is not parallel")
	}
	if Chain(newFake(), n).(Parallel).Parallel() {
		t.Fatal("a chain led by a serial surface must stay serial")
	}
	if err := c.(Detailer).ShowDetails("x1"); err != nil || len(n.detailsFor()) != 1 {
		t.Fatalf("ShowDetails = %v", err)
	}
	if err := Chain(newFake()).(Detailer).ShowDetails("x1"); !errors.Is(err, ErrNotShown) {
		t.Fatalf("ShowDetails without a detail surface = %v", err)
	}
}

func TestChainShowsOnePromptAtATimeOnASerialFallback(t *testing.T) {
	// A parallel head that fails hands every request to a dialog; the dialog
	// must not appear twice at once.
	var mu sync.Mutex
	active, peak := 0, 0
	dialog := &funcSurface{name: "dialog", ask: func(ctx context.Context, p Prompt) (Answer, error) {
		mu.Lock()
		active++
		peak = max(peak, active)
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		active--
		mu.Unlock()
		return Answer{Allow: true}, nil
	}}
	head := &funcSurface{name: "head", parallel: true, ask: func(context.Context, Prompt) (Answer, error) {
		return Answer{}, errors.New("cannot show")
	}}
	c := Chain(head, dialog)
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Ask(t.Context(), Prompt{ID: string(rune('a' + i))}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if peak != 1 {
		t.Fatalf("%d dialogs were open at once", peak)
	}
}

type funcSurface struct {
	name     string
	parallel bool
	ask      func(context.Context, Prompt) (Answer, error)
}

func (f *funcSurface) Name() string    { return f.name }
func (f *funcSurface) Available() bool { return true }
func (f *funcSurface) Parallel() bool  { return f.parallel }
func (f *funcSurface) Ask(ctx context.Context, p Prompt) (Answer, error) {
	return f.ask(ctx, p)
}
