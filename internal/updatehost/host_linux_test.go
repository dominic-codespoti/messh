//go:build linux

package updatehost

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestSystemdRequiresExactProcessAndSafeRestart(t *testing.T) {
	home := t.TempDir()
	exe := filepath.Join(home, "bin with spaces", "messh")
	root := filepath.Join(home, "state with spaces")
	unit := filepath.Join(home, ".config", "systemd", "user", serviceName)
	for _, dir := range []string{filepath.Dir(exe), root, filepath.Dir(unit)} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, file := range []string{exe, unit} {
		if err := os.WriteFile(file, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	const pid = 123
	original := map[string]string{
		"LoadState": "loaded", "ActiveState": "active", "SubState": "running", "MainPID": strconv.Itoa(pid), "Type": "simple", "Restart": "on-failure", "FragmentPath": unit, "ControlGroup": "/user.slice/messh.service", "NeedDaemonReload": "no",
		"ExecStart": "{ path=" + exe + " ; argv[]=" + exe + " node --state " + root + " ; ignore_errors=no ; start_time=[now] ; pid=123 ; code=(null) ; status=0/0 }",
	}
	if err := validateService(original, exe, root, home, pid); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, key, value string }{
		{"restart-after-success", "Restart", "always"},
		{"restart-only-success", "Restart", "on-success"},
		{"forced-restart", "RestartForceExitStatus", "0"},
		{"wrong-process", "MainPID", "999"},
		{"extra-stop-hook", "ExecStopPost", "/bin/do-something"},
		{"drop-in", "DropInPaths", "/tmp/custom.conf"},
		{"pending-config", "NeedDaemonReload", "yes"},
		{"wrong-user", "User", "root"},
		{"unrelated-unit", "FragmentPath", exe},
		{"different-command", "ExecStart", "{ path=" + exe + " ; argv[]=" + exe + " node --state /tmp/other ; ignore_errors=no ; start_time=[now] }"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := make(map[string]string, len(original))
			for k, v := range original {
				changed[k] = v
			}
			changed[tc.key] = tc.value
			if err := validateService(changed, exe, root, home, pid); err == nil {
				t.Fatal("accepted unsafe or unrelated service")
			}
		})
	}
}

func TestRecoveryTracksFailedAndRestartedInvocation(t *testing.T) {
	original := map[string]string{
		"LoadState": "loaded", "Type": "simple", "Restart": "on-failure",
		"ExecStart":        "{ path=/installed/messh ; argv[]=/installed/messh node --state /state ; ignore_errors=no ; start_time=old }",
		"NeedDaemonReload": "no", "MainPID": "123", "ActiveState": "active",
		"SubState": "running", "InvocationID": "original", "NRestarts": "0",
	}
	configuration := serviceConfiguration(original)
	for _, tc := range []struct {
		name    string
		changes map[string]string
		refuse  bool
	}{
		{"same-candidate", nil, false},
		{"failed-child", map[string]string{"MainPID": "0", "ActiveState": "failed", "SubState": "failed", "InvocationID": ""}, false},
		{"pending-restart", map[string]string{"MainPID": "0", "ActiveState": "activating", "SubState": "auto-restart"}, false},
		{"automatic-restart", map[string]string{"MainPID": "456", "InvocationID": "retry", "NRestarts": "1"}, false},
		{"foreign-start", map[string]string{"MainPID": "456", "InvocationID": "foreign"}, true},
		{"unknown-process", map[string]string{"InvocationID": ""}, true},
		{"changed-restart-policy", map[string]string{"Restart": "always"}, true},
		{"changed-executable", map[string]string{"ExecStart": "/other/node"}, true},
		{"changed-unit-on-disk", map[string]string{"NeedDaemonReload": "yes"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := make(map[string]string, len(original))
			for key, value := range original {
				current[key] = value
			}
			for key, value := range tc.changes {
				current[key] = value
			}
			err := validateRecoveryInvocation(current, configuration, "original")
			if (err != nil) != tc.refuse {
				t.Fatalf("recovery error = %v; refuse = %v", err, tc.refuse)
			}
		})
	}
}
