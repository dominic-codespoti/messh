package approval

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"messh/internal/provider"
	"messh/internal/state"
)

// Defaults for Options.
const (
	DefaultTimeout    = 10 * time.Minute
	DefaultMaxPending = 5
)

// Decision outcomes, as recorded in the audit log.
const (
	OutcomeAllowOnce   = "allow-once"
	OutcomeAllowAlways = "allow-always" // allowed, and a rule was saved
	OutcomeAllowRule   = "allow-rule"   // a saved rule matched
	OutcomeAllowAuto   = "allow-auto"   // the provider's configuration pre-authorised it
	OutcomeDeny        = "deny"
	OutcomeExpired     = "expired"
	OutcomeCancelled   = "cancelled"
)

// ErrNotPending is returned by Resolve for an ID that is not (or no longer) pending.
var ErrNotPending = errors.New("no such pending approval")

// ErrNoRule is returned by RemoveRule for an unknown rule ID.
var ErrNoRule = errors.New("no such rule")

// ErrClosed is returned for requests made after Close.
var ErrClosed = errors.New("approval engine closed")

// Request is one gated call awaiting a decision.
type Request struct {
	Caller   provider.Caller
	Tool     string
	Class    provider.Class
	Approval provider.Approval
	// Test marks a synthetic request (messh approvals test): it is always
	// prompted, and "always allow" on it saves no rule.
	Test bool
}

// Decision is the outcome for one request.
type Decision struct {
	ID      string // request ID; also the ticket ID and the audit record ID
	Allowed bool
	Outcome string
	Reason  string // why it was not allowed; empty when allowed (for a test request: why no rule was saved)
	RuleID  string // the rule that matched or was just saved
	Auto    string // the provider's Auto reason, for allow-auto
	Surface string // which surface answered, when a person did
	Scope   int    // for allow-always: the index of the chosen scope
	// ScopeLabel is the chosen scope's label, for allow-always.
	ScopeLabel string
}

// DeniedError reports a request that was not allowed. Tickets return it from Wait.
type DeniedError struct{ Decision Decision }

func (e *DeniedError) Error() string { return "denied: " + e.Decision.Reason }

// Options configures an Engine.
type Options struct {
	Paths              state.Paths
	Surface            Surface       // nil or unavailable: requests wait for Resolve (headless)
	Log                *slog.Logger  // nil: slog.Default
	Timeout            time.Duration // unanswered requests are denied after this; default DefaultTimeout
	MaxPendingPerAgent int           // pending requests per calling device+agent; default DefaultMaxPending
}

// Engine matches requests against saved rules and asks the person at the
// host when none apply. Prompts are shown one at a time, except on surfaces
// that can show several (Parallel).
type Engine struct {
	paths      state.Paths
	surface    Surface
	log        *slog.Logger
	timeout    time.Duration
	maxPending int
	audit      *auditLog

	mu       sync.Mutex
	rules    []Rule
	order    []*entry // pending requests, oldest first
	started  bool
	closed   bool
	wake     chan struct{}
	stop     chan struct{}
	workDone chan struct{}
	port     int // the node's loopback API port, for prompts that call back over HTTP
}

// entry is one request from enqueue until it is decided.
type entry struct {
	id       string
	req      Request
	scopes   []provider.Scope // [0] is the exact-request scope
	created  time.Time
	shown    bool
	finished bool
	askCtx   context.Context
	cancel   context.CancelFunc
	timer    *time.Timer
	dec      Decision
	done     chan struct{} // closed once dec is set
	nonce    string        // one-time secret for Activate
	used     bool          // a decision button was accepted for this request
}

// New loads saved rules and returns an engine. A corrupt rules file is an
// error rather than silently starting with no rules. The prompt loop starts
// with the first request that needs one, so an unused engine holds no goroutine.
func New(opts Options) (*Engine, error) {
	e := &Engine{
		paths:      opts.Paths,
		surface:    opts.Surface,
		log:        opts.Log,
		timeout:    opts.Timeout,
		maxPending: opts.MaxPendingPerAgent,
		audit:      &auditLog{path: opts.Paths.AuditFile(), max: auditMaxBytes},
		wake:       make(chan struct{}, 1),
		stop:       make(chan struct{}),
		workDone:   make(chan struct{}),
	}
	if e.log == nil {
		e.log = slog.Default()
	}
	if e.timeout <= 0 {
		e.timeout = DefaultTimeout
	}
	if e.maxPending <= 0 {
		e.maxPending = DefaultMaxPending
	}
	if e.surface == nil || !e.surface.Available() {
		e.surface = Headless{}
	}
	if ls, ok := e.surface.(LogSetter); ok {
		ls.SetLogger(e.log)
	}
	rules, err := loadRules(opts.Paths.RulesFile())
	if err != nil {
		return nil, err
	}
	e.rules = rules
	return e, nil
}

