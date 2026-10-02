package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"messh/internal/buildinfo"
	"messh/internal/control"
	"messh/internal/state"
	"messh/internal/update"
	"messh/internal/updatehost"
)

func init() {
	add(&Command{
		Name:     "update",
		Summary:  "check for or explicitly install the official main-channel release",
		Help:     "--check is read-only. Installation retains the previous executable and restarts only a recognized existing per-user launcher. Busy nodes must finish their work first; no approvals, firewall rules, or launcher settings are changed.",
		Flags:    func(fs *flag.FlagSet) { fs.Bool("check", false, "only check for an available release; change nothing") },
		Mutates:  true,
		Waits:    "bounded release download, then up to 40 seconds for transition and 55 seconds for recovery",
		Output:   "check: {current,available,update_available,reason}; install: {current,previous,new,updated,restarted,executable,backup,reason}",
		Examples: []string{"messh update --check --json", "messh update --json"},
		Run:      runUpdate,
	})
}

type updateResult struct {
	Current    buildinfo.Info `json:"current"`
	Previous   buildinfo.Info `json:"previous"`
	New        buildinfo.Info `json:"new"`
	Updated    bool           `json:"updated"`
	Restarted  bool           `json:"restarted"`
	Executable string         `json:"executable"`
	Backup     string         `json:"backup,omitempty"`
	Reason     string         `json:"reason"`
}

type updateManager interface {
	Stop(context.Context, string) error
	Start(context.Context) error
	RecoverStop(context.Context, string) error
}

// These private seams let fixtures use a local release server and an isolated
// launcher. The CLI never accepts an alternate publication source or launcher.
type updateDependencies struct {
	client   *update.Client
	discover func(context.Context, string, state.Paths, state.RunInfo) (updateManager, error)
}

func productionUpdateDependencies() updateDependencies {
	return updateDependencies{
		client: update.NewClient(),
		discover: func(ctx context.Context, exe string, paths state.Paths, run state.RunInfo) (updateManager, error) {
			return updatehost.Discover(ctx, exe, paths, run)
		},
	}
}

func runUpdate(c *Context) error {
	return runUpdateWithDependencies(c, productionUpdateDependencies())
}

func runUpdateWithDependencies(c *Context, deps updateDependencies) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	current := buildinfo.Current()
	if c.Bool("check") {
		// Deliberately before resolving state or the executable: no token, lock,
		// staging, launcher recovery, or even state-directory creation is needed.
		result, err := deps.client.Check(ctx, current, runtime.GOOS, runtime.GOARCH)
		if err != nil {
			return err
		}
		// Keep available explicit (null when absent) in the command contract.
		output := struct {
			Current         buildinfo.Info  `json:"current"`
			Available       *update.Release `json:"available"`
			UpdateAvailable bool            `json:"update_available"`
			Reason          string          `json:"reason"`
		}{result.Current, result.Available, result.UpdateAvailable, result.Reason}
		return c.Emit(output, func(w io.Writer) {
			if result.UpdateAvailable {
				fmt.Fprintf(w, "%s -> %s available; run messh update to install\n", current.Version, result.Available.Manifest.Version)
			} else {
				fmt.Fprintf(w, "%s: %s\n", current.Version, result.Reason)
			}
		})
	}
	paths, err := c.Paths()
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	result, err := installUpdate(ctx, paths, executable, current, deps)
	if err != nil {
		return err
	}
	return c.Emit(result, func(w io.Writer) {
		if result.Updated {
			fmt.Fprintf(w, "Updated %s -> %s at %s (restarted: %t; previous executable: %s)\n", result.Previous.Version, result.New.Version, result.Executable, result.Restarted, result.Backup)
		} else {
			fmt.Fprintf(w, "%s: %s\n", result.Current.Version, result.Reason)
		}
	})
}

