package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"messh/internal/provider"
)

var _ interface {
	Surface
	Parallel
	LogSetter
} = (*Notify)(nil)

// Real libnotify 0.8 and 0.7 `notify-send --help` texts (trimmed).
const notifyHelp08 = `Usage:
  notify-send [OPTION…] <SUMMARY> [BODY] - create a notification

Help Options:
  -?, --help                        Show help options

Application Options:
  -u, --urgency=LEVEL               Specifies the urgency level (low, normal, critical).
  -t, --expire-time=TIME            Specifies the timeout in milliseconds at which to expire the notification.
  -a, --app-name=APP_NAME           Specifies the app name for the icon
  -i, --icon=ICON                   Specifies an icon filename or stock icon to display.
  -c, --category=TYPE[,TYPE...]     Specifies the notification category.
  -e, --transient                   Create a transient notification
  -h, --hint=TYPE:NAME:VALUE        Specifies basic extra data to pass. Valid types are boolean, int, double, string, byte and variant.
  -p, --print-id                    Print the notification ID.
  -r, --replace-id=REPLACE_ID       The ID of the notification to replace.
  -w, --wait                        Wait for the notification to be closed before exiting.
  -A, --action=[NAME=]Text...       Specifies the actions to display to the user. Implies --wait to wait for user input. May be set multiple times. The name of the action is output to stdout. If NAME is not specified, the numerical index of the option is used (starting with 0).
  -v, --version                     Version of the package.
`

const notifyHelp07 = `Usage:
  notify-send [OPTION…] <SUMMARY> [BODY] - create a notification

Help Options:
  -?, --help                        Show help options

Application Options:
  -u, --urgency=LEVEL               Specifies the urgency level (low, normal, critical).
  -t, --expire-time=TIME            Specifies the timeout in milliseconds at which to expire the notification.
  -a, --app-name=APP_NAME           Specifies the app name for the icon
  -i, --icon=ICON                   Specifies an icon filename or stock icon to display.
  -c, --category=TYPE[,TYPE...]     Specifies the notification category.
  -h, --hint=TYPE:NAME:VALUE        Specifies basic extra data to pass. Valid types are int, double, string and byte.
  -v, --version                     Version of the package.
`

// fakeNotifyTool is the stand-in for notify-send, busctl and gdbus: the Notify
// surface is pointed at the test binary with MESSH_FAKE_NOTIFY / MESSH_FAKE_BUS
// set. The tools are told apart by their arguments because child processes
// also inherit the other stand-ins' variables.
func fakeNotifyTool(args []string) (int, bool) {
	has := func(a string) bool {
		for _, x := range args {
			if x == a {
				return true
			}
		}
		return false
	}
	switch {
	case has("--print-id") || has("--help"):
		if mode := os.Getenv("MESSH_FAKE_NOTIFY"); mode != "" {
			return fakeNotifySend(mode, args, has), true
		}
	case len(args) > 0 && (args[0] == "--user" || (args[0] == "call" && has("--session"))):
		if mode := os.Getenv("MESSH_FAKE_BUS"); mode != "" {
			return fakeBus(mode, args, has), true
		}
	}
	return 0, false
}

func fakeAppend(envKey string, args []string) {
	path := os.Getenv(envKey)
	if path == "" {
		return
	}
	line, _ := json.Marshal(args)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(append(line, '\n'))
}

// fakeNotifyID is 1000 plus the trailing digits of the request id in the
// synchronous hint, so every request gets its own notification id.
func fakeNotifyID(args []string) int {
	const prefix = "--hint=string:x-canonical-private-synchronous:messh-"
	for _, a := range args {
		if tag, ok := strings.CutPrefix(a, prefix); ok {
			end := len(tag)
			for end > 0 && tag[end-1] >= '0' && tag[end-1] <= '9' {
				end--
			}
			n, _ := strconv.Atoi(tag[end:])
			return 1000 + n
		}
	}
	return 1000
}

