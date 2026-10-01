package approval

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Terminal prompts on the node's own console: it prints to stderr and reads
// answers from stdin. It is for devices without a desktop (a Raspberry Pi
// reached over ssh or at a console).
type Terminal struct {
	In  *os.File  // default os.Stdin
	Out io.Writer // default os.Stderr

	once  sync.Once
	lines chan stamped
}

// stamped is an input line with the time it was read.
type stamped struct {
	text string
	at   time.Time
}

func (t *Terminal) Name() string { return "terminal" }

func (t *Terminal) Available() bool {
	in := t.In
	if in == nil {
		in = os.Stdin
	}
	return isTerminal(in)
}

func (t *Terminal) Ask(ctx context.Context, p Prompt) (Answer, error) {
	t.once.Do(t.startReader)
	out := t.Out
	if out == nil {
		out = os.Stderr
	}

	// Text typed before this prompt was drawn was not an answer to it.
	shownAt := time.Now()
	fmt.Fprintf(out, "\n=== messh approval %s ===\n%s\n%s\n", p.ID, Sanitize(p.Title, 200), Body(p))
	if note := WaitingNote(p); note != "" {
		fmt.Fprintf(out, "(%s)\n", note)
	}
	for {
		fmt.Fprint(out, "[o] allow once   [a] always allow...   [d] deny > ")
		line, err := t.readLine(ctx, shownAt)
		if err != nil {
			fmt.Fprintln(out)
			return Answer{}, err
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "o", "once", "y", "yes":
			return Answer{Allow: true}, nil
		case "d", "deny", "n", "no":
			return Answer{}, nil
		case "a", "always":
			scope, ok, err := t.askScope(ctx, out, p, shownAt)
			if err != nil {
				return Answer{}, err
			}
			if ok {
				return Answer{Allow: true, Always: true, Scope: scope}, nil
			}
		}
	}
}

// askScope lists the scopes; ok is false when the person goes back.
func (t *Terminal) askScope(ctx context.Context, out io.Writer, p Prompt, shownAt time.Time) (int, bool, error) {
	if len(p.Scopes) == 1 {
		return 0, true, nil
	}
	fmt.Fprintln(out, "Always allow, for this device and agent:")
	for i, s := range p.Scopes {
		fmt.Fprintf(out, "  %d  %s\n", i, FormatScope(s))
	}
	for {
		fmt.Fprintf(out, "scope 0-%d, or [b]ack > ", len(p.Scopes)-1)
		line, err := t.readLine(ctx, shownAt)
		if err != nil {
			return 0, false, err
		}
		line = strings.ToLower(strings.TrimSpace(line))
		if line == "b" || line == "back" {
			return 0, false, nil
		}
		if n, err := strconv.Atoi(line); err == nil && n >= 0 && n < len(p.Scopes) {
			return n, true, nil
		}
	}
}

// readLine returns the next line typed after since, or ctx's error.
func (t *Terminal) readLine(ctx context.Context, since time.Time) (string, error) {
	for {
		select {
		case l, ok := <-t.lines:
			if !ok {
				return "", io.EOF
			}
			if l.at.Before(since) {
				continue
			}
			return l.text, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// startReader reads stdin for the life of the process: a blocked read cannot
// be interrupted, so a single reader feeds whichever prompt is open.
func (t *Terminal) startReader() {
	in := t.In
	if in == nil {
		in = os.Stdin
	}
	t.lines = make(chan stamped)
	go func() {
		defer close(t.lines)
		r := bufio.NewReader(in)
		for {
			s, err := r.ReadString('\n')
			if s != "" {
				t.lines <- stamped{text: s, at: time.Now()}
			}
			if err != nil {
				return
			}
		}
	}()
}
