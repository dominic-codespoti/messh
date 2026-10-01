package approval

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"messh/internal/provider"
)

// Surface shows an approval prompt to the person at this device.
type Surface interface {
	// Name identifies the surface in the audit log ("windows", "zenity", "terminal", "headless").
	Name() string
	// Available reports whether the surface can show prompts right now.
	Available() bool
	// Ask shows p and blocks for the answer. It must return promptly, with
	// ctx's error, once ctx is cancelled and close whatever it showed.
	Ask(ctx context.Context, p Prompt) (Answer, error)
}

// Prompt is what a Surface shows.
type Prompt struct {
	ID      string
	Title   string
	Caller  provider.Caller
	Tool    string
	Details []provider.Detail
	// Scopes are the "Always allow" choices. Scopes[0] is always the exact
	// request; the rest come from the provider, narrowest first.
	Scopes  []provider.Scope
	Waiting int // other requests queued behind this one (always 0 for Parallel surfaces)
	// Expires is when the request times out unanswered; zero if unknown.
	Expires time.Time
	// Port is the node's loopback API port and Nonce a one-time secret for
	// Engine.Activate, for surfaces that can only call back over HTTP (the
	// Windows toast's protocol buttons). Port is 0 until the node listens.
	Port  int
	Nonce string
}

// Answer is the person's choice.
type Answer struct {
	Allow  bool
	Always bool // with Allow: save a rule for Scopes[Scope]
	Scope  int
	// Surface names who answered when it is not the engine's own surface
	// (set by Chain and by the control API).
	Surface string
}

// Pending is a request waiting for an answer, as listed through the control API.
type Pending struct {
	ID       string    `json:"id"`
	DeviceID string    `json:"device_id"`
	Device   string    `json:"device"`
	Agent    string    `json:"agent"`
	Tool     string    `json:"tool"`
	Class    string    `json:"class"`
	Title    string    `json:"title"`
	Details  []Line    `json:"details"`
	Scopes   []Choice  `json:"scopes"` // index is the number to pass as the scope
	Created  time.Time `json:"created"`
	Expires  time.Time `json:"expires"`
	Shown    bool      `json:"shown"` // a prompt is open for it on a surface
}

// Line is one labelled prompt line.
type Line struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// Choice is one "Always allow" scope.
type Choice struct {
	Label string `json:"label"`
	Broad bool   `json:"broad,omitempty"`
}

func (en *entry) view(timeout time.Duration) Pending {
	p := Pending{
		ID: en.id, DeviceID: en.req.Caller.DeviceID, Device: en.req.Caller.DeviceName, Agent: en.req.Caller.Agent,
		Tool: en.req.Tool, Class: string(en.req.Class), Title: en.req.Approval.Title,
		Created: en.created, Expires: en.created.Add(timeout), Shown: en.shown,
	}
	for _, d := range en.req.Approval.Details {
		p.Details = append(p.Details, Line{Label: d.Label, Value: d.Value})
	}
	for _, s := range en.scopes {
		p.Scopes = append(p.Scopes, Choice{Label: s.Label, Broad: s.Broad})
	}
	return p
}

// ErrDismissed is returned by a Dismissable dialog that was closed without a choice.
var ErrDismissed = errors.New("dialog dismissed without an answer")

// Headless never answers: requests stay pending until Engine.Resolve (the
// control API or `messh approvals`) decides them, or they time out.
type Headless struct{}

func (Headless) Name() string    { return "headless" }
func (Headless) Available() bool { return true }
func (Headless) Parallel() bool  { return true } // it shows nothing, so there is nothing to serialise
func (Headless) Ask(ctx context.Context, _ Prompt) (Answer, error) {
	<-ctx.Done()
	return Answer{}, ctx.Err()
}

// Parallel is implemented by surfaces that can show several prompts at once
// (notifications). The engine then asks for every pending request
// immediately instead of one after another.
type Parallel interface{ Parallel() bool }

// Detailer is implemented by surfaces that can open a fuller view (a dialog
// with the scope choice) for a prompt they already show. The answer comes
// back through the original Ask.
type Detailer interface{ ShowDetails(id string) error }

// ErrNotShown is returned by ShowDetails for a request the surface is not showing.
var ErrNotShown = errors.New("the request is not shown on this surface")

// LogSetter is implemented by surfaces that want the engine's logger.
type LogSetter interface{ SetLogger(*slog.Logger) }

// Surface names in audit records for answers that arrive over HTTP.
const (
	SurfaceToast  = "toast"
	SurfaceNotify = "notify"
)

