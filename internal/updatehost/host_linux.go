//go:build linux

package updatehost

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"messh/internal/state"
)

const serviceName = "messh.service"

var serviceProperties = []string{"LoadState", "ActiveState", "SubState", "MainPID", "Type", "Restart", "RestartForceExitStatus", "ExecStart", "ExecStartPre", "ExecStartPost", "ExecStop", "ExecStopPost", "FragmentPath", "DropInPaths", "ControlGroup", "User", "Result", "ExecMainStatus", "NeedDaemonReload", "InvocationID", "NRestarts"}

func inspectService(ctx context.Context) (map[string]string, error) {
	data, err := command(ctx, "/usr/bin/systemctl", nil, "--user", "show", serviceName, "--no-pager", "--property="+strings.Join(serviceProperties, ","))
	if err != nil {
		return nil, manual("cannot inspect systemd user service: " + err.Error())
	}
	return parseProperties(string(data)), nil
}
func parseProperties(data string) map[string]string {
	result := make(map[string]string)
	for _, line := range strings.Split(data, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			result[key] = value
		}
	}
	return result
}
func sameLinuxPath(a, b string) bool {
	resolved, err := filepath.EvalSymlinks(a)
	return err == nil && resolved == b
}
func validateService(p map[string]string, exe, root, home string, pid int) error {
	if p["LoadState"] != "loaded" || p["ActiveState"] != "active" || p["SubState"] != "running" || p["MainPID"] != strconv.Itoa(pid) {
		return manual("messh.service is not the active owner of the recorded process")
	}
	if p["Type"] != "simple" && p["Type"] != "exec" {
		return manual("unsupported systemd service type")
	}
	if p["Restart"] != "no" && p["Restart"] != "on-failure" {
		return manual("systemd Restart must be no or on-failure for a graceful update")
	}
	if p["RestartForceExitStatus"] != "" || p["ExecStartPre"] != "" || p["ExecStartPost"] != "" || p["ExecStop"] != "" || p["ExecStopPost"] != "" || p["DropInPaths"] != "" || p["User"] != "" || p["NeedDaemonReload"] != "no" {
		return manual("custom systemd hooks, user overrides, drop-ins, forced restarts, or pending configuration reload are unsupported")
	}
	if !sameLinuxPath(p["FragmentPath"], filepath.Join(home, ".config", "systemd", "user", serviceName)) {
		return manual("messh.service is not the expected per-user unit")
	}
	// systemctl renders argv as a single string, not shell syntax. Compare the
	// complete expected value (including spaces inside paths); /proc below checks
	// actual argument boundaries independently.
	prefix := "{ path="
	start := p["ExecStart"]
	if !strings.HasPrefix(start, prefix) {
		return manual("unrecognized systemd ExecStart")
	}
	executable, rest, ok := strings.Cut(strings.TrimPrefix(start, prefix), " ; argv[]=")
	if !ok || !sameLinuxPath(executable, exe) {
		return manual("systemd ExecStart executable does not match this installation")
	}
	argv, metadata, ok := strings.Cut(rest, " ; ignore_errors=")
	if !ok || !strings.HasPrefix(metadata, "no ; ") || strings.Count(start, "{ path=") != 1 || (argv != executable+" node --state "+root && argv != executable+" node --state "+root+" --json") {
		return manual("systemd ExecStart is not the supported node --state invocation")
	}
	if p["ControlGroup"] == "" {
		return manual("systemd service has no process control group")
	}
	return nil
}
func linuxProcess(pid int, exe, root, group string) (string, error) {
	base := filepath.Join("/proc", strconv.Itoa(pid))
	info, err := os.Stat(base)
	if err != nil {
		return "", err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return "", manual("node process is not owned by the current user")
	}
	path, err := os.Readlink(filepath.Join(base, "exe"))
	if err != nil {
		return "", err
	}
	if path != exe {
		return "", manual("running executable does not match the update target")
	}
	cmd, err := os.ReadFile(filepath.Join(base, "cmdline"))
	if err != nil {
		return "", err
	}
	args := bytes.Split(bytes.TrimSuffix(cmd, []byte{0}), []byte{0})
	if (len(args) != 4 && len(args) != 5) || string(args[1]) != "node" || string(args[2]) != "--state" || !sameLinuxPath(string(args[3]), root) || (len(args) == 5 && string(args[4]) != "--json") {
		return "", manual("running process has an unsupported state directory or arguments")
	}
	cgroup, err := os.ReadFile(filepath.Join(base, "cgroup"))
	if err != nil {
		return "", err
	}
	found := false
	for _, line := range strings.Split(string(cgroup), "\n") {
		fields := strings.SplitN(line, ":", 3)
		if len(fields) == 3 && fields[2] == group {
			found = true
		}
	}
	if !found {
		return "", manual("node PID is outside messh.service's control group")
	}
	return linuxProcessIdentity(pid)
}
func linuxProcessIdentity(pid int) (string, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return "", err
	}
	// The comm field may contain spaces and parentheses. Starttime is field 22.
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return "", fmt.Errorf("short process stat")
	}
	return fields[19], nil
}
func discoverPlatform(ctx context.Context, exe, root string, run state.RunInfo) (*Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	p, err := inspectService(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateService(p, exe, root, home, run.PID); err != nil {
		return nil, err
	}
	identity, err := linuxProcess(run.PID, exe, root, p["ControlGroup"])
	if err != nil {
		return nil, err
	}
	configuration := serviceConfiguration(p)
	m := &Manager{kind: "systemd-user"}
	var invocation string
	var launchIssued bool
	m.validate = func(ctx context.Context) error {
		current, err := inspectService(ctx)
		if err != nil {
			return err
		}
		if err := validateService(current, exe, root, home, run.PID); err != nil {
			return err
		}
		if serviceConfiguration(current) != configuration {
			return manual("systemd configuration changed during update")
		}
		actual, err := linuxProcess(run.PID, exe, root, current["ControlGroup"])
		if err != nil {
			return err
		}
		if actual != identity {
			return manual("node process changed during update")
		}
		return nil
	}
	m.stopped = func(ctx context.Context) (bool, error) {
		actual, err := linuxProcessIdentity(run.PID)
		if err == nil && actual == identity {
			return false, nil
		}
		if err != nil && !os.IsNotExist(err) {
			return false, err
		}
		current, err := inspectService(ctx)
		if err != nil {
			return false, err
		}
		if serviceConfiguration(current) != configuration {
			return false, manual("systemd configuration changed during shutdown")
		}
		if current["ActiveState"] == "failed" || current["SubState"] == "auto-restart" || (current["MainPID"] != "0" && current["MainPID"] != strconv.Itoa(run.PID)) {
			return false, manual("systemd did not observe a successful graceful shutdown")
		}
		return current["ActiveState"] == "inactive" && current["SubState"] == "dead" && current["MainPID"] == "0" && current["Result"] == "success" && current["ExecMainStatus"] == "0", nil
	}
	m.start = func(ctx context.Context) error {
		invocation, launchIssued = "", false
		current, err := inspectService(ctx)
		if err != nil {
			return err
		}
		if serviceConfiguration(current) != configuration || current["ActiveState"] != "inactive" || current["MainPID"] != "0" {
			return manual("systemd launcher changed or is no longer stopped")
		}
		previous := current["InvocationID"]
		launchIssued = true
		_, startErr := command(ctx, "/usr/bin/systemctl", nil, "--user", "start", "--job-mode=fail", serviceName)
		// A failing start command can still have spawned a process. Keep ownership
		// even in that case; inability to inspect is NOT evidence of a safe stop.
		observe, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		current, err = inspectService(observe)
		if err == nil && serviceConfiguration(current) == configuration && current["InvocationID"] != previous {
			invocation = current["InvocationID"]
		}
		if startErr != nil {
			return startErr
		}
		if err != nil {
			return err
		}
		if invocation == "" {
			return manual("cannot establish the started systemd invocation")
		}
		return nil
	}
	m.recoverStop = func(ctx context.Context, graceful func(int) (bool, error)) error {
		current, err := inspectService(ctx)
		if err != nil {
			return err
		}
		if serviceConfiguration(current) != configuration {
			return manual("systemd configuration changed before recovery")
		}
		if !launchIssued {
			if current["ActiveState"] == "inactive" && current["MainPID"] == "0" {
				return nil
			}
			return manual("systemd launcher became active without an updater start")
		}
		for {
			observedInvocation := current["InvocationID"]
			if err := validateRecoveryInvocation(current, configuration, invocation); err != nil {
				return err
			}
			pid, err := strconv.Atoi(current["MainPID"])
			if err != nil {
				return err
			}
			var candidate string
			if pid > 0 {
				candidate, err = linuxProcess(pid, exe, root, current["ControlGroup"])
				if err != nil {
					if !os.IsNotExist(err) {
						return err
					}
					current, err = inspectService(ctx)
					if err != nil {
						return err
					}
					continue // Child failed between manager inspection and /proc read.
				}
				accepted, err := graceful(pid)
				if err != nil {
					return err
				}
				if accepted {
					for {
						actual, err := linuxProcessIdentity(pid)
						if os.IsNotExist(err) || (err == nil && actual != candidate) {
							break
						}
						if err != nil {
							return err
						}
						select {
						case <-ctx.Done():
							return ctx.Err()
						case <-time.After(100 * time.Millisecond):
						}
					}
				}
			}
			// Automatic retries remain part of this update transaction: the exact
			// unchanged launcher and startup gate protect every incarnation.
			current, err = inspectService(ctx)
			if err != nil {
				return err
			}
			if current["MainPID"] != "0" && (current["MainPID"] != strconv.Itoa(pid) || current["InvocationID"] != observedInvocation) {
				continue // Inspect and gracefully stop a newly restarted child too.
			}
			if err := validateRecoveryInvocation(current, configuration, invocation); err != nil {
				return err
			}
			if err := m.checkGate(); err != nil {
				return err
			}
			// Stop cancels systemd's restart job as well as the verified gated
			// invocation. No process-name kill or persistent unit change is used.
			if _, err := command(ctx, "/usr/bin/systemctl", nil, "--user", "stop", "--job-mode=replace", serviceName); err != nil {
				return err
			}
			current, err = inspectService(ctx)
			if err != nil {
				return err
			}
			if serviceConfiguration(current) == configuration && (current["ActiveState"] == "failed" || current["ActiveState"] == "inactive") && current["MainPID"] == "0" {
				// Clear this stopped unit's failure/start-limit state so the restored
				// executable is not rejected because the candidate exhausted retries.
				if _, err := command(ctx, "/usr/bin/systemctl", nil, "--user", "reset-failed", serviceName); err != nil {
					return err
				}
				current, err = inspectService(ctx)
				if err != nil {
					return err
				}
			}
			if serviceConfiguration(current) != configuration || current["ActiveState"] != "inactive" || current["MainPID"] != "0" || current["SubState"] != "dead" {
				return manual("systemd candidate has not become inactive")
			}
			if candidate != "" {
				actual, err := linuxProcessIdentity(pid)
				if err == nil && actual == candidate {
					return manual("candidate remains alive after manager stop")
				}
				if err != nil && !os.IsNotExist(err) {
					return err
				}
			}
			return nil
		}
	}
	return m, nil
}
func serviceConfiguration(p map[string]string) string {
	var b strings.Builder
	for _, key := range serviceProperties {
		switch key {
		case "ActiveState", "SubState", "MainPID", "Result", "ExecMainStatus", "ControlGroup", "InvocationID", "NRestarts":
			continue
		}
		value := p[key]
		if key == "ExecStart" {
			value, _, _ = strings.Cut(value, " ; start_time=")
		}
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(value)
		b.WriteByte('\n')
	}
	return b.String()
}

func validateRecoveryInvocation(current map[string]string, configuration, invocation string) error {
	if serviceConfiguration(current) != configuration {
		return manual("systemd configuration changed during candidate recovery")
	}
	if current["MainPID"] == "0" && (current["ActiveState"] == "inactive" || current["ActiveState"] == "failed" || current["SubState"] == "auto-restart") {
		return nil // A failed child may leave only the manager's restart timer.
	}
	if current["InvocationID"] == "" {
		return manual("systemd has no invocation identity for its candidate process")
	}
	if invocation != "" && current["InvocationID"] != invocation {
		restarts, err := strconv.ParseUint(current["NRestarts"], 10, 64)
		if err != nil || restarts == 0 {
			return manual("systemd invocation changed without an observed automatic restart")
		}
	}
	return nil
}

// RecoverInterrupted has no launcher settings to restore on Linux.
func RecoverInterrupted(context.Context, state.Paths) error { return nil }
