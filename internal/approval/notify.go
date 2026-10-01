package approval

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"messh/internal/provider"
)

// Action names handed to notify-send; the one the person picked comes back on stdout.
const (
	notifyOnce    = "once"
	notifyAlways  = "always"
	notifyDeny    = "deny"
	notifyDefault = "default" // a click on the notification itself
	notifyOptions = "options"
)

const (
	notifyProbeTTL     = 30 * time.Second // how long a capability probe result is trusted
	notifyProbeTimeout = 5 * time.Second
	notifyBusName      = "org.freedesktop.Notifications"
	notifyBusPath      = "/org/freedesktop/Notifications"
)

// Notify shows approval prompts as desktop notifications through notify-send
// (libnotify >= 0.8, which can attach actions and wait for the choice) and
// whichever notification daemon is running (mako, dunst, swaync, KDE, GNOME).
//
// What notify-send really does (libnotify tools/notify-send.c, read at 0.8.3;
// the release notes cover the other versions):
//   - With --print-id --wait stdout is "<id>\n", followed by "<action name>\n"
//     when an action was invoked; a plain close prints no action. The action
//     line is only flushed when notify-send exits, and before 0.8.4 so is the
//     id, which is then lost if notify-send has to be killed.
//   - SIGINT makes it close the notification itself, flush and exit 0, so
//     cancelling interrupts it first and kills it only if that fails.
//   - --expire-time makes it stop waiting (exit 0, no action) after that long.
//   - Body text goes through g_strcompress (backslash escapes), so backslashes
//     are doubled; the summary is passed through untouched.
//   - A show failure or bad option exits 1; a daemon without the "actions"
//     capability makes it exit 1 too instead of waiting, so Available checks
//     the capability first.
//
// A click on the notification body (the "default" action) or the "More
// options" button opens Dialog for the same request; closing that dialog
// without a choice returns to the notification. A notification that is
// dismissed, or expires, leaves the request pending.
type Notify struct {
	// Bin is the notify-send executable; empty looks it up on PATH.
	Bin string
	// Busctl and Gdbus are the D-Bus clients used to ask the daemon for its
	// capabilities and to close notifications; empty looks them up on PATH
	// (busctl is preferred).
	Busctl, Gdbus string
	// Dialog is the "more options" view; nil means a Dismissable Zenity.
	Dialog Surface

	log atomic.Pointer[slog.Logger]

	probeMu sync.Mutex
	probed  time.Time
	probeOK bool
	now     func() time.Time // tests only
}

func (n *Notify) Name() string   { return SurfaceNotify }
func (n *Notify) Parallel() bool { return true }

func (n *Notify) SetLogger(l *slog.Logger) { n.log.Store(l) }

func (n *Notify) logger() *slog.Logger {
	if l := n.log.Load(); l != nil {
		return l
	}
	return slog.Default()
}

func (n *Notify) clock() time.Time {
	if n.now != nil {
		return n.now()
	}
	return time.Now()
}

func (n *Notify) dialog() Surface {
	if n.Dialog != nil {
		return n.Dialog
	}
	return &Zenity{Dismissable: true}
}

func (n *Notify) notifySend() (string, error) {
	if n.Bin != "" {
		return n.Bin, nil
	}
	return exec.LookPath("notify-send")
}

// desktopSession reports whether a desktop session (or at least a session
// bus) seems to exist. It is only a cheap pre-check; the capability probe
// decides.
func desktopSession() bool {
	for _, k := range []string{"WAYLAND_DISPLAY", "DISPLAY", "DBUS_SESSION_BUS_ADDRESS"} {
		if os.Getenv(k) != "" {
			return true
		}
	}
	if dir := os.Getenv("XDG_RUNTIME_DIR"); dir != "" {
		if _, err := os.Stat(filepath.Join(dir, "bus")); err == nil {
			return true
		}
	}
	return false
}

// Available reports whether notify-send can attach actions and the running
// daemon supports them. The external commands behind that answer run at most
// once per notifyProbeTTL (the daemon may start later).
func (n *Notify) Available() bool {
	if !desktopSession() {
		return false
	}
	n.probeMu.Lock()
	defer n.probeMu.Unlock()
	if !n.probed.IsZero() && n.clock().Sub(n.probed) < notifyProbeTTL {
		return n.probeOK
	}
	ok, why := n.probe()
	if !ok {
		n.logger().Debug("desktop notifications unavailable", "reason", why)
	}
	n.probeOK, n.probed = ok, n.clock()
	return ok
}

func (n *Notify) probe() (bool, string) {
	bin, err := n.notifySend()
	if err != nil {
		return false, "notify-send not found"
	}
	ctx, cancel := context.WithTimeout(context.Background(), notifyProbeTimeout)
	defer cancel()
	help, _ := exec.CommandContext(ctx, bin, "--help").CombinedOutput() // GLib prints --help on stdout
	if !notifySendSupportsActions(string(help)) {
		return false, "notify-send is older than libnotify 0.8 (no --action)"
	}
	out, err := n.bus(ctx, "GetCapabilities")
	if err != nil {
		return false, "cannot query the notification daemon: " + err.Error()
	}
	caps, err := parseCapabilities(out)
	if err != nil {
		return false, err.Error()
	}
	for _, c := range caps {
		if c == "actions" {
			return true, ""
		}
	}
	return false, "the notification daemon does not support actions"
}

