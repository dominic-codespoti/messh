package browser

import (
	"bytes"
	"io"
	"sync"
	"time"
)

// treeSpec describes the upstream process.
type treeSpec struct {
	Path   string
	Args   []string
	Dir    string
	Env    []string
	Output io.Writer // stdout and stderr
}

// tree is a started process plus everything it spawns (node, npm, the
// browser). Kill ends all of it; nothing may survive the node.
type tree interface {
	PID() int
	// Token identifies this incarnation (its start time), so a recorded PID is
	// never mistaken for another process after a restart.
	Token() string
	// Wait blocks until the main process exits and then removes leftovers.
	Wait() error
	// Kill ends the whole tree at once; safe to call repeatedly.
	Kill()
}

// killWait bounds how long Wait lingers for output pipes held open by
// stragglers once the main process is gone.
const killWait = 3 * time.Second

// tailBuffer keeps the last bytes written, for error messages.
type tailBuffer struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func newTail(max int) *tailBuffer { return &tailBuffer{max: max} }

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = append([]byte(nil), t.buf[len(t.buf)-t.max:]...)
	}
	return len(p), nil
}

// String returns the buffered text, trimmed.
func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(bytes.TrimSpace(t.buf))
}
