//go:build windows

package jobs

import (
	"runtime"
	"testing"
)

// The execution state is write-only, so the previous-state return value of
// SetThreadExecutionState is the only observable proof that a request landed.
func TestExecutionStateIsAppliedPerThread(t *testing.T) {
	done := make(chan uintptr)
	go func() {
		runtime.LockOSThread()
		procSetThreadExecutionState.Call(esContinuous | esSystemRequired)
		prev, _, _ := procSetThreadExecutionState.Call(esContinuous)
		done <- prev
	}()
	if prev := <-done; prev != esContinuous|esSystemRequired {
		t.Fatalf("previous execution state = %#x, want ES_CONTINUOUS|ES_SYSTEM_REQUIRED", prev)
	}
}

func TestWindowsInhibitorLifecycle(t *testing.T) {
	in := newSleepInhibitor(nil).(*winInhibitor)
	if err := in.Hold(); err != nil {
		t.Fatal(err)
	}
	in.Release()
	if err := in.Hold(); err != nil {
		t.Fatal(err)
	}
	in.Close()
}