// installUpdate is also the smoke entry point: supply production dependencies
// with only client replaced by a local HTTP fixture to exercise real launchers.
func installUpdate(ctx context.Context, paths state.Paths, executable string, current buildinfo.Info, deps updateDependencies) (result updateResult, err error) {
	result = updateResult{Current: current, Previous: current, New: current, Executable: executable}
	unlock, err := update.AcquireLock(executable)
	if err != nil {
		if errors.Is(err, update.ErrLocked) {
			return result, withHint(err, "Wait for the other updater to finish; do not delete its lock file.")
		}
		return result, withHint(err, "Use the installed regular executable at its original path and ensure its directory is writable; symlink installations cannot be updated in place.")
	}
	defer unlock()
	// Explicit installs repair interrupted launcher bookkeeping even when the
	// selected build turns out to be current. Checks never enter this function.
	recoveryCtx, recoveryCancel := context.WithTimeout(ctx, 15*time.Second)
	err = updatehost.RecoverInterrupted(recoveryCtx, paths)
	recoveryCancel()
	if err != nil {
		return result, err
	}
	check, err := deps.client.Check(ctx, current, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return result, err
	}
	result.Reason = check.Reason
	if !check.UpdateAvailable {
		return result, nil
	}
	if check.Available == nil {
		return result, errors.New("update check did not return the selected release")
	}
	wanted := manifestBuild(check.Available.Manifest)
	probeCtx, probeCancel := context.WithTimeout(ctx, 10*time.Second)
	snapshot, err := readUpdateNode(probeCtx, paths)
	probeCancel()
	if err != nil {
		return result, withHint(err, "Check messh status and the original launcher; do not update while a recorded node may still be running.")
	}
	var manager updateManager
	if snapshot != nil {
		if !sameUpdatePath(snapshot.run.Executable, executable) || snapshot.status.Build != current {
			return result, withHint(errors.New("installed CLI and running node executable or build do not match"), "Invoke the running node's installed executable, or stop it manually using its original launcher before updating.")
		}
		preflightCtx, preflightCancel := context.WithTimeout(ctx, 15*time.Second)
		manager, err = deps.discover(preflightCtx, executable, paths, snapshot.run)
		preflightCancel()
		if err != nil {
			return result, withHint(err, "Stop the node manually using its original launcher, run messh update while it is stopped, then restart that launcher. Do not force-stop active work.")
		}
	}
	staged, err := deps.client.DownloadStage(ctx, *check.Available, executable)
	if err != nil {
		return result, err
	}
	// A failed recovery can need the candidate as evidence. Do not erase its
	// staging directory unless no replacement happened or recovery completed.
	cleanStage := true
	defer func() {
		if cleanStage {
			err = errors.Join(err, staged.Close())
		}
	}()
	if err := verifyUpdateExecutable(ctx, staged.Path, wanted); err != nil {
		return result, err
	}
	if snapshot == nil {
		// Recheck after the download; a node started meanwhile must not be swapped.
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		live, probeErr := readUpdateNode(probeCtx, paths)
		cancel()
		if probeErr != nil {
			return result, probeErr
		}
		if live != nil {
			return result, errors.New("a node started during the update; retry with its launcher available")
		}
		cleanStage = false
		replacement, swapErr := update.Swap(executable, staged.Path)
		if swapErr != nil {
			proofCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			proofErr := verifyUpdateExecutable(proofCtx, executable, current)
			cancel()
			if proofErr != nil {
				return result, updateRecoveryError(swapErr, proofErr, executable)
			}
			cleanStage = true
			return result, swapErr
		}
		if err := replacement.Commit(); err != nil {
			return result, err
		}
		cleanStage = true
		result.Current, result.New = wanted, wanted
		result.Updated, result.Backup, result.Reason = true, replacement.Backup, "updated"
		return result, nil
	}
	prepareCtx, prepareCancel := context.WithTimeout(ctx, 5*time.Second)
	gate, err := snapshot.client.PrepareUpdate(prepareCtx)
	prepareCancel()
	if err != nil {
		return result, withHint(err, "Let active requests, jobs (for every owner), transfers, and approvals finish, then retry; older nodes without maintenance support must be stopped manually.")
	}
	abortOriginal := func(cause error) error {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		return errors.Join(cause, snapshot.client.AbortUpdate(cleanupCtx, gate.Lease))
	}
	if err := gate.Validate(snapshot.run.ID, snapshot.run.Executable, time.Now()); err != nil {
		return result, abortOriginal(err)
	}
	if time.Until(gate.Expires) < 115*time.Second {
		return result, abortOriginal(errors.New("maintenance lease has insufficient time for update and recovery"))
	}
	if err := paths.SaveUpdateStartup(gate, time.Now()); err != nil {
		return result, abortOriginal(err)
	}
	if time.Until(gate.Expires) < 110*time.Second {
		return result, abortOriginal(errors.New("persisting the maintenance gate left insufficient recovery time"))
	}
	forwardCtx, forwardCancel := context.WithTimeout(ctx, 40*time.Second)
	defer forwardCancel()
	stopCtx, stopCancel := context.WithTimeout(forwardCtx, 18*time.Second)
	err = manager.Stop(stopCtx, gate.Lease)
	stopCancel()
	if err != nil {
		// Only release an early failed stop when the exact old listener/process
		// is still proven. An ambiguous shutdown NEVER permits a binary swap.
		proofCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		old, proofErr := readUpdateNode(proofCtx, paths)
		if proofErr == nil && old != nil && old.run == snapshot.run && old.status.Build == snapshot.status.Build {
			_, proofErr = deps.discover(proofCtx, executable, paths, old.run)
			if proofErr == nil {
				err = abortOriginal(err)
			}
		} else if proofErr == nil {
			proofErr = errors.New("original live node could not be proven after the failed stop")
		}
		err = errors.Join(err, proofErr)
		cancel()
		return result, withHint(err, "The executable was not replaced. Inspect the original launcher and messh status; if shutdown was incomplete, retain update-startup.json until the node is safely recovered.")
	}
	// Swap itself can fail to restore its first rename. Retain staging until
	// the original node is proven healthy again, even when Swap returns no handle.
	cleanStage = false
	replacement, swapErr := update.Swap(executable, staged.Path)
	attempted := false
	if swapErr == nil {
		attempted = true
		startCtx, cancel := context.WithTimeout(forwardCtx, 12*time.Second)
		err = manager.Start(startCtx)
		cancel()
		if err == nil {
			healthCtx, cancel := context.WithTimeout(forwardCtx, 7*time.Second)
			var candidate *updateNode
			candidate, err = waitUpdateHealth(healthCtx, paths, executable, wanted, snapshot, deps)
			cancel()
			if err == nil {
				admitCtx, cancel := context.WithTimeout(forwardCtx, 3*time.Second)
				err = candidate.client.AbortUpdate(admitCtx, gate.Lease)
				cancel()
			}
		}
	} else {
		err = swapErr
	}
	if err == nil {
		if err = replacement.Commit(); err != nil {
			return result, err
		}
		cleanStage = true
		result.Current, result.New = wanted, wanted
		result.Updated, result.Restarted, result.Backup, result.Reason = true, true, replacement.Backup, "updated"
		return result, nil
	}
	originalErr := err
	// Recovery is independent of caller cancellation but remains bounded by the
	// same nonrenewable lease. No unchecked candidate ever has admission opened.
	recoverCtx, recoverCancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer recoverCancel()
	if attempted {
		stopCtx, cancel := context.WithTimeout(recoverCtx, 15*time.Second)
		err = manager.RecoverStop(stopCtx, gate.Lease)
		cancel()
		if err != nil {
			return result, updateRecoveryError(originalErr, err, executable)
		}
		if rollbackErr := replacement.Rollback(); rollbackErr != nil {
			// Rollback can restore the original successfully but then fail to
			// remove the stopped candidate. Do not leave a recoverable old node
			// down merely because that cleanup failed; prove its image first.
			if proofErr := verifyUpdateExecutable(recoverCtx, executable, current); proofErr != nil {
				return result, updateRecoveryError(originalErr, errors.Join(rollbackErr, proofErr), executable)
			}
			originalErr = errors.Join(originalErr, fmt.Errorf("rollback cleanup failed: %w", rollbackErr))
		}
	} else {
		// Swap can fail while restoring its first rename. Prove the installed
		// image is still the original before asking its launcher to run it.
		if err = verifyUpdateExecutable(recoverCtx, executable, current); err != nil {
			return result, updateRecoveryError(originalErr, err, executable)
		}
	}
	startCtx, startCancel := context.WithTimeout(recoverCtx, 15*time.Second)
	err = manager.Start(startCtx)
	startCancel()
	if err != nil {
		return result, updateRecoveryError(originalErr, err, executable)
	}
	healthCtx, healthCancel := context.WithTimeout(recoverCtx, 10*time.Second)
	restored, err := waitUpdateHealth(healthCtx, paths, executable, current, snapshot, deps)
	healthCancel()
	if err != nil {
		return result, updateRecoveryError(originalErr, err, executable)
	}
	admitCtx, admitCancel := context.WithTimeout(recoverCtx, 3*time.Second)
	err = restored.client.AbortUpdate(admitCtx, gate.Lease)
	admitCancel()
	if err != nil {
		return result, updateRecoveryError(originalErr, err, executable)
	}
	cleanStage = true
	return result, fmt.Errorf("update failed; previous executable and healthy node restored: %w", originalErr)
}