// SurfaceName reports the surface prompts are shown on.
func (e *Engine) SurfaceName() string { return e.surface.Name() }

// Decide blocks until req is decided. The error is non-nil only when the
// request could not be queued (cap, invalid, closed) or ctx ended first; a
// denial is Decision{Allowed: false} with a nil error.
func (e *Engine) Decide(ctx context.Context, req Request) (Decision, error) {
	en, err := e.enqueue(req)
	if err != nil {
		return Decision{}, err
	}
	select {
	case <-en.done:
		return en.dec, nil
	case <-ctx.Done():
		e.withdraw(en, "request cancelled by the caller")
		<-en.done
		if en.dec.Allowed {
			return en.dec, nil // decided in the same instant; the caller's ctx will fail the call itself
		}
		return en.dec, ctx.Err()
	}
}

// Submit queues req without blocking and returns its ticket. A request that
// a rule or Auto already allows returns an approved ticket; one that cannot
// be queued returns a ticket that is already denied. Tickets outlive the
// context of the call that created them.
func (e *Engine) Submit(req Request) provider.Ticket {
	en, err := e.enqueue(req)
	if en == nil { // rejected before it was recorded
		en = &entry{id: newID(), done: make(chan struct{}), finished: true,
			dec: Decision{Outcome: OutcomeDeny, Reason: err.Error()}}
		en.dec.ID = en.id
		close(en.done)
	}
	return &ticket{e: e, en: en}
}

// Pending lists requests waiting for an answer, oldest first.
func (e *Engine) Pending() []Pending {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]Pending, 0, len(e.order))
	for _, en := range e.order {
		out = append(out, en.view(e.timeout))
	}
	return out
}

// Resolve answers a pending request as if the person had used the prompt;
// the shown dialog, if any, is closed. It is how headless devices approve.
func (e *Engine) Resolve(id string, a Answer) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, en := range e.order {
		if en.id == id {
			if a.Surface == "" {
				a.Surface = "control"
			}
			return e.applyLocked(en, a)
		}
	}
	return ErrNotPending
}

// Rules lists saved rules, oldest first.
func (e *Engine) Rules() []Rule {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Rule{}, e.rules...)
}

// RemoveRule deletes a saved rule.
func (e *Engine) RemoveRule(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for i, r := range e.rules {
		if r.ID == id {
			next := append(append([]Rule(nil), e.rules[:i]...), e.rules[i+1:]...)
			if err := saveRules(e.paths.RulesFile(), next); err != nil {
				return err
			}
			e.rules = next
			return nil
		}
	}
	return ErrNoRule
}

// Audit returns the newest n audit records, oldest first.
func (e *Engine) Audit(n int) ([]AuditRecord, error) { return e.audit.tail(n) }

// Complete appends the record of how the call allowed by d went.
func (e *Engine) Complete(d Decision, req Request, ok bool, errMsg string, took time.Duration) {
	rec := AuditRecord{
		Time: time.Now(), Kind: KindCompletion, ID: d.ID,
		DeviceID: req.Caller.DeviceID, Device: req.Caller.DeviceName, Agent: req.Caller.Agent,
		Tool: req.Tool, Class: string(req.Class), Title: req.Approval.Title, Exact: req.Approval.Exact,
		Decision: d.Outcome, RuleID: d.RuleID, Auto: d.Auto,
		OK: &ok, Error: errMsg, DurationMS: took.Milliseconds(),
	}
	if err := e.audit.append(rec); err != nil {
		e.log.Error("write audit log", "error", err)
	}
}

// Close denies everything still pending and stops the prompt loop.
func (e *Engine) Close() {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.closed = true
	for _, en := range append([]*entry(nil), e.order...) {
		e.finishLocked(en, Decision{Outcome: OutcomeCancelled, Reason: "node shutting down"})
	}
	close(e.stop)
	started := e.started
	e.mu.Unlock()
	if started {
		<-e.workDone
	}
}