// Chain tries each available surface in turn until one shows the prompt. A
// surface that fails (for example zenity without a display) hands over to
// the next, and the reason is logged. Surfaces that cannot show several
// prompts at once are used one prompt at a time.
func Chain(surfaces ...Surface) Surface {
	c := &chain{log: slog.Default()}
	for _, s := range surfaces {
		c.members = append(c.members, &member{Surface: s})
	}
	return c
}

type member struct {
	Surface
	mu sync.Mutex // held while a serial surface shows a prompt
}

func isParallel(s Surface) bool {
	p, ok := s.(Parallel)
	return ok && p.Parallel()
}

type chain struct {
	members []*member
	log     *slog.Logger
}

func (c *chain) Name() string {
	names := make([]string, len(c.members))
	for i, m := range c.members {
		names[i] = m.Name()
	}
	return strings.Join(names, ">")
}

func (c *chain) Available() bool {
	for _, m := range c.members {
		if m.Available() {
			return true
		}
	}
	return false
}

// Parallel reports whether the surface that would show the next prompt can
// show several at once.
func (c *chain) Parallel() bool {
	for _, m := range c.members {
		if m.Available() {
			return isParallel(m.Surface)
		}
	}
	return false
}

func (c *chain) SetLogger(l *slog.Logger) {
	c.log = l
	for _, m := range c.members {
		if ls, ok := m.Surface.(LogSetter); ok {
			ls.SetLogger(l)
		}
	}
}

func (c *chain) ShowDetails(id string) error {
	for _, m := range c.members {
		if d, ok := m.Surface.(Detailer); ok && d.ShowDetails(id) == nil {
			return nil
		}
	}
	return ErrNotShown
}

func (c *chain) Ask(ctx context.Context, p Prompt) (Answer, error) {
	var errs []error
	for i, m := range c.members {
		if !m.Available() {
			continue
		}
		a, err := c.askMember(ctx, m, p)
		if err == nil {
			if a.Surface == "" {
				a.Surface = m.Name()
			}
			return a, nil
		}
		if ctx.Err() != nil {
			return Answer{}, ctx.Err()
		}
		errs = append(errs, fmt.Errorf("%s: %w", m.Name(), err))
		if i < len(c.members)-1 {
			c.log.Warn("approval surface failed; trying the next one", "surface", m.Name(), "error", err)
		}
	}
	if len(errs) == 0 {
		return Answer{}, errors.New("no approval surface available")
	}
	return Answer{}, errors.Join(errs...)
}

func (c *chain) askMember(ctx context.Context, m *member, p Prompt) (Answer, error) {
	if isParallel(m.Surface) {
		return m.Ask(ctx, p)
	}
	locked := make(chan struct{})
	go func() { m.mu.Lock(); close(locked) }()
	select {
	case <-locked:
		defer m.mu.Unlock()
		return m.Ask(ctx, p)
	case <-ctx.Done():
		go func() { <-locked; m.mu.Unlock() }()
		return Answer{}, ctx.Err()
	}
}

// FormatCaller renders who is asking, e.g. `raspi (agent "omp")`.
func FormatCaller(c provider.Caller) string {
	name := c.DeviceName
	if name == "" {
		name = c.DeviceID
	}
	name = Sanitize(name, 64)
	if c.Agent == "" {
		return name
	}
	return fmt.Sprintf("%s (agent %q)", name, Sanitize(c.Agent, 64))
}

// FormatScope renders an "Always allow" choice, marking broad ones.
func FormatScope(s provider.Scope) string {
	label := Sanitize(s.Label, 200)
	if s.Broad {
		return "WARNING, broad: " + label
	}
	return label
}

// Sanitize makes untrusted text safe to show on one line: control characters
// become spaces (newlines show as a visible mark), and the result is cut to
// max runes. Prompt text comes from other devices, so it must not be able to
// fake extra lines in a dialog.
func Sanitize(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= max {
			b.WriteString("...")
			break
		}
		switch {
		case r == '\n':
			b.WriteString(" \u23ce ")
		case r == utf8.RuneError || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029':
			b.WriteByte(' ')
		default:
			b.WriteRune(r)
		}
		n++
	}
	return b.String()
}

// Body renders the detail lines of a prompt as plain text, one per line.
func Body(p Prompt) string {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s", FormatCaller(p.Caller))
	for _, d := range p.Details {
		fmt.Fprintf(&b, "\n%s: %s", Sanitize(d.Label, 40), Sanitize(d.Value, 600))
	}
	return b.String()
}

// WaitingNote says how many other requests are queued, or "" for none.
func WaitingNote(p Prompt) string {
	if p.Waiting <= 0 {
		return ""
	}
	return fmt.Sprintf("%d more waiting", p.Waiting)
}