func updateRecoveryError(cause, recovery error, executable string) error {
	return withHint(errors.Join(cause, fmt.Errorf("automatic recovery incomplete: %w", recovery)),
		fmt.Sprintf("Do not replace or stop an unverified process. Inspect the original launcher and messh status; retain %s.previous and update-startup.json (and update-host-recovery.json if present) for manual recovery.", executable))
}

func manifestBuild(m update.Manifest) buildinfo.Info {
	return buildinfo.Info{Version: m.Version, Commit: m.Commit, Channel: m.Channel, Build: m.Build}
}

func sameUpdatePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	a, err := filepath.Abs(a)
	if err != nil {
		return false
	}
	b, err = filepath.Abs(b)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

type updateNode struct {
	run    state.RunInfo
	status control.Status
	client *control.Client
}

// Unlike control.Dial this never creates a token. A stale run file is not proof
// of an absent process: startup, hangs, and occupied ports must fail closed.
func readUpdateNode(ctx context.Context, paths state.Paths) (*updateNode, error) {
	run, err := paths.LoadRunInfo()
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if run.PID <= 0 || run.ID == "" || run.Started.IsZero() || run.Executable == "" {
		return nil, errors.New("node run record lacks process identity; stop the old node manually before updating")
	}
	host, _, err := net.SplitHostPort(run.Local)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return nil, errors.New("node control address is not an IP loopback address")
	}
	token, err := os.ReadFile(paths.ControlTokenFile())
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(string(token)) == "" {
		return nil, errors.New("empty control token")
	}
	client := &control.Client{Base: "http://" + run.Local, Token: strings.TrimSpace(string(token)), HTTP: &http.Client{Timeout: 3 * time.Second}}
	var status control.Status
	if err := client.Do(ctx, http.MethodGet, "/v1/status", nil, &status); err != nil {
		return nil, err
	}
	if status.ID != run.ID || !status.Started.Equal(run.Started) || status.Local != run.Local {
		return nil, errors.New("node status does not match its recorded process identity")
	}
	return &updateNode{run: run, status: status, client: client}, nil
}

