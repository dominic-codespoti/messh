//go:build windows

package wol

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

// PowerRegisterSuspendResumeNotification (Windows 8+) with
// DEVICE_NOTIFY_CALLBACK delivers PBT_* events to a callback on a system
// thread: no window and no message loop, and it works for services too.
const (
	deviceNotifyCallback  = 2
	pbtAPMSuspend         = 0x4
	pbtAPMResumeSuspend   = 0x7
	pbtAPMResumeAutomatic = 0x12
)

var (
	powrprof            = syscall.NewLazyDLL("powrprof.dll")
	procPowerRegister   = powrprof.NewProc("PowerRegisterSuspendResumeNotification")
	procPowerUnregister = powrprof.NewProc("PowerUnregisterSuspendResumeNotification")
)

// deviceNotifySubscribeParameters is DEVICE_NOTIFY_SUBSCRIBE_PARAMETERS.
type deviceNotifySubscribeParameters struct {
	callback uintptr
	context  uintptr
}

var (
	callbackOnce sync.Once
	callbackPtr  uintptr

	// params stays referenced while registered: Windows keeps the pointer.
	params *deviceNotifySubscribeParameters

	watchMu   sync.Mutex // one watcher at a time
	handler   atomic.Pointer[func(PowerEvent)]
	suspended atomic.Bool
)

func powerCallback(_, typ, _ uintptr) uintptr {
	fn := handler.Load()
	if fn == nil {
		return 0
	}
	switch typ {
	case pbtAPMSuspend:
		if !suspended.Swap(true) {
			// Blocking here delays the suspend, so the announcements leave
			// while the NIC is still up.
			runBounded(*fn, Suspending)
		}
	case pbtAPMResumeSuspend, pbtAPMResumeAutomatic:
		if suspended.Swap(false) {
			go (*fn)(Resumed) // never hold up the resume path
		}
	}
	return 0
}

// WatchSleep calls fn(Suspending) when this device is about to sleep and
// fn(Resumed) after it wakes, until ctx ends. Sleep waits for fn(Suspending)
// up to about 1.8 s. It fails at once when the OS offers no notification.
func WatchSleep(ctx context.Context, _ *slog.Logger, fn func(PowerEvent)) error {
	if err := procPowerRegister.Find(); err != nil {
		return fmt.Errorf("suspend notifications unavailable (needs Windows 8 or newer): %w", err)
	}
	if !watchMu.TryLock() {
		return errors.New("a sleep watcher is already running in this process")
	}
	defer watchMu.Unlock()
	callbackOnce.Do(func() {
		callbackPtr = syscall.NewCallback(powerCallback)
		params = &deviceNotifySubscribeParameters{callback: callbackPtr}
	})
	handler.Store(&fn)
	defer handler.Store(nil)
	var reg uintptr
	r, _, _ := procPowerRegister.Call(deviceNotifyCallback, uintptr(unsafe.Pointer(params)), uintptr(unsafe.Pointer(&reg)))
	if r != 0 {
		return fmt.Errorf("PowerRegisterSuspendResumeNotification: %w", syscall.Errno(r))
	}
	<-ctx.Done()
	procPowerUnregister.Call(reg)
	return nil
}