// enqueue decides what it can immediately (Auto, rules) and otherwise
// queues the request for a prompt.
func (e *Engine) enqueue(req Request) (*entry, error) {
	switch {
	case req.Class == "" || req.Class == provider.ClassInfo:
		return nil, fmt.Errorf("class %q does not need approval", req.Class)
	case req.Caller.DeviceID == "":
		return nil, errors.New("caller device is unknown")
	case req.Approval.Exact == "":
		return nil, errors.New("approval has no exact request hash")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, ErrClosed
	}
	en := &entry{id: e.newIDLocked(), req: req, created: time.Now(), done: make(chan struct{}), nonce: newNonce()}
	if req.Approval.Auto != "" {
		e.finishLocked(en, Decision{Allowed: true, Outcome: OutcomeAllowAuto, Auto: req.Approval.Auto})
		return en, nil
	}
	for _, r := range e.rules {
		if !req.Test && r.matches(req) { // a test request is always prompted
			e.finishLocked(en, Decision{Allowed: true, Outcome: OutcomeAllowRule, RuleID: r.ID})
			return en, nil
		}
	}
	same := 0
	for _, o := range e.order {
		if o.req.Caller.DeviceID == req.Caller.DeviceID && o.req.Caller.Agent == req.Caller.Agent {
			same++
		}
	}
	if same >= e.maxPending {
		err := fmt.Errorf("too many approval requests waiting from %s (agent %q); wait for them to be answered", callerName(req.Caller), req.Caller.Agent)
		e.finishLocked(en, Decision{Outcome: OutcomeDeny, Reason: err.Error()})
		return en, err
	}

	en.scopes = promptScopes(req.Approval)
	en.askCtx, en.cancel = context.WithCancel(context.Background())
	en.timer = time.AfterFunc(e.timeout, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.finishLocked(en, Decision{Outcome: OutcomeExpired, Reason: "timed out waiting for approval"})
	})
	e.order = append(e.order, en)
	if !e.started {
		e.started = true
		go e.run()
	}
	select {
	case e.wake <- struct{}{}:
	default:
	}
	return en, nil
}

// withdraw cancels a still-pending request.
func (e *Engine) withdraw(en *entry, reason string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.finishLocked(en, Decision{Outcome: OutcomeCancelled, Reason: reason})
}

// applyLocked turns an answer into a decision for a pending entry.
func (e *Engine) applyLocked(en *entry, a Answer) error {
	if en.finished {
		return ErrNotPending
	}
	if a.Allow && a.Always && (a.Scope < 0 || a.Scope >= len(en.scopes)) {
		return fmt.Errorf("scope %d is out of range (0-%d)", a.Scope, len(en.scopes)-1)
	}
	d := Decision{Surface: a.Surface}
	switch {
	case !a.Allow:
		d.Outcome, d.Reason = OutcomeDeny, "denied by the user"
	case a.Always && en.req.Test:
		d.Allowed, d.Outcome, d.Scope, d.ScopeLabel = true, OutcomeAllowAlways, a.Scope, en.scopes[a.Scope].Label
		d.Reason = "test request: no rule was saved"
	case a.Always:
		s, c := en.scopes[a.Scope], en.req.Caller
		rule, err := e.addRuleLocked(Rule{
			DeviceID: c.DeviceID, Device: c.DeviceName, Agent: c.Agent, Class: en.req.Class,
			Key: s.Key, Label: s.Label, Broad: s.Broad,
		})
		if err != nil {
			// Still honour this one request; the person can retry "always".
			e.log.Error("save approval rule", "error", err)
			d.Allowed, d.Outcome = true, OutcomeAllowOnce
			break
		}
		d.Allowed, d.Outcome, d.RuleID, d.Scope, d.ScopeLabel = true, OutcomeAllowAlways, rule.ID, a.Scope, s.Label
	default:
		d.Allowed, d.Outcome = true, OutcomeAllowOnce
	}
	e.finishLocked(en, d)
	return nil
}

