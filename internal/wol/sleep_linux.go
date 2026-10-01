//go:build linux

package wol

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// WatchSleep calls fn(Suspending) when logind announces PrepareForSleep(true)
// and fn(Resumed) on PrepareForSleep(false), until ctx ends. The signal is
// read from `gdbus monitor` (glib) or `dbus-monitor` as a child process. A
// logind delay inhibitor (systemd-inhibit --mode=delay) holds off the
// suspend until fn returns, at most about 1.8 s.
func WatchSleep(ctx context.Context, log *slog.Logger, fn func(PowerEvent)) error {
	if log == nil {
		log = slog.Default()
	}
	argv, err := monitorArgv()
	if err != nil {
		return err
	}
	lock := &delayLock{log: log}
	lock.take()
	defer lock.release()
	for {
		err := runMonitor(ctx, argv, func(ev PowerEvent) {
			switch ev {
			case Suspending:
				runBounded(fn, ev)
				lock.release() // let the suspend proceed
			case Resumed:
				lock.take()
				go fn(ev)
			}
		})
		if ctx.Err() != nil {
			return nil
		}
		log.Warn("logind sleep monitor exited; restarting in 30s", "cmd", argv[0], "error", err)
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(30 * time.Second):
		}
	}
}

func monitorArgv() ([]string, error) {
	if exe, err := exec.LookPath("gdbus"); err == nil {
		return []string{exe, "monitor", "--system", "--dest", "org.freedesktop.login1", "--object-path", "/org/freedesktop/login1"}, nil
	}
	if exe, err := exec.LookPath("dbus-monitor"); err == nil {
		return []string{exe, "--system", "type='signal',interface='org.freedesktop.login1.Manager',member='PrepareForSleep'"}, nil
	}
	return nil, errors.New("neither gdbus (glib2) nor dbus-monitor found; cannot hear logind sleep signals")
}

// runMonitor runs the monitor until it exits or ctx ends, feeding events to on.
func runMonitor(ctx context.Context, argv []string, on func(PowerEvent)) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	var p monitorParser
	sc := bufio.NewScanner(out)
	for sc.Scan() {
		if ev, ok := p.feed(sc.Text()); ok {
			on(ev)
		}
	}
	return cmd.Wait()
}

// delayLock holds a logind "delay" sleep inhibitor by keeping a
// systemd-inhibit child alive; killing it releases the lock.
type delayLock struct {
	log    *slog.Logger
	mu     sync.Mutex
	cmd    *exec.Cmd
	warned bool
}

func (l *delayLock) take() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cmd != nil {
		return
	}
	exe, err := exec.LookPath("systemd-inhibit")
	if err != nil {
		if !l.warned {
			l.warned = true
			l.log.Info("systemd-inhibit not found; sleep announcements are sent without delaying the suspend")
		}
		return
	}
	// A very long sleep rather than "infinity", which BusyBox does not know.
	cmd := exec.Command(exe, "--what=sleep", "--who=messh", "--why=tell paired devices this one is going to sleep",
		"--mode=delay", "sleep", "2147483647")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		l.log.Warn("cannot take a logind delay inhibitor", "error", err)
		return
	}
	l.cmd = cmd
	go func() {
		// Report a lock that died at once (e.g. polkit refused it).
		if err := cmd.Wait(); err != nil {
			l.mu.Lock()
			alive := l.cmd == cmd
			if alive {
				l.cmd = nil
			}
			l.mu.Unlock()
			if alive {
				l.log.Warn("logind delay inhibitor ended", "error", err)
			}
		}
	}()
}

func (l *delayLock) release() {
	l.mu.Lock()
	cmd := l.cmd
	l.cmd = nil
	l.mu.Unlock()
	if cmd != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
