package approval

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Native shows a Windows task dialog (comctl32 TaskDialogIndirect). It needs
// the version 6 common controls, which a process only gets through a
// manifest; messh embeds none, so the dialog thread activates an activation
// context created at run time from an equivalent manifest.
type Native struct {
	// Dismissable makes Esc and the close button return ErrDismissed instead
	// of denying: the dialog is the "more options" view of a notification,
	// and closing it should leave the request pending.
	Dismissable bool
}

func (Native) Name() string    { return "windows" }
func (Native) Available() bool { return true }

var (
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	user32   = windows.NewLazySystemDLL("user32.dll")
	comctl32 = windows.NewLazySystemDLL("comctl32.dll")

	procCreateActCtx     = kernel32.NewProc("CreateActCtxW")
	procActivateActCtx   = kernel32.NewProc("ActivateActCtx")
	procDeactivateActCtx = kernel32.NewProc("DeactivateActCtx")

	procSetWindowPos        = user32.NewProc("SetWindowPos")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procPostMessage         = user32.NewProc("PostMessageW")
	procFlashWindowEx       = user32.NewProc("FlashWindowEx")

	procTaskDialogIndirect = comctl32.NewProc("TaskDialogIndirect")
)

const commonControlsManifest = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<assembly xmlns="urn:schemas-microsoft-com:asm.v1" manifestVersion="1.0">
  <dependency>
    <dependentAssembly>
      <assemblyIdentity type="win32" name="Microsoft.Windows.Common-Controls" version="6.0.0.0" processorArchitecture="*" publicKeyToken="6595b64144ccf1df" language="*"/>
    </dependentAssembly>
  </dependency>