// addRuleLocked saves r, reusing an identical existing rule.
func (e *Engine) addRuleLocked(r Rule) (Rule, error) {
	for _, old := range e.rules {
		if old.DeviceID == r.DeviceID && old.Agent == r.Agent && old.Class == r.Class && old.Key == r.Key {
			return old, nil
		}
	}
	r.ID = e.newIDLocked()
	r.Created = time.Now().UTC()
	next := append(append([]Rule(nil), e.rules...), r)
	if err := saveRules(e.paths.RulesFile(), next); err != nil {
		return Rule{}, err
	}
	e.rules = next
	return r, nil
}

// finishLocked records the decision, removes the entry from the queue, closes
// its prompt and wakes waiters. It is a no-op once the entry is decided.
func (e *Engine) finishLocked(en *entry, d Decision) {
	if en.finished {
		return
	}
	en.finished = true
	d.ID = en.id
	en.dec = d
	if en.timer != nil {
		en.timer.Stop()
	}
	if en.cancel != nil {
		en.cancel()
	}
	for i, o := range e.order {
		if o == en {
			e.order = append(e.order[:i], e.order[i+1:]...)
			break
		}
	}
	rec := AuditRecord{
		Time: time.Now(), Kind: KindDecision, ID: d.ID,
		DeviceID: en.req.Caller.DeviceID, Device: en.req.Caller.DeviceName, Agent: en.req.Caller.Agent,
		Tool: en.req.Tool, Class: string(en.req.Class), Title: en.req.Approval.Title, Exact: en.req.Approval.Exact,
		Decision: d.Outcome, RuleID: d.RuleID, Auto: d.Auto, Reason: d.Reason, Surface: d.Surface,
	}
	// The record is written before waiters wake so a caller never acts on a
	// decision that is not yet in the log.
	if err := e.audit.append(rec); err != nil {
		e.log.Error("write audit log", "error", err)
	}
	close(en.done)
}

// run shows queued requests: one at a time, or all at once on a Parallel surface.
func (e *Engine) run() {
	defer close(e.workDone)
	var wg sync.WaitGroup
	defer wg.Wait() // prompts end when their request is decided, which Close does for every one
	for {
		en, waiting := e.next()
		if en == nil {
			return
		}
		if isParallel(e.surface) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				e.ask(en, 0)
			}()
			continue
		}
		e.ask(en, waiting)
	}
}

// ask shows one request on the surface and applies the answer.
func (e *Engine) ask(en *entry, waiting int) {
	e.mu.Lock()
	port := e.port
	e.mu.Unlock()
	ans, err := e.surface.Ask(en.askCtx, Prompt{
		ID: en.id, Title: en.req.Approval.Title, Caller: en.req.Caller, Tool: en.req.Tool,
		Details: en.req.Approval.Details, Scopes: en.scopes, Waiting: waiting,
		Expires: en.created.Add(e.timeout), Port: port, Nonce: en.nonce,
	})
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case en.finished:
	case err != nil && en.askCtx.Err() == nil:
		e.log.Error("approval prompt failed", "surface", e.surface.Name(), "error", err)
		e.finishLocked(en, Decision{Outcome: OutcomeDeny, Reason: "the approval prompt failed: " + err.Error(), Surface: e.surface.Name()})
	case err != nil:
		// Withdrawn while the prompt was open.
	default:
		if ans.Surface == "" {
			ans.Surface = e.surface.Name()
		}
		if err := e.applyLocked(en, ans); err != nil {
			e.finishLocked(en, Decision{Outcome: OutcomeDeny, Reason: "invalid answer: " + err.Error(), Surface: ans.Surface})
		}
	}
}

// SetPort tells prompts which loopback port the node's API listens on.
func (e *Engine) SetPort(port int) {
	e.mu.Lock()
	e.port = port
	e.mu.Unlock()
}

// Activation actions for Engine.Activate.
const (
	ActionOnce    = "once"
	ActionAlways  = "always" // saves the narrowest scope (the exact request)
	ActionDeny    = "deny"
	ActionOptions = "options"
)

// Activation errors.
var (
	ErrBadNonce  = errors.New("invalid or expired approval token")
	ErrNonceUsed = errors.New("this approval token was already used")
	ErrBadAction = errors.New("unknown approval action")
)

