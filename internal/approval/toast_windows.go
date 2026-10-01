package approval

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// toastAUMID is messh's AppUserModelID: the identity Windows files its
// notifications under. An unpackaged program gets one by naming it in
// HKCU\Software\Classes\AppUserModelId.
const (
	toastAUMID  = "messh"
	toastGroup  = "messh"
	aumidKey    = `Software\Classes\AppUserModelId\` + toastAUMID
	protocolKey = `Software\Classes\` + ProtocolScheme
)

// Toast shows approval prompts as Windows notifications (Action Center
// style, bottom right) with Allow once / Always allow / Deny / More options
// buttons. Several can be on screen at once.
//
// The buttons are protocol activations: clicking one launches
// messh-approve://respond?... , which `messh respond` turns into an HTTP
// call to the node. That works after the banner has moved to Action Center
// and needs no COM server in the node. The protocol handler and the
// AppUserModelID are registered under HKCU, verified before every toast, and
// removed (after the toasts) by `messh approvals unregister`.
//
// Closing a toast without a choice leaves the request pending.
type Toast struct {
	// Options is the fuller view opened by "More options" and by clicking the
	// toast; its answer resolves the request. Default: Native{Dismissable: true}.
	Options Surface

	mu      sync.Mutex
	log     *slog.Logger
	started bool
	initErr error
	live    map[string]*liveToast
}

// liveToast is a toast on screen for one request.
type liveToast struct {
	prompt  Prompt
	ctx     context.Context
	answers chan Answer
	options atomic.Bool // the options dialog is open
}

func (t *Toast) Name() string   { return SurfaceToast }
func (t *Toast) Parallel() bool { return true }

func (t *Toast) SetLogger(l *slog.Logger) {
	t.mu.Lock()
	t.log = l
	t.mu.Unlock()
}

func (t *Toast) logger() *slog.Logger {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.log == nil {
		return slog.Default()
	}
	return t.log
}

// Available reports whether toasts can work at all: Windows 10 or later and
// a Windows Runtime that initialised. Whether this particular toast can be
// shown (registration, notification settings) is checked per request.
func (t *Toast) Available() bool {
	if v := windows.RtlGetVersion(); v.MajorVersion < 10 {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.initErr == nil
}

func (t *Toast) options() Surface {
	if t.Options != nil {
		return t.Options
	}
	return Native{Dismissable: true}
}

// start does the one-time setup: bring up the Windows Runtime and remove
// toasts a previous node left behind, whose buttons would reach nobody.
func (t *Toast) start() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.started {
		return t.initErr
	}
	t.started = true
	t.live = map[string]*liveToast{}
	if err := registerToast(); err != nil {
		t.initErr = err
		return err
	}
	if err := clearToasts(toastAUMID); err != nil {
		t.initErr = fmt.Errorf("Windows notifications are not available: %w", err)
	}
	return t.initErr
}

func (t *Toast) Ask(ctx context.Context, p Prompt) (Answer, error) {
	if err := t.start(); err != nil {
		return Answer{}, fmt.Errorf("toast setup: %w", err)
	}
	if p.Port == 0 || p.Nonce == "" {
		return Answer{}, errors.New("the node's local API port is unknown, so toast buttons could not reach it")
	}
	lt := &liveToast{prompt: p, ctx: ctx, answers: make(chan Answer, 1)}
	lt.prompt.Waiting = 0
	t.mu.Lock()
	t.live[p.ID] = lt
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		delete(t.live, p.ID)
		t.mu.Unlock()
		// Whatever ended the request (an answer here, the CLI, a timeout, shutdown),
		// its toast must not stay: its buttons would be dead.
		if err := hideToast(toastAUMID, p.ID, toastGroup); err != nil {
			t.logger().Warn("could not remove the approval toast", "id", p.ID, "error", err)
		}
	}()
	if err := t.show(lt); err != nil {
		return Answer{}, err
	}
	select {
	case a := <-lt.answers:
		return a, nil
	case <-ctx.Done():
		return Answer{}, ctx.Err()
	}
}

// show (re)displays the toast after checking the registration it depends on.
func (t *Toast) show(lt *liveToast) error {
	if err := registerToast(); err != nil {
		return fmt.Errorf("toast buttons cannot be relied on: %w", err)
	}
	p := lt.prompt
	xml := toastXML(p, func(action string) string {
		return RespondURL(RespondTarget{Port: p.Port, ID: p.ID, Nonce: p.Nonce, Action: action})
	})
	if err := showToast(toastAUMID, xml, p.ID, toastGroup, p.Expires); err != nil {
		return fmt.Errorf("show toast: %w", err)
	}
	return nil
}

// ShowDetails opens the options dialog for a request whose toast is on screen.
func (t *Toast) ShowDetails(id string) error {
	t.mu.Lock()
	lt := t.live[id]
	t.mu.Unlock()
	if lt == nil {
		return ErrNotShown
	}
	if !lt.options.CompareAndSwap(false, true) {
		return nil // already open
	}
	go func() {
		defer lt.options.Store(false)
		opts := t.options()
		ans, err := opts.Ask(lt.ctx, lt.prompt)
		switch {
		case lt.ctx.Err() != nil:
		case err == nil:
			if ans.Surface == "" {
				ans.Surface = opts.Name()
			}
			select {
			case lt.answers <- ans:
			default:
			}
		default:
			// Closed without a choice, or could not be shown: the toast went away
			// when it was clicked, so put it back and keep waiting.
			if !errors.Is(err, ErrDismissed) {
				t.logger().Warn("approval details dialog failed", "id", lt.prompt.ID, "error", err)
			}
			if err := t.show(lt); err != nil {
				t.logger().Warn("could not show the approval toast again", "id", lt.prompt.ID, "error", err)
			}
		}
	}()
	return nil
}

// handlerCommand is the command line Windows runs for a messh-approve:// URL.
// messh.exe is a console program, which would flash a console window for the
// moment it runs; conhost --headless runs it without one.
func handlerCommand(exe string) (string, error) {
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return "", err
	}
	conhost := filepath.Join(sys, "conhost.exe")
	if _, err := os.Stat(conhost); err != nil {
		return "", err
	}
	return `"` + conhost + `" --headless "` + exe + `" respond --notify "%1"`, nil
}

// registerToast makes sure the protocol handler and the AppUserModelID are
// present and point at this executable, and reads them back. It is cheap, and
// runs before every toast so that a deleted or stale registration is repaired
// rather than leaving buttons that open Windows' "Get an app" dialog.
func registerToast() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	if _, err := os.Stat(exe); err != nil {
		return fmt.Errorf("messh executable is missing: %w", err)
	}
	cmd, err := handlerCommand(exe)
	if err != nil {
		return err
	}
	want := []struct{ key, name, val string }{
		{protocolKey, "", "URL:messh approval"},
		{protocolKey, "URL Protocol", ""},
		{protocolKey + `\shell\open\command`, "", cmd},
		{aumidKey, "DisplayName", "messh"},
	}
	for pass := 0; pass < 2; pass++ {
		missing := false
		for _, w := range want {
			k, _, err := registry.CreateKey(registry.CURRENT_USER, w.key, registry.QUERY_VALUE|registry.SET_VALUE)
			if err != nil {
				return fmt.Errorf("registry %s: %w", w.key, err)
			}
			got, _, gerr := k.GetStringValue(w.name)
			if gerr != nil || got != w.val {
				missing = true
				if err := k.SetStringValue(w.name, w.val); err != nil {
					k.Close()
					return fmt.Errorf("registry %s: %w", w.key, err)
				}
			}
			k.Close()
		}
		if !missing {
			return nil // present and correct, as read back
		}
	}
	return errors.New("the messh-approve:// registration did not stick")
}

// UnregisterNotifications removes messh's toasts from the screen and Action
// Center first (a toast whose handler is gone would open Windows' "Get an
// app" dialog) and only then the registry keys created by registerToast.
func UnregisterNotifications() (string, error) {
	var errs []error
	if err := clearToasts(toastAUMID); err != nil {
		errs = append(errs, fmt.Errorf("remove toasts: %w", err))
	}
	removed := 0
	for _, key := range []string{
		protocolKey + `\shell\open\command`, protocolKey + `\shell\open`, protocolKey + `\shell`, protocolKey, aumidKey,
	} {
		switch err := registry.DeleteKey(registry.CURRENT_USER, key); {
		case err == nil:
			removed++
		case errors.Is(err, registry.ErrNotExist):
		default:
			errs = append(errs, fmt.Errorf("delete %s: %w", key, err))
		}
	}
	msg := "Removed messh's toasts and its HKCU registrations (messh-approve:// and the app ID)."
	if removed == 0 && len(errs) == 0 {
		msg = "Nothing to remove: messh had registered nothing."
	}
	return msg, errors.Join(errs...)
}
