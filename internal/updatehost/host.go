// Package updatehost controls only the existing per-user messh launchers.
// It never changes launcher configuration or terminates a process directly.
package updatehost

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"messh/internal/control"
	"messh/internal/state"
)

const operationTimeout = 45 * time.Second

// Manager retains launcher provenance across Start, including ambiguous errors.
// Stop requires a live maintenance lease and successful graceful node exit.
// Start and RecoverStop require the same persisted, unexpired startup gate.
// Managers are not safe for concurrent method calls.
type Manager struct {
	kind        string
	client      *control.Client
	validate    func(context.Context) error
	stopped     func(context.Context) (bool, error)
	start       func(context.Context) error
	didStop     bool
	attempted   bool
	paths       state.Paths
	executable  string
	id          string
	gate        *state.UpdateStartup
	recoverStop func(context.Context, func(int) (bool, error)) error
}

// Discover recognizes the current user's exact existing messh launcher.
// Unmanaged nodes must be stopped manually; no fallback process killing or
// detached launching is performed. Callers must not call this for stopped nodes.
func Discover(ctx context.Context, executable string, paths state.Paths, run state.RunInfo) (*Manager, error) {
	if run.PID <= 0 {
		return nil, manual("the state file has no running process")
	}
	exe, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, fmt.Errorf("resolve executable: %w", err)
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return nil, err
	}
	root, err := filepath.EvalSymlinks(paths.Root)
	if err != nil {
		return nil, fmt.Errorf("resolve state directory: %w", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(run.Local)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return nil, manual("the node control address is not an IP loopback address")
	}
	// Read only: discovery must not create or replace state, including the token.
	token, err := os.ReadFile(paths.ControlTokenFile())
	if err != nil {
		return nil, fmt.Errorf("read control token: %w", err)
	}
	if strings.TrimSpace(string(token)) == "" {
		return nil, errors.New("empty control token")
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	m, err := discoverPlatform(ctx, exe, root, run)
	if err != nil {
		return nil, err
	}
	m.client = &control.Client{Base: "http://" + run.Local, Token: strings.TrimSpace(string(token)), HTTP: &http.Client{Timeout: 10 * time.Second}}
	m.paths, m.executable, m.id = state.Paths{Root: root}, exe, run.ID
	return m, nil
}

func (m *Manager) Kind() string { return m.kind }

func (m *Manager) Stop(ctx context.Context, lease string) error {
	if strings.TrimSpace(lease) == "" {
		return errors.New("a valid maintenance lease is required to stop the node")
	}
	if m.didStop {
		return errors.New("this launcher has already been stopped")
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if err := m.validate(ctx); err != nil {
		return err
	}
	if err := m.client.StopUpdate(ctx, lease); err != nil {
		return err
	}
	ticker := time.NewTicker(150 * time.Millisecond)
	defer ticker.Stop()
	for {
		done, err := m.stopped(ctx)
		if err != nil {
			return fmt.Errorf("await graceful launcher shutdown (do not replace the executable): %w", err)
		}
		if done {
			m.didStop = true
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("await graceful launcher shutdown (do not replace the executable): %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// Start starts only the validated launcher under a persisted startup gate.
// Both success and error consume the stopped state: callers must RecoverStop
// before replacing the binary or trying Start again.
func (m *Manager) Start(ctx context.Context) error {
	if !m.didStop || m.attempted {
		return errors.New("launcher restart requires a completed Stop or RecoverStop")
	}
	gate, err := m.paths.LoadUpdateStartup()
	if err != nil {
		return err
	}
	if gate == nil {
		return errors.New("launcher restart requires a persisted startup gate")
	}
	if err := gate.Validate(m.id, m.executable, time.Now()); err != nil {
		return err
	}
	if time.Until(gate.Expires) <= operationTimeout {
		return errors.New("startup gate has insufficient time remaining for a bounded launcher operation")
	}
	m.gate = gate
	m.attempted = true
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	return m.start(ctx)
}

// RecoverStop stops only the candidate owned by the last Start attempt. The
// startup lease must still be persisted and valid. Reachable candidates receive
// a graceful control stop; manager-specific recovery is allowed only while the
// gate guarantees that the candidate cannot accept work. An error leaves the
// attempt owned but NOT safe to replace. Success permits binary rollback and
// Start of the restored executable under the same gate.
func (m *Manager) RecoverStop(ctx context.Context, lease string) error {
	if !m.didStop || !m.attempted || m.gate == nil || lease != m.gate.Lease {
		return errors.New("recovery requires the startup lease of an owned Start attempt")
	}
	release, err := m.paths.HoldUpdateStartup(*m.gate)
	if err != nil {
		return err
	}
	defer release()
	if err := m.checkGate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	if err := m.recoverStop(ctx, func(pid int) (bool, error) {
		if err := m.checkGate(); err != nil {
			return false, err
		}
		run, err := m.paths.LoadRunInfo()
		if err != nil || run.PID != pid {
			return false, nil // No control endpoint for this proven gated process.
		}
		if run.ID != m.id {
			return false, errors.New("candidate control identity changed")
		}
		host, _, err := net.SplitHostPort(run.Local)
		if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
			return false, errors.New("candidate control endpoint is not loopback")
		}
		client := *m.client
		client.Base = "http://" + run.Local
		if err := client.StopUpdate(ctx, lease); err != nil {
			var network *net.OpError
			if errors.As(err, &network) && network.Op == "dial" {
				return false, nil // No server accepted the request; startup gate still holds.
			}
			return false, fmt.Errorf("candidate refused graceful recovery: %w", err)
		}
		return true, nil
	}); err != nil {
		return fmt.Errorf("candidate recovery incomplete (do not replace the executable): %w", err)
	}
	m.attempted = false
	return nil
}

func (m *Manager) checkGate() error {
	current, err := m.paths.LoadUpdateStartup()
	if err != nil {
		return err
	}
	if current == nil || m.gate == nil || *current != *m.gate {
		return errors.New("startup gate was removed or changed; recovery cannot stop this node")
	}
	if err := current.Validate(m.id, m.executable, time.Now()); err != nil {
		return err
	}
	if time.Until(current.Expires) <= operationTimeout {
		return errors.New("startup gate expires before recovery can safely finish")
	}
	return nil
}

func manual(reason string) error {
	return fmt.Errorf("automatic update cannot manage this node: %s; stop messh manually using its original launcher, run the update while stopped, then restart that launcher", reason)
}

// Subprocess output and duration are both bounded. No shell receives interpolated
// user data. The Windows platform uses a fixed PowerShell program plus env values.
type boundedOutput struct {
	data     []byte
	overflow bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := (1 << 20) - len(b.data)
	if len(p) > remaining {
		p = p[:remaining]
		b.overflow = true
	}
	b.data = append(b.data, p...)
	return n, nil
}
func command(ctx context.Context, executable string, env []string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.WaitDelay = time.Second
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var stdout, stderr boundedOutput
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.data, fmt.Errorf("%s: %w: %s", filepath.Base(executable), err, strings.TrimSpace(string(stderr.data)))
	}
	if stdout.overflow || stderr.overflow {
		return nil, errors.New("launcher inspection output exceeded 1 MiB")
	}
	return stdout.data, nil
}