// notifySendSupportsActions reads `notify-send --help`: libnotify before 0.8
// has no --action (and then no way to wait for a choice).
func notifySendSupportsActions(help string) bool {
	return strings.Contains(help, "--action") && strings.Contains(help, "--print-id")
}

var (
	busctlCapsRe = regexp.MustCompile(`^as\s+(\d+)\b`)
	quotedStrRe  = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"|'((?:[^'\\]|\\.)*)'`)
)

// parseCapabilities understands the output of GetCapabilities from busctl
// (`as 4 "actions" "body"`) and from gdbus (`(['actions', 'body'],)`).
func parseCapabilities(out string) ([]string, error) {
	out = strings.TrimSpace(out)
	var caps []string
	for _, m := range quotedStrRe.FindAllStringSubmatch(out, -1) {
		s := m[1]
		if m[2] != "" {
			s = m[2]
		}
		caps = append(caps, s)
	}
	switch {
	case busctlCapsRe.MatchString(out):
		want, _ := strconv.Atoi(busctlCapsRe.FindStringSubmatch(out)[1])
		if want != len(caps) {
			return nil, fmt.Errorf("unexpected busctl capabilities output %q", out)
		}
	case strings.HasPrefix(out, "(") && strings.HasSuffix(out, ")"):
	default:
		return nil, fmt.Errorf("unexpected capabilities output %q", out)
	}
	return caps, nil
}

// bus calls a method of the notification daemon with busctl, or gdbus when
// busctl is missing. params are the method's arguments (a uint32 here).
func (n *Notify) bus(ctx context.Context, method string, params ...uint32) (string, error) {
	bin, argv, err := n.busCommand(method, params...)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, bin, argv...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(errb.String()); msg != "" {
			return "", fmt.Errorf("%s: %w: %s", filepath.Base(bin), err, msg)
		}
		return "", fmt.Errorf("%s: %w", filepath.Base(bin), err)
	}
	return out.String(), nil
}

func (n *Notify) busCommand(method string, params ...uint32) (string, []string, error) {
	busctl := n.Busctl
	if busctl == "" {
		busctl, _ = exec.LookPath("busctl")
	}
	if busctl != "" {
		argv := []string{"--user", "call", notifyBusName, notifyBusPath, notifyBusName, method}
		if len(params) > 0 {
			argv = append(argv, strings.Repeat("u", len(params)))
			for _, v := range params {
				argv = append(argv, strconv.FormatUint(uint64(v), 10))
			}
		}
		return busctl, argv, nil
	}
	gdbus := n.Gdbus
	if gdbus == "" {
		gdbus, _ = exec.LookPath("gdbus")
	}
	if gdbus == "" {
		return "", nil, errors.New("neither busctl nor gdbus found")
	}
	argv := []string{"call", "--session", "--dest", notifyBusName, "--object-path", notifyBusPath, "--method", notifyBusName + "." + method}
	for _, v := range params {
		argv = append(argv, "uint32 "+strconv.FormatUint(uint64(v), 10))
	}
	return gdbus, argv, nil
}

// closeNotification asks the daemon to close notification id. It runs after
// the request's context is cancelled, so it gets its own short deadline.
func (n *Notify) closeNotification(ctx context.Context, id uint32) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), notifyProbeTimeout)
	defer cancel()
	if _, err := n.bus(ctx, "CloseNotification", id); err != nil {
		// The notification may be gone already (answered, dismissed, expired).
		n.logger().Debug("could not close the notification", "id", id, "error", err)
	}
}

func (n *Notify) Ask(ctx context.Context, p Prompt) (Answer, error) {
	bin, err := n.notifySend()
	if err != nil {
		return Answer{}, err
	}
	dialog := n.dialog()
	for {
		res, err := n.show(ctx, bin, p, dialog.Available())
		if err != nil {
			return Answer{}, err
		}
		switch res.action {
		case notifyOnce:
			return Answer{Allow: true, Surface: SurfaceNotify}, nil
		case notifyAlways:
			return Answer{Allow: true, Always: true, Scope: 0, Surface: SurfaceNotify}, nil
		case notifyDeny:
			return Answer{Surface: SurfaceNotify}, nil
		case notifyDefault, notifyOptions:
			a, err := dialog.Ask(ctx, p)
			if errors.Is(err, ErrDismissed) {
				continue // closed without a choice: back to the notification
			}
			if err != nil {
				return Answer{}, err
			}
			a.Surface = dialog.Name()
			return a, nil
		case "":
			// Dismissed or expired: the request stays pending for another
			// surface (the CLI, the control API) until it is resolved.
			<-ctx.Done()
			if res.id != 0 {
				n.closeNotification(ctx, res.id)
			}
			return Answer{}, ctx.Err()
		default:
			return Answer{}, fmt.Errorf("notify-send reported an unknown action %q", res.action)
		}
	}
}

