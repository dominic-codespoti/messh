package approval

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	zenityTitle  = "messh approval"
	zenityAlways = "Always allow..."
	zenityDeny   = "Deny"
)

// Zenity shows approval prompts with the zenity command (GTK dialogs), which
// works on X11 and Wayland compositors alike without linking GTK. Choosing
// "Always allow..." opens a second dialog to pick the scope.
type Zenity struct {
	// Bin is the zenity executable; empty looks it up on PATH.
	Bin string

	// Dismissable makes Esc and the close button return ErrDismissed instead
	// of denying (an extra Deny button then carries the denial): the dialog
	// is the "more options" view of a notification, and closing it should
	// leave the request pending.
	Dismissable bool

	// skipDisplayCheck lets tests run a stand-in zenity without a display.
	skipDisplayCheck bool
}

func (z *Zenity) Name() string { return "zenity" }

func (z *Zenity) Available() bool {
	if !z.skipDisplayCheck && os.Getenv("WAYLAND_DISPLAY") == "" && os.Getenv("DISPLAY") == "" {
		return false
	}
	_, err := z.path()
	return err == nil
}

func (z *Zenity) path() (string, error) {
	if z.Bin != "" {
		return z.Bin, nil
	}
	return exec.LookPath("zenity")
}

func (z *Zenity) Ask(ctx context.Context, p Prompt) (Answer, error) {
	bin, err := z.path()
	if err != nil {
		return Answer{}, err
	}
	floatOnHyprland(ctx)
	for {
		res, err := runZenity(ctx, bin, questionArgs(p, z.Dismissable))
		if err != nil {
			return Answer{}, err
		}
		switch {
		case res.code == 0:
			return Answer{Allow: true}, nil
		case res.code == 1 && strings.TrimSpace(res.stdout) == zenityAlways:
		case res.code == 1 && z.Dismissable && strings.TrimSpace(res.stdout) != zenityDeny:
			return Answer{}, ErrDismissed // Esc, or the window was closed
		case res.code == 1:
			return Answer{}, nil // Deny, or Esc and the window closed (not Dismissable)
		default:
			return Answer{}, fmt.Errorf("zenity exited with status %d: %s", res.code, strings.TrimSpace(res.stderr))
		}
		if len(p.Scopes) <= 1 {
			return Answer{Allow: true, Always: true}, nil
		}
		res, err = runZenity(ctx, bin, scopeArgs(p))
		if err != nil {
			return Answer{}, err
		}
		switch res.code {
		case 0:
			n, err := strconv.Atoi(strings.TrimSpace(res.stdout))
			if err != nil || n < 0 || n >= len(p.Scopes) {
				return Answer{}, fmt.Errorf("zenity returned an unknown scope %q", strings.TrimSpace(res.stdout))
			}
			return Answer{Allow: true, Always: true, Scope: n}, nil
		case 1: // Back: show the first dialog again
		default:
			return Answer{}, fmt.Errorf("zenity exited with status %d: %s", res.code, strings.TrimSpace(res.stderr))
		}
	}
}

type zenityResult struct {
	code           int
	stdout, stderr string
}

// runZenity runs one dialog. Cancelling ctx kills zenity, which closes the dialog.
func runZenity(ctx context.Context, bin string, args []string) (zenityResult, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return zenityResult{}, ctx.Err()
	}
	res := zenityResult{stdout: out.String(), stderr: errb.String()}
	var ee *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &ee) && ee.ExitCode() >= 0:
		res.code = ee.ExitCode()
	default:
		return zenityResult{}, err
	}
	// GTK reports a missing display as a plain exit 1, which would read as "Deny".
	if res.code != 0 && strings.Contains(res.stderr, "cannot open display") {
		return zenityResult{}, errors.New("zenity cannot open the display")
	}
	return res, nil
}

var markupEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func escapeMarkup(s string) string { return markupEscaper.Replace(s) }

func questionArgs(p Prompt, dismissable bool) []string {
	var text strings.Builder
	fmt.Fprintf(&text, "<b>%s</b>\n\n%s", escapeMarkup(Sanitize(p.Title, 200)), escapeMarkup(Body(p)))
	if note := WaitingNote(p); note != "" {
		fmt.Fprintf(&text, "\n\n(%s)", note)
	}
	args := []string{
		"--question",
		"--title=" + zenityTitle,
		"--width=520",
		"--text=" + text.String(),
		"--ok-label=Allow once",
		"--cancel-label=Deny",
		"--extra-button=" + zenityAlways,
		"--default-cancel", // a stray Enter must not approve
	}
	if dismissable {
		// Cancel (and Esc, and closing the window) now means "decide later".
		args[5], args[6] = "--cancel-label=Close", "--extra-button="+zenityDeny
		args = append(args, "--extra-button="+zenityAlways)
	}
	return args
}

func scopeArgs(p Prompt) []string {
	args := []string{
		"--list", "--radiolist",
		"--title=" + zenityTitle,
		"--width=560", "--height=320",
		"--text=" + escapeMarkup("Always allow for "+FormatCaller(p.Caller)+" only. Choose how much:"),
		"--column=Pick", "--column=#", "--column=Allow",
		"--hide-column=2", "--print-column=2",
		"--ok-label=Always allow",
		"--cancel-label=Back",
		"--", // scope labels may start with "-"
	}
	for i, s := range p.Scopes {
		pick := "FALSE"
		if i == 0 {
			pick = "TRUE"
		}
		args = append(args, pick, strconv.Itoa(i), FormatScope(s))
	}
	return args
}

var hyprOnce sync.Once

// floatOnHyprland asks Hyprland to float, centre and pin the approval window
// (tiling compositors would otherwise tile it into the layout). Rule syntax
// changed between Hyprland releases, so both forms are tried. This is best
// effort: failures are ignored and waiting is capped so a prompt never hangs on it.
func floatOnHyprland(ctx context.Context) {
	if os.Getenv("HYPRLAND_INSTANCE_SIGNATURE") == "" {
		return
	}
	hyprOnce.Do(func() {
		hyprctl, err := exec.LookPath("hyprctl")
		if err != nil {
			return
		}
		done := make(chan struct{})
		go func() {
			defer close(done)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			match := "title:^(" + zenityTitle + ")$"
			for _, rule := range []string{"float", "center", "pin"} {
				old := exec.CommandContext(ctx, hyprctl, "keyword", "windowrulev2", rule+","+match)
				if out, err := old.Output(); err == nil && strings.TrimSpace(string(out)) == "ok" {
					continue
				}
				exec.CommandContext(ctx, hyprctl, "keyword", "windowrule", rule+" on, match:"+match).Run()
			}
		}()
		select {
		case <-done:
		case <-time.After(1500 * time.Millisecond):
		case <-ctx.Done():
		}
	})
}