func fakeNotifySend(mode string, args []string, has func(string) bool) int {
	if has("--help") {
		if mode == "old" {
			fmt.Print(notifyHelp07)
		} else {
			fmt.Print(notifyHelp08)
		}
		fakeAppend("MESSH_FAKE_NOTIFY_HELP_LOG", args)
		return 0
	}
	id := fakeNotifyID(args)
	switch mode {
	case "fail":
		fmt.Fprintln(os.Stderr, "boom: cannot show the notification")
		return 1
	case "noid":
		return 0
	case "hang-noid":
		time.Sleep(time.Minute)
		return 0
	}
	// Unlike the real notify-send, which only flushes at exit, the id is
	// written at once; the log line follows it, so a test that has seen the log
	// knows the id is in the pipe.
	fmt.Println(id)
	fakeAppend("MESSH_FAKE_NOTIFY_LOG", args)
	switch mode {
	case "once", "always", "deny", "default", "options":
		fmt.Println(mode)
	case "unknown":
		fmt.Println("bogus")
	case "closed":
	case "hang":
		time.Sleep(time.Minute)
	case "options-then-once":
		flag := os.Getenv("MESSH_FAKE_NOTIFY_FLAG")
		if _, err := os.Stat(flag); err != nil {
			os.WriteFile(flag, nil, 0o600)
			fmt.Println("options")
		} else {
			fmt.Println("once")
		}
	case "wait-all": // answers only once every request of the test is showing
		dir := os.Getenv("MESSH_FAKE_NOTIFY_DIR")
		want, _ := strconv.Atoi(os.Getenv("MESSH_FAKE_NOTIFY_N"))
		os.WriteFile(filepath.Join(dir, strconv.Itoa(id)), nil, 0o600)
		deadline := time.Now().Add(20 * time.Second)
		for {
			if entries, _ := os.ReadDir(dir); len(entries) >= want {
				break
			}
			if time.Now().After(deadline) {
				return 3
			}
			time.Sleep(5 * time.Millisecond)
		}
		if id%2 == 0 {
			fmt.Println("once")
		} else {
			fmt.Println("deny")
		}
	default:
		return 3
	}
	return 0
}

func fakeBus(mode string, args []string, has func(string) bool) int {
	fakeAppend("MESSH_FAKE_BUS_LOG", args)
	if mode == "fail" {
		fmt.Fprintln(os.Stderr, "Failed to connect to bus: No such file or directory")
		return 1
	}
	gdbus := args[0] == "call"
	switch {
	case has("GetCapabilities") || has("org.freedesktop.Notifications.GetCapabilities"):
		caps, ok := strings.CutPrefix(mode, "caps:")
		if !ok {
			return 3
		}
		var list []string
		if caps != "" {
			list = strings.Split(caps, ",")
		}
		if gdbus {
			quoted := make([]string, len(list))
			for i, c := range list {
				quoted[i] = "'" + c + "'"
			}
			if len(list) == 0 {
				fmt.Println("(@as [],)")
			} else {
				fmt.Printf("([%s],)\n", strings.Join(quoted, ", "))
			}
		} else {
			quoted := make([]string, len(list))
			for i, c := range list {
				quoted[i] = strconv.Quote(c)
			}
			fmt.Println(strings.TrimSpace(fmt.Sprintf("as %d %s", len(list), strings.Join(quoted, " "))))
		}
		return 0
	case has("CloseNotification") || has("org.freedesktop.Notifications.CloseNotification"):
		if gdbus {
			fmt.Println("()")
		}
		return 0
	}
	return 3
}

func readLog(path string) [][]string {
	data, _ := os.ReadFile(path)
	var out [][]string
	for _, l := range strings.Split(string(data), "\n") {
		if l == "" {
			continue
		}
		var a []string
		if json.Unmarshal([]byte(l), &a) == nil {
			out = append(out, a)
		}
	}
	return out
}

// notifyEnv gives the test a desktop session, an empty PATH (so no real
// busctl, gdbus or notify-send is found) and log files for the stand-ins.
func notifyEnv(t *testing.T, mode string) (notifyLog, busLog string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("DISPLAY", ":0")
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("PATH", dir)
	t.Setenv("MESSH_FAKE_NOTIFY", mode)
	t.Setenv("MESSH_FAKE_BUS", "caps:actions,body")
	notifyLog, busLog = filepath.Join(dir, "notify.log"), filepath.Join(dir, "bus.log")
	t.Setenv("MESSH_FAKE_NOTIFY_LOG", notifyLog)
	t.Setenv("MESSH_FAKE_NOTIFY_HELP_LOG", filepath.Join(dir, "help.log"))
	t.Setenv("MESSH_FAKE_BUS_LOG", busLog)
	return
}

func fakeNotify() *Notify {
	return &Notify{
		Bin: os.Args[0], Busctl: os.Args[0],
		Dialog: &Zenity{Bin: os.Args[0], skipDisplayCheck: true, Dismissable: true},
	}
}

func TestParseCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		want      []string
		bad       bool
	}{
		{"busctl", "as 11 \"action-icons\" \"actions\" \"body\" \"body-hyperlinks\" \"body-markup\" \"icon-static\" \"persistence\" \"sound\" \"x-canonical-private-synchronous\" \"x-dunst-stack-tag\" \"x-kde-urls\"\n",
			[]string{"action-icons", "actions", "body", "body-hyperlinks", "body-markup", "icon-static", "persistence", "sound", "x-canonical-private-synchronous", "x-dunst-stack-tag", "x-kde-urls"}, false},
		{"busctl empty", "as 0\n", nil, false},
		{"gdbus", "(['actions', 'body', 'body-markup', 'icon-static'],)\n", []string{"actions", "body", "body-markup", "icon-static"}, false},
		{"gdbus empty", "(@as [],)\n", nil, false},
		{"gdbus double quotes", "(['actions', \"it's\"],)\n", []string{"actions", "it's"}, false},
		{"busctl count mismatch", "as 3 \"actions\" \"body\"\n", nil, true},
		{"error text", "Failed to connect to bus: No such file or directory\n", nil, true},
		{"empty", "", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCapabilities(tc.out)
			if (err != nil) != tc.bad || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseCapabilities = %q, %v; want %q (error %v)", got, err, tc.want, tc.bad)
			}
		})
	}
}

func TestNotifySendHelpNeedsActions(t *testing.T) {
	if !notifySendSupportsActions(notifyHelp08) {
		t.Error("libnotify 0.8 help was not recognised")
	}
	if notifySendSupportsActions(notifyHelp07) {
		t.Error("libnotify 0.7 help (no --action) was accepted")
	}
}

