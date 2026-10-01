//go:build windows

package jobs

import (
	"errors"
	"log/slog"
	"runtime"
	"sync"
	"syscall"
)

const (
	esContinuous     = 0x80000000
	esSystemRequired = 0x00000001
)

var procSetThreadExecutionState = syscall.NewLazyDLL("kernel32.dll").NewProc("SetThreadExecutionState")

// winInhibitor applies ES_CONTINUOUS|ES_SYSTEM_REQUIRED from one dedicated
// locked OS thread. The setting belongs to the calling thread and lapses
// when it exits, so the thread must outlive the Go scheduler's migrations.
type winInhibitor struct {
	once sync.Once
	reqs chan inhibitReq
	done chan struct{}
}

type inhibitReq struct {
	hold bool
	ack  chan error
}

// newSleepInhibitor ignores log: the execution state cannot lapse on its own,
// so Hold's error is all there is to report.
func newSleepInhibitor(*slog.Logger) SleepInhibitor { return &winInhibitor{} }

func (w *winInhibitor) start() {
	w.reqs = make(chan inhibitReq)
	w.done = make(chan struct{})
	go func() {
		runtime.LockOSThread() // never unlocked: the thread dies with this goroutine
		defer close(w.done)
		for r := range w.reqs {
			flags := uintptr(esContinuous)
			if r.hold {
				flags |= esSystemRequired
			}
			prev, _, _ := procSetThreadExecutionState.Call(flags)
			if prev == 0 {
				r.ack <- errors.New("SetThreadExecutionState failed")
				continue
			}
			r.ack <- nil
		}
		procSetThreadExecutionState.Call(esContinuous)
	}()
}

func (w *winInhibitor) set(hold bool) error {
	w.once.Do(w.start)
	ack := make(chan error, 1)
	w.reqs <- inhibitReq{hold, ack}
	return <-ack
}

func (w *winInhibitor) Hold() error { return w.set(true) }
func (w *winInhibitor) Release()    { w.set(false) }

// Close ends the helper thread (after clearing the request).
func (w *winInhibitor) Close() {
	w.once.Do(w.start)
	close(w.reqs)
	<-w.done
}
