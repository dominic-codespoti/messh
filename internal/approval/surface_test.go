package approval

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"messh/internal/provider"
)

// TestMain doubles as a stand-in zenity: the Zenity surface is pointed at the
// test binary with MESSH_FAKE_ZENITY set, so the real exec path (arguments,
// exit codes, stdout parsing, killing on cancel) runs without a display.
func TestMain(m *testing.M) {
	// notify-send, busctl and gdbus stand-ins are told apart by their arguments
	// (a notify-send child also inherits MESSH_FAKE_ZENITY and vice versa).
	if code, ok := fakeNotifyTool(os.Args[1:]); ok {
		os.Exit(code)
	}
	if mode := os.Getenv("MESSH_FAKE_ZENITY"); mode != "" {
		os.Exit(fakeZenity(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

func fakeZenity(mode string, args []string) int {
	has := func(a string) bool {
		for _, x := range args {
			if x == a {
				return true
			}
		}
		return false
	}
	question := has("--question")
	switch {
	case mode == "hang":
		time.Sleep(time.Minute)
	case mode == "nodisplay":
		fmt.Fprintln(os.Stderr, "Gtk-WARNING **: cannot open display: ")
		return 1
	case question && !has("--default-cancel"):
		fmt.Fprintln(os.Stderr, "fake zenity: the question dialog must default to Deny")
		return 2
	case !question && !has("--"):
		fmt.Fprintln(os.Stderr, "fake zenity: scope rows must follow --")
		return 2
	}
	switch {
	case mode == "once" && question:
		return 0
	case mode == "deny" && question:
		return 1
	case strings.HasPrefix(mode, "always:") && question:
		fmt.Println("Always allow...")
		return 1
	case strings.HasPrefix(mode, "always:"):
		fmt.Println(strings.TrimPrefix(mode, "always:"))
		return 0
	case mode == "back-then-deny" && question:
		if _, err := os.Stat(os.Getenv("MESSH_FAKE_ZENITY_FLAG")); err == nil {
			return 1
		}
		fmt.Println("Always allow...")
		return 1
	case mode == "back-then-deny":
		os.WriteFile(os.Getenv("MESSH_FAKE_ZENITY_FLAG"), nil, 0o600)
		return 1 // Back
	}
	return 3
}

func zenityPrompt() Prompt {
	return Prompt{
		ID: "id1", Title: "Run <b>a</b> job", Caller: provider.Caller{DeviceName: "raspi", Agent: "omp"},
		Details: []provider.Detail{{Label: "Command", Value: "echo '--x' & <tag>"}},
		Scopes:  []provider.Scope{{Key: "exact:1", Label: "exactly this request"}, {Key: "k", Label: "-leading dash"}, {Key: "b", Label: "any", Broad: true}},
	}
}

func TestZenityAnswers(t *testing.T) {
	z := &Zenity{Bin: os.Args[0], skipDisplayCheck: true}
	for _, tc := range []struct {
		mode string
		want Answer
	}{
		{"once", Answer{Allow: true}},
		{"deny", Answer{}},
		{"always:0", Answer{Allow: true, Always: true, Scope: 0}},
		{"always:2", Answer{Allow: true, Always: true, Scope: 2}},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			t.Setenv("MESSH_FAKE_ZENITY", tc.mode)
			got, err := z.Ask(t.Context(), zenityPrompt())
			if err != nil || got != tc.want {
				t.Fatalf("Ask = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestZenityBackReturnsToTheFirstDialog(t *testing.T) {
	t.Setenv("MESSH_FAKE_ZENITY", "back-then-deny")
	t.Setenv("MESSH_FAKE_ZENITY_FLAG", t.TempDir()+"/flag")
	got, err := (&Zenity{Bin: os.Args[0], skipDisplayCheck: true}).Ask(t.Context(), zenityPrompt())
	if err != nil || got.Allow {
		t.Fatalf("Ask = %+v, %v; want Deny after Back", got, err)
	}
}

func TestZenityMissingDisplayIsAnErrorNotADenial(t *testing.T) {
	t.Setenv("MESSH_FAKE_ZENITY", "nodisplay")
	if _, err := (&Zenity{Bin: os.Args[0], skipDisplayCheck: true}).Ask(t.Context(), zenityPrompt()); err == nil {
		t.Fatal("a dialog that could not be shown was reported as an answer")
	}
}

func TestZenityCancelKillsTheDialog(t *testing.T) {
	t.Setenv("MESSH_FAKE_ZENITY", "hang")
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := (&Zenity{Bin: os.Args[0], skipDisplayCheck: true}).Ask(ctx, zenityPrompt())
	if err == nil || time.Since(start) > 10*time.Second {
		t.Fatalf("Ask = %v after %s; want a prompt cancellation error", err, time.Since(start))
	}
}

func TestZenityTextEscapesMarkup(t *testing.T) {
	var text string
	for _, a := range questionArgs(zenityPrompt(), false) {
		if v, ok := strings.CutPrefix(a, "--text="); ok {
			text = v
		}
	}
	if strings.Contains(text, "<tag>") || strings.Contains(text, "<b>a</b>") || !strings.Contains(text, "&lt;tag&gt;") || !strings.Contains(text, "&amp;") {
		t.Fatalf("text = %q: untrusted markup was not escaped", text)
	}
}

func TestZenityUnavailableWithoutDisplay(t *testing.T) {
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("DISPLAY", "")
	if (&Zenity{Bin: os.Args[0]}).Available() {
		t.Fatal("zenity reported available with no display")
	}
}

// syncBuf is a goroutine-safe buffer for terminal output.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestTerminalAnswers(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close()
	out := &syncBuf{}
	term := &Terminal{In: r, Out: out}
	p := zenityPrompt()
	p.Waiting = 2

	ask := func(input string) (Answer, error) {
		type res struct {
			a   Answer
			err error
		}
		ch := make(chan res, 1)
		go func() { a, err := term.Ask(t.Context(), p); ch <- res{a, err} }()
		// Type only after the prompt is drawn, as a person would.
		waitUntil(t, "prompt text", func() bool { return strings.Contains(out.String(), "> ") })
		time.Sleep(20 * time.Millisecond)
		fmt.Fprint(w, input)
		r := get(t, ch)
		return r.a, r.err
	}

	if a, err := ask("o\n"); err != nil || a != (Answer{Allow: true}) {
		t.Fatalf("once = %+v, %v", a, err)
	}
	if a, err := ask("what\nd\n"); err != nil || a.Allow {
		t.Fatalf("deny after junk = %+v, %v", a, err)
	}
	if a, err := ask("a\n7\nb\na\n2\n"); err != nil || a != (Answer{Allow: true, Always: true, Scope: 2}) {
		t.Fatalf("always = %+v, %v", a, err)
	}
	text := out.String()
	for _, want := range []string{"messh approval id1", "raspi", "Command", "2 more waiting", "WARNING, broad"} {
		if !strings.Contains(text, want) {
			t.Errorf("terminal output lacks %q:\n%s", want, text)
		}
	}
}

func TestTerminalIgnoresInputTypedBeforeThePrompt(t *testing.T) {
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	term := &Terminal{In: r, Out: &syncBuf{}}
	// Start the reader and leave a stale "yes" waiting from before the prompt.
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	term.once.Do(term.startReader)
	fmt.Fprint(w, "o\n")
	time.Sleep(50 * time.Millisecond)
	_, err := term.Ask(ctx, Prompt{Title: "x", Scopes: []provider.Scope{{Key: "e"}}})
	if err == nil {
		t.Fatal("a line typed before the prompt answered it")
	}
}

func TestTerminalAskReturnsWhenCancelled(t *testing.T) {
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	term := &Terminal{In: r, Out: &syncBuf{}}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := term.Ask(ctx, Prompt{Title: "x", Scopes: []provider.Scope{{Key: "e"}}}); done <- err }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	if err := get(t, done); err == nil {
		t.Fatal("Ask returned no error after cancel")
	}
}