</assembly>`

// actCtx is the shared activation context handle, created once.
var actCtx struct {
	once   sync.Once
	handle uintptr
	err    error
}

type actCtxW struct {
	size                  uint32
	flags                 uint32
	source                *uint16
	processorArchitecture uint16
	langID                uint16
	assemblyDirectory     *uint16
	resourceName          *uint16
	applicationName       *uint16
	module                uintptr
}

const invalidHandle = ^uintptr(0)

func createActCtx() (uintptr, error) {
	f, err := os.CreateTemp("", "messh-comctl-*.manifest")
	if err != nil {
		return 0, err
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString(commonControlsManifest)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	src, err := windows.UTF16PtrFromString(f.Name())
	if err != nil {
		return 0, err
	}
	ctx := actCtxW{source: src}
	ctx.size = uint32(unsafe.Sizeof(ctx))
	h, _, e := procCreateActCtx.Call(uintptr(unsafe.Pointer(&ctx)))
	runtime.KeepAlive(src)
	if h == invalidHandle {
		return 0, fmt.Errorf("CreateActCtx: %w", e)
	}
	return h, nil
}

// Task dialog constants (commctrl.h).
const (
	tdfAllowDialogCancellation = 0x0008
	tdnCreated                 = 0
	tdnDestroyed               = 5
	tdmClickButton             = 0x400 + 102 // WM_USER+102
	idCancel                   = 2
	tdWarningIcon              = 0xFFFF // MAKEINTRESOURCE(-1)

	btnAllowOnce = 1001
	btnAlways    = 1002
	btnDeny      = 1003
	radioBase    = 2000

	swpNoSize     = 0x0001
	swpNoMove     = 0x0002
	swpShowWindow = 0x0040
	hwndTopmost   = ^uintptr(0)
)

// dialogs maps the callback's reference value to per-dialog state; there is
// a single callback because syscall.NewCallback slots are never released.
var dialogs struct {
	mu   sync.Mutex
	next uintptr
	m    map[uintptr]*dialogState
	cb   uintptr
	once sync.Once
}

type dialogState struct {
	mu        sync.Mutex
	hwnd      uintptr
	cancelled bool
}

func (s *dialogState) close() {
	s.mu.Lock()
	s.cancelled = true
	h := s.hwnd
	s.mu.Unlock()
	if h != 0 {
		// The dialog treats a click on Cancel like the user dismissing it.
		procPostMessage.Call(h, tdmClickButton, idCancel, 0)
	}
}

func dialogCallback(hwnd, msg, wparam, lparam, ref uintptr) uintptr {
	dialogs.mu.Lock()
	st := dialogs.m[ref]
	dialogs.mu.Unlock()
	if st == nil {
		return 0
	}
	switch msg {
	case tdnCreated:
		st.mu.Lock()
		st.hwnd = hwnd
		cancelled := st.cancelled
		st.mu.Unlock()
		bringToFront(hwnd)
		if cancelled {
			procPostMessage.Call(hwnd, tdmClickButton, idCancel, 0)
		}
	case tdnDestroyed:
		st.mu.Lock()
		st.hwnd = 0
		st.mu.Unlock()
	}
	return 0
}

// bringToFront puts the dialog above other windows. The node runs in the
// background, where Windows may refuse to give it the foreground; a topmost
// window stays visible regardless, and a taskbar flash covers the rest. (An
// AttachThreadInput focus-stealing trick was tried and dropped: it got the
// test binary quarantined by Windows Defender as Trojan:Win32/Bearfoos.A!ml,
// and the topmost window already came up in front without it.)
func bringToFront(hwnd uintptr) {
	procSetWindowPos.Call(hwnd, hwndTopmost, 0, 0, 0, 0, swpNoMove|swpNoSize|swpShowWindow)
	r, _, _ := procSetForegroundWindow.Call(hwnd)
	if r == 0 {
		// Could not take focus: flash the taskbar button until the user notices.
		type flashInfo struct {
			size    uint32
			hwnd    uintptr
			flags   uint32
			count   uint32
			timeout uint32
		}
		fi := flashInfo{hwnd: hwnd, flags: 0x3 | 0xC, count: 0} // FLASHW_ALL | FLASHW_TIMERNOFG
		fi.size = uint32(unsafe.Sizeof(fi))
		procFlashWindowEx.Call(uintptr(unsafe.Pointer(&fi)))
	}
}

// packed builds C structures declared under #pragma pack(1), which Go
// structs cannot express.
type packed struct{ b []byte }

func (p *packed) u32(v uint32) { p.b = binary.NativeEndian.AppendUint32(p.b, v) }
func (p *packed) ptr(v uintptr) {
	if unsafe.Sizeof(v) == 8 {
		p.b = binary.NativeEndian.AppendUint64(p.b, uint64(v))
	} else {
		p.b = binary.NativeEndian.AppendUint32(p.b, uint32(v))
	}
}

// strings keeps UTF-16 buffers alive and pinned while native code holds their addresses.
type strings16 struct {
	keep   [][]uint16
	pinner runtime.Pinner
}

func (s *strings16) of(text string) uintptr {
	u, err := windows.UTF16FromString(text)
	if err != nil { // embedded NUL: Sanitize prevents it, but never fail a prompt over text
		u, _ = windows.UTF16FromString("?")
	}
	s.keep = append(s.keep, u)
	s.pinner.Pin(&u[0])
	return uintptr(unsafe.Pointer(&u[0]))
}

func (s *strings16) buttons(items []tdButton) (uintptr, *packed) {
	p := &packed{}
	for _, it := range items {
		p.u32(uint32(it.id))
		p.ptr(s.of(it.text))
	}
	s.pinner.Pin(&p.b[0])
	return uintptr(unsafe.Pointer(&p.b[0])), p
}

type tdButton struct {
	id   int
	text string
}

// Ask shows the dialog on a dedicated OS thread (activation contexts and the
// dialog's message loop are per-thread) and returns when it is answered or ctx ends.
func (n Native) Ask(ctx context.Context, p Prompt) (Answer, error) {
	type result struct {
		ans Answer
		err error
	}
	st := &dialogState{}
	done := make(chan result, 1)
	finished := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			st.close()
		case <-finished:
		}
	}()
	go func() {
		defer close(finished)
		runtime.LockOSThread() // never unlocked: the thread ends with this goroutine
		ans, err := showTaskDialog(p, st, n.Dismissable)
		done <- result{ans, err}
	}()
	select {
	case r := <-done:
		if ctx.Err() != nil {
			return Answer{}, ctx.Err()
		}
		return r.ans, r.err
	case <-ctx.Done():
		return Answer{}, ctx.Err()
	}
}

// dialogMu keeps one dialog on screen at a time even across Native values.
var dialogMu sync.Mutex

func showTaskDialog(p Prompt, st *dialogState, dismissable bool) (Answer, error) {
	dialogMu.Lock()
	defer dialogMu.Unlock()
	st.mu.Lock()
	cancelled := st.cancelled
	st.mu.Unlock()
	if cancelled {
		return Answer{}, context.Canceled
	}

	actCtx.once.Do(func() { actCtx.handle, actCtx.err = createActCtx() })
	if actCtx.err != nil {
		return Answer{}, actCtx.err
	}
	var cookie uintptr
	if r, _, e := procActivateActCtx.Call(actCtx.handle, uintptr(unsafe.Pointer(&cookie))); r == 0 {
		return Answer{}, fmt.Errorf("ActivateActCtx: %w", e)
	}
	defer procDeactivateActCtx.Call(0, cookie)
	if err := procTaskDialogIndirect.Find(); err != nil {
		return Answer{}, fmt.Errorf("TaskDialogIndirect unavailable: %w", err)
	}

	dialogs.once.Do(func() {
		dialogs.m = map[uintptr]*dialogState{}
		dialogs.cb = syscall.NewCallback(dialogCallback)
	})
	dialogs.mu.Lock()
	dialogs.next++
	ref := dialogs.next
	dialogs.m[ref] = st
	dialogs.mu.Unlock()
	defer func() {
		dialogs.mu.Lock()
		delete(dialogs.m, ref)
		dialogs.mu.Unlock()
	}()

	var s strings16
	defer s.pinner.Unpin()

	buttons, btnBuf := s.buttons([]tdButton{
		{btnAllowOnce, "Allow once"},
		{btnAlways, "Always allow"},
		{btnDeny, "Deny"},
	})
	var radios uintptr
	var radioBuf *packed
	var items []tdButton
	if len(p.Scopes) > 1 {
		for i, sc := range p.Scopes {
			items = append(items, tdButton{radioBase + i, FormatScope(sc)})
		}
		radios, radioBuf = s.buttons(items)
	}
	footer := `"Always allow" saves a rule for ` + FormatCaller(p.Caller) + " only."
	if len(p.Scopes) > 1 {
		footer = `"Always allow" uses the choice selected above, and applies to ` + FormatCaller(p.Caller) + " only."
	}
	if note := WaitingNote(p); note != "" {
		footer += "  (" + note + ")"
	}

	var c packed
	c.u32(0) // cbSize, patched below
	c.ptr(0) // hwndParent
	c.ptr(0) // hInstance
	c.u32(tdfAllowDialogCancellation)
	c.u32(0) // dwCommonButtons
	c.ptr(s.of("messh approval"))
	c.ptr(tdWarningIcon)
	c.ptr(s.of(Sanitize(p.Title, 200)))
	c.ptr(s.of(Body(p)))
	c.u32(3) // cButtons
	c.ptr(buttons)
	c.u32(btnDeny) // nDefaultButton: a stray Enter must not approve
	c.u32(uint32(len(items)))
	c.ptr(radios)
	c.u32(radioBase) // nDefaultRadioButton: the exact request
	c.ptr(0)         // pszVerificationText
	c.ptr(0)         // pszExpandedInformation
	c.ptr(0)         // pszExpandedControlText
	c.ptr(0)         // pszCollapsedControlText
	c.ptr(0)         // hFooterIcon
	c.ptr(s.of(footer))
	c.ptr(dialogs.cb)
	c.ptr(ref)
	c.u32(240) // cxWidth in dialog units
	binary.NativeEndian.PutUint32(c.b, uint32(len(c.b)))
	s.pinner.Pin(&c.b[0])

	var button, radio int32
	hr, _, _ := procTaskDialogIndirect.Call(uintptr(unsafe.Pointer(&c.b[0])),
		uintptr(unsafe.Pointer(&button)), uintptr(unsafe.Pointer(&radio)), 0)
	runtime.KeepAlive(btnBuf)
	runtime.KeepAlive(radioBuf)
	runtime.KeepAlive(&s)
	if int32(hr) < 0 {
		return Answer{}, fmt.Errorf("TaskDialogIndirect failed: %w", syscall.Errno(uint32(hr)))
	}
	switch button {
	case btnAllowOnce:
		return Answer{Allow: true}, nil
	case btnAlways:
		scope := 0
		if len(items) > 0 {
			scope = int(radio) - radioBase
			if scope < 0 || scope >= len(items) {
				return Answer{}, errors.New("dialog returned an unknown scope")
			}
		}
		return Answer{Allow: true, Always: true, Scope: scope}, nil
	}
	if button == idCancel && dismissable {
		return Answer{}, ErrDismissed
	}
	return Answer{}, nil // Deny, Esc or the close button
}