// Activate answers a pending request on behalf of a notification button.
// The only credential is the request's own one-time nonce, which the engine
// hands to the surface in Prompt.Nonce: it works for that request only, and
// ends when the request is decided. Deny, once and always each work once;
// options opens the surface's fuller view and may be repeated. It can do
// nothing else: in particular it never sees rules or the audit log.
func (e *Engine) Activate(id, nonce, action string) error {
	switch action {
	case ActionOnce, ActionAlways, ActionDeny, ActionOptions:
	default:
		return ErrBadAction
	}
	e.mu.Lock()
	var en *entry
	for _, o := range e.order {
		if o.id == id {
			en = o
			break
		}
	}
	if en == nil {
		e.mu.Unlock()
		return ErrNotPending
	}
	if subtle.ConstantTimeCompare([]byte(nonce), []byte(en.nonce)) != 1 {
		e.mu.Unlock()
		return ErrBadNonce
	}
	if action == ActionOptions {
		surface := e.surface
		e.mu.Unlock()
		d, ok := surface.(Detailer)
		if !ok {
			return ErrNotShown
		}
		return d.ShowDetails(id)
	}
	defer e.mu.Unlock()
	if en.used {
		return ErrNonceUsed
	}
	en.used = true
	return e.applyLocked(en, Answer{
		Allow: action != ActionDeny, Always: action == ActionAlways, Scope: 0, Surface: SurfaceToast,
	})
}

// next blocks until a request has not been shown yet and returns it with the
// number of other requests still waiting. It returns nil when the engine closes.
func (e *Engine) next() (*entry, int) {
	for {
		e.mu.Lock()
		for _, en := range e.order {
			if !en.shown {
				en.shown = true
				waiting := len(e.order) - 1
				e.mu.Unlock()
				return en, waiting
			}
		}
		e.mu.Unlock()
		select {
		case <-e.wake:
		case <-e.stop:
			return nil, 0
		}
	}
}

func (e *Engine) newIDLocked() string {
	for {
		id := newID()
		taken := false
		for _, r := range e.rules {
			taken = taken || r.ID == id
		}
		for _, o := range e.order {
			taken = taken || o.id == id
		}
		if !taken {
			return id
		}
	}
}

func newID() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return hex.EncodeToString(b[:])
}

// newNonce returns a 128-bit one-time secret, hex encoded.
func newNonce() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}

// Test shows a harmless synthetic request on the real surface and waits for
// the answer (messh approvals test). Nothing runs and "always allow" saves
// no rule; the decision is still audited, as tool "approval_test". The
// request is withdrawn if ctx ends first.
func (e *Engine) Test(ctx context.Context, host string) (Decision, error) {
	return e.Decide(ctx, Request{
		Caller: provider.Caller{DeviceID: "local-test", DeviceName: host, Agent: "messh approvals test"},
		Tool:   "approval_test",
		Class:  provider.ClassExec,
		Test:   true,
		Approval: provider.Approval{
			Title: "TEST: allow this harmless request?",
			Details: []provider.Detail{
				{Label: "Test", Value: "Started by `messh approvals test`. Nothing will run and no rule will be saved."},
			},
			Exact:  "test:" + newID(),
			Scopes: []provider.Scope{{Key: "test:any", Label: "every test request (an example of a broad choice)", Broad: true}},
		},
	})
}

// promptScopes returns the scopes offered for "Always allow": the exact
// request first, then the provider's, skipping empty and duplicate keys.
func promptScopes(a provider.Approval) []provider.Scope {
	out := []provider.Scope{{Key: ExactKey(a.Exact), Label: "exactly this request"}}
	seen := map[string]bool{out[0].Key: true}
	for _, s := range a.Scopes {
		if s.Key != "" && !seen[s.Key] {
			seen[s.Key] = true
			out = append(out, s)
		}
	}
	return out
}

// ExactKey is the rule key that matches one precise request.
func ExactKey(exact string) string { return "exact:" + exact }

type ticket struct {
	e  *Engine
	en *entry
}

func (t *ticket) ID() string { return t.en.id }

func (t *ticket) Wait(ctx context.Context) (bool, error) {
	select {
	case <-t.en.done:
		if t.en.dec.Allowed {
			return true, nil
		}
		return false, &DeniedError{Decision: t.en.dec}
	case <-ctx.Done():
		return false, ctx.Err()
	}
}

func (t *ticket) Cancel() { t.e.withdraw(t.en, "request cancelled") }

func callerName(c provider.Caller) string {
	if c.DeviceName != "" {
		return c.DeviceName
	}
	return c.DeviceID
}
