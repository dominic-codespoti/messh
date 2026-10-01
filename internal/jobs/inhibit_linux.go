//go:build linux

package jobs

import (
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// inhibitBin is the systemd-inhibit executable; tests swap in a stand-in.
var inhibitBin = "systemd-inhibit"

// inhibitSettle is how long Hold waits for systemd-inhibit to fail (polkit
// answers within a D-Bus round trip) before trusting that the lock holds.
var inhibitSettle = 300 * time.Millisecond

// inhibitStderrMax bounds the systemd-inhibit stderr kept for error messages.
const inhibitStderrMax = 1024

// linuxInhibitor holds a logind sleep inhibitor by keeping a
// `systemd-inhibit ... sleep` child alive. logind refuses block inhibitors
// to sessions polkit does not trust (ssh, headless boxes, WSL); the child
// then exits at once, which Hold reports instead of claiming the hold.
type linuxInhibitor struct {
	log *slog.Logger

	mu     sync.Mutex
	child  *inhibitChild // the live child; nil when not held
	failed string        // why the last hold failed or lapsed; "" after a good hold
	warned string        // the last lapse logged, so each kind is logged once
}

type inhibitChild struct {
	cmd    *exec.Cmd
	stderr *cappedBuffer
	done   chan struct{} // closed when the child has exited
	err    error         // cmd.Wait's result, valid once done is closed
}

func newSleepInhibitor(log *slog.Logger) SleepInhibitor {
	if log == nil {
		log = slog.Default()
	}
	return &linuxInhibitor{log: log}
}

func (l *linuxInhibitor) Hold() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.child != nil {
		return nil
	}
	if err := l.start(); err != nil {
		l.failed = err.Error()
		return err
	}
	l.failed = ""
	return nil
}

// start launches the child and waits inhibitSettle for an early exit. l.mu
// is held.
func (l *linuxInhibitor) start() error {
	exe, err := exec.LookPath(inhibitBin)
	if err != nil {
		return errors.New("systemd-inhibit not found; the device may sleep during jobs")
	}
	// A very long sleep rather than "infinity", which BusyBox does not know.
	cmd := exec.Command(exe, "--what=sleep", "--who=messh", "--why=messh is running a job", "--mode=block", "sleep", "2147483647")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c := &inhibitChild{cmd: cmd, stderr: &cappedBuffer{max: inhibitStderrMax}, done: make(chan struct{})}
	cmd.Stderr = c.stderr
	// An orphaned `sleep` keeps the stderr pipe open; do not let that hide
	// systemd-inhibit's own exit.
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("systemd-inhibit: %v (the device may sleep during jobs)", err)
	}
	go l.watch(c)
	select {
	case <-c.done:
		return errors.New(c.reason())
	case <-time.After(inhibitSettle):
	}
	l.child = c
	return nil
}

// watch reaps the child. If it exits while it is still the hold (not killed
// by Release), the hold has lapsed: log it and let the next Hold retry.
func (l *linuxInhibitor) watch(c *inhibitChild) {
	c.err = c.cmd.Wait()
	close(c.done)
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.child != c {
		return // released, or it failed inside Hold
	}
	l.child = nil
	l.failed = "lapsed: " + c.reason()
	if l.failed != l.warned {
		l.warned = l.failed
		l.log.Warn("the sleep inhibitor ended while jobs run; the device may sleep", "error", c.reason())
	}
}

// reason describes why the child exited. Valid once done is closed.
func (c *inhibitChild) reason() string {
	var lines []string
	for _, s := range strings.Split(c.stderr.String(), "\n") {
		if s = strings.TrimSpace(s); s != "" {
			lines = append(lines, s)
		}
	}
	msg := strings.Join(lines, "; ")
	if msg == "" {
		if c.err != nil {
			msg = "exited: " + c.err.Error()
		} else {
			msg = "exited"
		}
	}
	hint := "the device may sleep during jobs"
	if strings.Contains(msg, "Access denied") {
		hint = "polkit refuses block inhibitors for this session; " + hint
	}
	return "systemd-inhibit: " + msg + " (" + hint + ")"
}

func (l *linuxInhibitor) Release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := l.child
	if c == nil {
		return
	}
	l.child = nil
	syscall.Kill(-c.cmd.Process.Pid, syscall.SIGKILL)
	<-c.done
}

func (l *linuxInhibitor) Status() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.child != nil:
		return "held"
	case l.failed != "":
		return "unavailable: " + l.failed
	}
	return "not held"
}

func (l *linuxInhibitor) Close() { l.Release() }

// cappedBuffer keeps the first max bytes written and drops the rest, so a
// chatty child cannot grow it without bound.
type cappedBuffer struct {
	mu  sync.Mutex
	max int
	b   []byte
}

func (w *cappedBuffer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if n := w.max - len(w.b); n > 0 {
		w.b = append(w.b, p[:min(n, len(p))]...)
	}
	return len(p), nil
}

func (w *cappedBuffer) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return string(w.b)
}