func TestNotifyAvailable(t *testing.T) {
	gdbusOnly := func() *Notify { return &Notify{Bin: os.Args[0], Gdbus: os.Args[0]} }
	for _, tc := range []struct {
		name   string
		notify string // MESSH_FAKE_NOTIFY
		bus    string // MESSH_FAKE_BUS
		n      func() *Notify
		env    map[string]string
		want   bool
	}{
		{"busctl with actions", "full", "caps:actions,body", fakeNotify, nil, true},
		{"gdbus with actions", "full", "caps:actions,body", gdbusOnly, nil, true},
		{"old libnotify", "old", "caps:actions,body", fakeNotify, nil, false},
		{"daemon without actions", "full", "caps:body,body-markup", fakeNotify, nil, false},
		{"daemon without any capability", "full", "caps:", fakeNotify, nil, false},
		{"no daemon", "full", "fail", fakeNotify, nil, false},
		{"no busctl and no gdbus", "full", "caps:actions", func() *Notify { return &Notify{Bin: os.Args[0]} }, nil, false},
		{"no notify-send", "full", "caps:actions", func() *Notify { return &Notify{Busctl: os.Args[0]} }, nil, false},
		{"no desktop session", "full", "caps:actions", fakeNotify, map[string]string{"DISPLAY": ""}, false},
		{"session bus only", "full", "caps:actions", fakeNotify, map[string]string{"DISPLAY": "", "DBUS_SESSION_BUS_ADDRESS": "unix:path=/run/user/1000/bus"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notifyEnv(t, tc.notify)
			t.Setenv("MESSH_FAKE_BUS", tc.bus)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			if got := tc.n().Available(); got != tc.want {
				t.Fatalf("Available = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNotifyAvailableProbesOncePerInterval(t *testing.T) {
	_, busLog := notifyEnv(t, "full")
	helpLog := os.Getenv("MESSH_FAKE_NOTIFY_HELP_LOG")
	now := time.Now()
	n := fakeNotify()
	n.now = func() time.Time { return now }
	for range 5 {
		if !n.Available() {
			t.Fatal("Available = false")
		}
	}
	if h, b := len(readLog(helpLog)), len(readLog(busLog)); h != 1 || b != 1 {
		t.Fatalf("%d help and %d bus calls for five Available calls; want one of each", h, b)
	}

	// A daemon that starts later is noticed after the interval, not before.
	t.Setenv("MESSH_FAKE_BUS", "fail")
	now = now.Add(notifyProbeTTL - time.Second)
	if !n.Available() || len(readLog(busLog)) != 1 {
		t.Fatal("probed again before the interval ended")
	}
	now = now.Add(2 * time.Second)
	if n.Available() {
		t.Fatal("Available = true after the daemon went away")
	}
	if len(readLog(helpLog)) != 2 || len(readLog(busLog)) != 2 {
		t.Fatal("did not probe again after the interval")
	}
	for range 3 {
		n.Available()
	}
	if len(readLog(busLog)) != 2 {
		t.Fatal("a negative result was not cached")
	}
}

func TestNotifyAnswers(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want Answer
	}{
		{"once", Answer{Allow: true, Surface: "notify"}},
		{"always", Answer{Allow: true, Always: true, Scope: 0, Surface: "notify"}},
		{"deny", Answer{Surface: "notify"}},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			notifyEnv(t, tc.mode)
			got, err := fakeNotify().Ask(t.Context(), zenityPrompt())
			if err != nil || got != tc.want {
				t.Fatalf("Ask = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestNotifyDetailsOpensTheDialog(t *testing.T) {
	for _, tc := range []struct {
		notify, zenity string
		want           Answer
	}{
		{"default", "once", Answer{Allow: true, Surface: "zenity"}},
		{"options", "once", Answer{Allow: true, Surface: "zenity"}},
		{"options", "always:2", Answer{Allow: true, Always: true, Scope: 2, Surface: "zenity"}},
	} {
		t.Run(tc.notify+"/"+tc.zenity, func(t *testing.T) {
			notifyEnv(t, tc.notify)
			t.Setenv("MESSH_FAKE_ZENITY", tc.zenity)
			got, err := fakeNotify().Ask(t.Context(), zenityPrompt())
			if err != nil || got != tc.want {
				t.Fatalf("Ask = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestNotifyDialogDismissedShowsTheNotificationAgain(t *testing.T) {
	notifyLog, _ := notifyEnv(t, "options-then-once")
	t.Setenv("MESSH_FAKE_NOTIFY_FLAG", filepath.Join(t.TempDir(), "flag"))
	t.Setenv("MESSH_FAKE_ZENITY", "deny") // plain exit 1 without "Deny": the window was closed
	got, err := fakeNotify().Ask(t.Context(), zenityPrompt())
	if err != nil || got != (Answer{Allow: true, Surface: "notify"}) {
		t.Fatalf("Ask = %+v, %v; want the second notification's answer", got, err)
	}
	if n := len(readLog(notifyLog)); n != 2 {
		t.Fatalf("notify-send ran %d times, want 2", n)
	}
}

func TestNotifyDialogErrorIsReturned(t *testing.T) {
	notifyEnv(t, "options")
	t.Setenv("MESSH_FAKE_ZENITY", "nodisplay")
	if _, err := fakeNotify().Ask(t.Context(), zenityPrompt()); err == nil || errors.Is(err, ErrDismissed) {
		t.Fatalf("Ask = %v; want the dialog's failure so the chain can fall through", err)
	}
}

func TestNotifyClosedWithoutActionStaysPending(t *testing.T) {
	notifyLog, busLog := notifyEnv(t, "closed")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type result struct {
		a   Answer
		err error
	}
	done := make(chan result, 1)
	go func() {
		a, err := fakeNotify().Ask(ctx, zenityPrompt())
		done <- result{a, err}
	}()
	waitUntil(t, "notify-send to run", func() bool { return len(readLog(notifyLog)) == 1 })
	select {
	case r := <-done:
		t.Fatalf("Ask returned %+v, %v although the notification was only dismissed", r.a, r.err)
	case <-time.After(150 * time.Millisecond):
	}
	if len(readLog(notifyLog)) != 1 {
		t.Fatal("a dismissed notification was shown again")
	}
	cancel()
	r := <-done
	if !errors.Is(r.err, context.Canceled) || r.a != (Answer{}) {
		t.Fatalf("Ask = %+v, %v; want context.Canceled", r.a, r.err)
	}
	// The daemon may still show a notification that notify-send stopped waiting for.
	if calls := readLog(busLog); len(calls) != 1 || !isClose(calls[0], "1001") {
		t.Fatalf("bus calls = %q; want CloseNotification 1001", calls)
	}
}

func isClose(args []string, id string) bool {
	for i, a := range args {
		if strings.HasSuffix(a, "CloseNotification") {
			rest := args[i+1:]
			return len(rest) > 0 && (rest[len(rest)-1] == id || rest[len(rest)-1] == "uint32 "+id)
		}
	}
	return false
}

func TestNotifyFailuresAreErrors(t *testing.T) {
	for _, tc := range []struct{ mode, want string }{
		{"fail", "boom: cannot show the notification"},
		{"noid", "no notification id"},
		{"unknown", `unknown action "bogus"`},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			notifyEnv(t, tc.mode)
			got, err := fakeNotify().Ask(t.Context(), zenityPrompt())
			if err == nil || !strings.Contains(err.Error(), tc.want) || got != (Answer{}) {
				t.Fatalf("Ask = %+v, %v; want an error containing %q", got, err, tc.want)
			}
		})
	}
}

func TestNotifyCancelClosesTheNotification(t *testing.T) {
	for _, tc := range []struct {
		name string
		n    func() *Notify
		want int // CloseNotification calls
		mode string
	}{
		{"busctl", fakeNotify, 1, "hang"},
		{"gdbus", func() *Notify {
			n := fakeNotify()
			n.Busctl, n.Gdbus = "", os.Args[0]
			return n
		}, 1, "hang"},
		{"id unknown", fakeNotify, 0, "hang-noid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notifyEnv(t, tc.mode)
			notifyLog, busLog := os.Getenv("MESSH_FAKE_NOTIFY_LOG"), os.Getenv("MESSH_FAKE_BUS_LOG")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := tc.n().Ask(ctx, zenityPrompt())
				done <- err
			}()
			if tc.mode == "hang" {
				waitUntil(t, "notify-send to show the notification", func() bool { return len(readLog(notifyLog)) == 1 })
			} else {
				time.Sleep(100 * time.Millisecond) // nothing is observable from outside
			}
			start := time.Now()
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("Ask = %v, want context.Canceled", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("Ask did not return after cancel")
			}
			if d := time.Since(start); d > 8*time.Second {
				t.Fatalf("cancel took %s", d)
			}
			calls := readLog(busLog)
			if len(calls) != tc.want || (tc.want == 1 && !isClose(calls[0], "1001")) {
				t.Fatalf("bus calls = %q; want %d CloseNotification 1001", calls, tc.want)
			}
		})
	}
}

func TestNotifyAsksRunConcurrently(t *testing.T) {
	const n = 8
	notifyEnv(t, "wait-all")
	t.Setenv("MESSH_FAKE_NOTIFY_DIR", t.TempDir())
	t.Setenv("MESSH_FAKE_NOTIFY_N", strconv.Itoa(n))
	surface := fakeNotify()
	answers := make([]Answer, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := zenityPrompt()
			p.ID = "req" + strconv.Itoa(i)
			answers[i], errs[i] = surface.Ask(t.Context(), p)
		}()
	}
	wg.Wait() // every fake notify-send waits for all the others, so this only ends if they run together
	for i := range n {
		// the stand-in allows even ids and denies odd ones
		want := Answer{Allow: i%2 == 0, Surface: "notify"}
		if errs[i] != nil || answers[i] != want {
			t.Errorf("request %d: %+v, %v; want %+v", i, answers[i], errs[i], want)
		}
	}
}

func TestParseNotifySend(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		id        uint32
		action    string
		bad       bool
	}{
		{"id and action", "42\nonce\n", 42, "once", false},
		{"windows line endings", "42\r\nalways\r\n", 42, "always", false},
		{"closed without action", "7\n", 7, "", false},
		{"nothing", "", 0, "", false},
		{"no id", "deny\n", 0, "deny", false},
		{"blank lines", "\n 9 \n\n options \n", 9, "options", false},
		{"two actions", "9\nonce\ndeny\n", 9, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, action, err := parseNotifySend(tc.out)
			if id != tc.id || action != tc.action || (err != nil) != tc.bad {
				t.Fatalf("parseNotifySend = %d, %q, %v; want %d, %q (error %v)", id, action, err, tc.id, tc.action, tc.bad)
			}
		})
	}
}

func argValue(args []string, prefix string) (string, bool) {
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, prefix); ok {
			return v, true
		}
	}
	return "", false
}

func TestNotifyArgs(t *testing.T) {
	now := time.Now()
	p := Prompt{
		ID:     "id/1 ;x",
		Title:  "--urgency=low <b>Run</b> & \x1b[31m\nnow",
		Caller: provider.Caller{DeviceName: "ra<spi>\x00", Agent: "om&p"},
		Details: []provider.Detail{
			{Label: "Command", Value: `echo "a\nb" <script>` + "\r\nrm -rf /"},
			{Label: "Dir\x07", Value: strings.Repeat("x", 500)},
			{Label: "Third", Value: "never shown"},
		},
		Expires: now.Add(90 * time.Second),
	}
	args := notifyArgs(p, now, true)

	sep := -1
	for i, a := range args {
		if a == "--" {
			sep = i
		}
	}
	if sep != len(args)-3 {
		t.Fatalf("args = %q: want summary and body after --", args)
	}
	title, body := args[sep+1], args[sep+2]
	for _, want := range []string{"--app-name=messh", "--urgency=critical", "--expire-time=90000", "--wait", "--print-id",
		"--action=default=Details", "--action=once=Allow once", "--action=always=Always allow", "--action=deny=Deny", "--action=options=More options",
		"--hint=string:x-canonical-private-synchronous:messh-id_1__x"} {
		found := false
		for _, a := range args[:sep] {
			found = found || a == want
		}
		if !found {
			t.Errorf("missing %q in %q", want, args[:sep])
		}
	}
	for _, a := range args[:sep] {
		if !strings.HasPrefix(a, "--") {
			t.Errorf("stray argument %q before --", a)
		}
	}

	if strings.ContainsAny(title, "\x1b\n\r<>") || !strings.Contains(title, "&lt;b&gt;Run&lt;/b&gt; &amp; ") || !strings.HasPrefix(title, "--urgency=low") {
		t.Errorf("title = %q", title)
	}
	lines := strings.Split(body, "\n")
	if len(lines) != 4 {
		t.Fatalf("body = %q: want who, two details and the hint on 4 lines", body)
	}
	if lines[0] != "ra&lt;spi&gt;  / om&amp;p" {
		t.Errorf("who = %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], `Command: echo "a\\nb" &lt;script&gt;`) || !strings.HasSuffix(lines[1], "\u23ce rm -rf /") {
		t.Errorf("command line = %q: backslashes must be doubled for g_strcompress and newlines shown as a mark", lines[1])
	}
	if !strings.HasPrefix(lines[2], "Dir : xxx") || utf8.RuneCountInString(lines[2]) > len("Dir : ")+160+3 {
		t.Errorf("long value was not bounded: %d runes", utf8.RuneCountInString(lines[2]))
	}
	if lines[3] != "Always allow = exactly this request" || strings.Contains(body, "Third") {
		t.Errorf("hint/extra details: %q", body)
	}
	for _, r := range body {
		if r < ' ' && r != '\n' {
			t.Errorf("control character %q in body", r)
		}
	}
	if strings.Contains(body, "<") || strings.Contains(body, ">") {
		t.Errorf("body %q has unescaped markup", body)
	}
}

func TestNotifyArgsExpiry(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		expires time.Time
		want    string
	}{
		{time.Time{}, "--expire-time=0"},
		{now.Add(5 * time.Minute), "--expire-time=300000"},
		{now.Add(10 * time.Millisecond), "--expire-time=1000"},
		{now.Add(-time.Hour), "--expire-time=1000"},
	} {
		p := zenityPrompt()
		p.Expires = tc.expires
		if got, _ := argValue(notifyArgs(p, now, true), "--expire-time="); "--expire-time="+got != tc.want {
			t.Errorf("Expires %v: --expire-time=%s, want %s", tc.expires, got, tc.want)
		}
	}
}

type dialogStub struct{ available bool }

func (d dialogStub) Name() string    { return "stub" }
func (d dialogStub) Available() bool { return d.available }
func (d dialogStub) Ask(context.Context, Prompt) (Answer, error) {
	return Answer{}, errors.New("unused")
}

func TestNotifyWithoutDialogOffersNoDetails(t *testing.T) {
	notifyLog, _ := notifyEnv(t, "once")
	n := fakeNotify()
	n.Dialog = dialogStub{available: false}
	if _, err := n.Ask(t.Context(), zenityPrompt()); err != nil {
		t.Fatal(err)
	}
	calls := readLog(notifyLog)
	if len(calls) != 1 {
		t.Fatalf("calls = %q", calls)
	}
	for _, a := range calls[0] {
		if strings.HasPrefix(a, "--action=default=") || strings.HasPrefix(a, "--action=options=") {
			t.Errorf("%q offered although no dialog can open", a)
		}
	}
	if _, ok := argValue(calls[0], "--action=once="); !ok {
		t.Error("the Allow once action is missing")
	}
}
