package jobs

import (
	"os"
	"time"
)

// launchSpec is what the scheduler hands to the OS layer.
type launchSpec struct {
	ID         string
	Path       string
	Args       []string
	Shell      bool
	Line       string
	Dir        string
	Env        []string
	Stdout     *os.File
	Stderr     *os.File
	MemLimitMB int64
	KillGrace  time.Duration
	NoScope    bool // never wrap in a systemd scope (tests)
}

// process is a started job: the child plus everything it spawns.
type process interface {
	PID() int
	// Token identifies this process incarnation (its start time) so a stale
	// PID is never mistaken for it after a restart.
	Token() string
	// Unit names the systemd scope holding the tree, if any.
	Unit() string
	// Notes lists limits that could not be enforced.
	Notes() []string
	// Wait blocks until the main process exits, then tears down anything it
	// left behind. exit is -1 when the process was killed by a signal.
	Wait() (exit int, err error)
	// Terminate asks the tree to stop and escalates to Kill after the grace
	// period. It does not block.
	Terminate()
	// Kill ends the whole tree immediately.
	Kill()
}