func waitUpdateHealth(ctx context.Context, paths state.Paths, executable string, wanted buildinfo.Info, old *updateNode, deps updateDependencies) (*updateNode, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var last error
	for {
		candidate, err := readUpdateNode(ctx, paths)
		if err == nil && candidate != nil {
			if candidate.run.ID != old.run.ID || candidate.status.Build != wanted || !sameUpdatePath(candidate.run.Executable, executable) || !candidate.run.Started.After(old.run.Started) || candidate.run.PID == old.run.PID {
				err = errors.New("restarted node identity, build, PID, executable, or start time does not match the update")
			} else {
				_, err = deps.discover(ctx, executable, paths, candidate.run)
				if err == nil {
					return candidate, nil
				}
			}
		}
		if err != nil {
			last = err
		} else {
			last = errors.New("restarted node has not published its run record")
		}
		select {
		case <-ctx.Done():
			return nil, errors.Join(fmt.Errorf("restarted node did not become healthy: %w", ctx.Err()), last)
		case <-ticker.C:
		}
	}
}

type updateOutput struct {
	data     []byte
	overflow bool
}

func (b *updateOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := (64 << 10) - len(b.data)
	if len(p) > remaining {
		p = p[:remaining]
		b.overflow = true
	}
	b.data = append(b.data, p...)
	return n, nil
}

func verifyUpdateExecutable(ctx context.Context, executable string, wanted buildinfo.Info) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "version", "--json")
	cmd.WaitDelay = time.Second
	var stdout, stderr updateOutput
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("verify staged executable identity: %w: %s", err, strings.TrimSpace(string(stderr.data)))
	}
	if stdout.overflow || stderr.overflow {
		return errors.New("staged executable identity output exceeded 64 KiB")
	}
	var actual buildinfo.Info
	decoder := json.NewDecoder(bytes.NewReader(stdout.data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&actual); err != nil {
		return fmt.Errorf("invalid staged executable identity: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("staged executable returned trailing identity output")
	}
	if actual != wanted {
		return fmt.Errorf("staged executable identity does not match release manifest: got %+v; want %+v", actual, wanted)
	}
	return nil
}