type notifyResult struct {
	id     uint32 // 0 when unknown
	action string // "" when the notification closed without one
}

// show displays the notification and waits until an action is invoked, it
// closes, or ctx ends (then it is closed and ctx.Err() returned).
func (n *Notify) show(ctx context.Context, bin string, p Prompt, withDialog bool) (notifyResult, error) {
	cmd := exec.CommandContext(ctx, bin, notifyArgs(p, n.clock(), withDialog)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	// SIGINT makes notify-send close its notification and flush its output;
	// without it (Windows, or a process that cannot be signalled) kill it.
	cmd.Cancel = func() error {
		if err := cmd.Process.Signal(os.Interrupt); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = 3 * time.Second // then kill it
	runErr := cmd.Run()

	id, action, parseErr := parseNotifySend(out.String())
	if ctx.Err() != nil {
		if id != 0 {
			n.closeNotification(ctx, id)
		} else {
			n.logger().Warn("notification id unknown; the notification was not closed explicitly")
		}
		return notifyResult{}, ctx.Err()
	}
	var ee *exec.ExitError
	switch {
	case runErr == nil:
	case errors.As(runErr, &ee) && ee.ExitCode() >= 0:
		msg := strings.TrimSpace(errb.String())
		return notifyResult{}, fmt.Errorf("notify-send exited with status %d: %s", ee.ExitCode(), msg)
	default:
		return notifyResult{}, runErr
	}
	if parseErr != nil {
		return notifyResult{}, parseErr
	}
	if id == 0 && action == "" {
		// A real notify-send always prints the id of what it showed.
		return notifyResult{}, errors.New("notify-send printed no notification id")
	}
	return notifyResult{id: id, action: action}, nil
}

// parseNotifySend reads notify-send's stdout: the notification id from
// --print-id, then the name of the invoked action, if any.
func parseNotifySend(out string) (id uint32, action string, err error) {
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > 0 {
		if v, perr := strconv.ParseUint(lines[0], 10, 32); perr == nil {
			id, lines = uint32(v), lines[1:]
		}
	}
	switch len(lines) {
	case 0:
		return id, "", nil
	case 1:
		return id, lines[0], nil
	}
	return id, "", fmt.Errorf("unexpected notify-send output %q", out)
}

// notifyArgs builds notify-send's arguments. Every argument is its own argv
// entry (no shell); the user-controlled summary and body follow `--` so they
// can never be read as options.
func notifyArgs(p Prompt, now time.Time, withDialog bool) []string {
	args := []string{
		"--app-name=messh",
		"--urgency=critical",
		"--expire-time=" + strconv.Itoa(notifyExpireMillis(p.Expires, now)),
		"--wait", "--print-id",
	}
	if tag := notifyTag(p.ID); tag != "" {
		args = append(args, "--hint=string:x-canonical-private-synchronous:messh-"+tag)
	}
	if withDialog {
		args = append(args, "--action=default=Details")
	}
	args = append(args, "--action=once=Allow once", "--action=always=Always allow", "--action=deny=Deny")
	if withDialog {
		args = append(args, "--action=options=More options")
	}
	title := escapeMarkup(Sanitize(p.Title, 100))
	if strings.TrimSpace(title) == "" {
		title = zenityTitle
	}
	return append(args, "--", title, notifyBody(p))
}

// notifyExpireMillis is how long the notification may stay up: until the
// request times out (at least a second), or forever when that is unknown.
func notifyExpireMillis(expires, now time.Time) int {
	if expires.IsZero() {
		return 0
	}
	ms := expires.Sub(now).Milliseconds()
	switch {
	case ms < 1000:
		return 1000
	case ms > math.MaxInt32:
		return math.MaxInt32
	}
	return int(ms)
}

// notifyTag makes the id safe inside the synchronous-hint value.
func notifyTag(id string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			return r
		}
		return '_'
	}, id)
}

func notifyWho(c provider.Caller) string {
	name := c.DeviceName
	if name == "" {
		name = c.DeviceID
	}
	name = Sanitize(name, 64)
	if strings.TrimSpace(name) == "" {
		name = "unknown device"
	}
	if c.Agent != "" {
		name += " / " + Sanitize(c.Agent, 64)
	}
	return name
}

// notifyBody is the notification text: who is asking, the first two detail
// lines and a short hint. Notification servers render a subset of HTML, so
// markup characters are escaped, and notify-send runs the body through
// g_strcompress, so backslashes are doubled.
func notifyBody(p Prompt) string {
	lines := []string{notifyWho(p.Caller)}
	for _, d := range p.Details {
		if len(lines) > 2 {
			break
		}
		lines = append(lines, Sanitize(d.Label, 40)+": "+Sanitize(d.Value, 160))
	}
	lines = append(lines, "Always allow = exactly this request")
	for i, l := range lines {
		lines[i] = escapeMarkup(strings.ReplaceAll(l, `\`, `\\`))
	}
	return strings.Join(lines, "\n")
}
