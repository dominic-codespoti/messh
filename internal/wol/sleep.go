package wol

import (
	"strings"
	"time"
)

// PowerEvent is a sleep transition of this device.
type PowerEvent int

const (
	Suspending PowerEvent = iota + 1 // about to sleep; the network is still up
	Resumed                          // back from sleep
)

func (e PowerEvent) String() string {
	switch e {
	case Suspending:
		return "suspending"
	case Resumed:
		return "resumed"
	}
	return "unknown"
}

// suspendBudget bounds how long a Suspending handler may hold off sleep.
// Windows allows about two seconds for PBT_APMSUSPEND; logind waits up to
// InhibitDelayMaxSec (default 5 s) for delay inhibitors.
const suspendBudget = 1800 * time.Millisecond

// monitorParser turns logind PrepareForSleep signals printed by
// `gdbus monitor` (one line: "...Manager.PrepareForSleep (true,)") or
// `dbus-monitor` (a "member=PrepareForSleep" header, then "boolean true")
// into events.
type monitorParser struct {
	pending bool // dbus-monitor header seen, argument line next
}

func (p *monitorParser) feed(line string) (PowerEvent, bool) {
	if p.pending {
		p.pending = false
		switch strings.TrimSpace(line) {
		case "boolean true":
			return Suspending, true
		case "boolean false":
			return Resumed, true
		}
		return 0, false
	}
	i := strings.Index(line, "PrepareForSleep")
	if i < 0 {
		return 0, false
	}
	rest := line[i+len("PrepareForSleep"):]
	switch {
	case strings.HasPrefix(strings.TrimSpace(rest), "(true"):
		return Suspending, true
	case strings.HasPrefix(strings.TrimSpace(rest), "(false"):
		return Resumed, true
	case strings.Contains(line, "member=PrepareForSleep"):
		p.pending = true
	}
	return 0, false
}

// runBounded runs fn(ev) but stops waiting for it after suspendBudget, so a
// slow network never holds the machine awake.
func runBounded(fn func(PowerEvent), ev PowerEvent) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn(ev)
	}()
	select {
	case <-done:
	case <-time.After(suspendBudget):
	}
}
