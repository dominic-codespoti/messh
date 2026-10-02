//go:build windows

package updatehost

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"messh/internal/state"
)

func TestTaskProcessAncestryAndInteractiveOwnership(t *testing.T) {
	const exe = `C:\Users\A B\messh.exe`
	const root = `C:\Users\A B\state`
	const ps = `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
	const launcher = root + `\start-node.ps1`
	fixture := taskSnapshot{
		XML: taskFixture(), State: "Running", SID: "S-1-5-21-123", Session: 2, Engines: []int{200},
		Processes: []windowsProcess{
			{PID: 100, Parent: 200, Executable: exe, CommandLine: `"` + exe + `" node --state "` + root + `" --json`, Created: "node-instance", SID: "S-1-5-21-123", Session: 2},
			{PID: 200, Parent: 300, Executable: ps, CommandLine: `"` + ps + `" -NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -WindowStyle Hidden -File "` + launcher + `"`, Created: "wrapper-instance", SID: "S-1-5-21-123", Session: 2},
		},
	}
	if err := validateWindowsInstance(fixture, exe, root, ps, launcher, 100); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*taskSnapshot){
		"unrelated-task-instance": func(s *taskSnapshot) { s.Engines = []int{999} },
		"other-user-node":         func(s *taskSnapshot) { s.Processes[0].SID = "other-user" },
		"other-user-wrapper":      func(s *taskSnapshot) { s.Processes[1].SID = "other-user" },
		"other-session":           func(s *taskSnapshot) { s.Processes[0].Session = 3 },
		"other-executable":        func(s *taskSnapshot) { s.Processes[0].Executable = `C:\other.exe` },
		"unrelated-pid":           func(s *taskSnapshot) { s.Processes[0].PID = 999 },
		"wrong-parent":            func(s *taskSnapshot) { s.Processes[0].Parent = 999 },
		"extra-node-flag":         func(s *taskSnapshot) { s.Processes[0].CommandLine += " --unrecognized" },
		"extra-shell-command":     func(s *taskSnapshot) { s.Processes[1].CommandLine += " -Command other" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			changed := fixture
			changed.Processes = append([]windowsProcess(nil), fixture.Processes...)
			mutate(&changed)
			if err := validateWindowsInstance(changed, exe, root, ps, launcher, 100); err == nil {
				t.Fatal("accepted unrelated or unsafe process instance")
			}
		})
	}
}

func TestStartAttemptRetainsRecoveryOwnership(t *testing.T) {
	for _, failed := range []bool{false, true} {
		name := "successful-start"
		if failed {
			name = "ambiguous-start-error"
		}
		t.Run(name, func(t *testing.T) {
			paths := state.Paths{Root: t.TempDir()}
			exe := filepath.Join(paths.Root, "messh.exe")
			gate := state.UpdateStartup{Lease: strings.Repeat("ab", 32), Expires: time.Now().Add(110 * time.Second), ID: "device", Executable: exe}
			if err := paths.SaveUpdateStartup(gate, time.Now()); err != nil {
				t.Fatal(err)
			}
			starts := 0
			m := &Manager{didStop: true, paths: paths, executable: exe, id: gate.ID}
			m.start = func(context.Context) error {
				starts++
				if failed {
					return errors.New("launch reply was lost after spawning")
				}
				return nil
			}
			m.recoverStop = func(context.Context, func(int) (bool, error)) error {
				if err := paths.RemoveUpdateStartup(gate); err == nil {
					t.Fatal("concurrent abort reopened admissions during manager recovery")
				}
				return nil
			}
			err := m.Start(context.Background())
			if (err != nil) != failed {
				t.Fatalf("Start error = %v", err)
			}
			if err := m.Start(context.Background()); err == nil {
				t.Fatal("allowed a second launch before recovery")
			}
			if starts != 1 {
				t.Fatalf("launched %d candidates before recovery", starts)
			}
			if err := m.RecoverStop(context.Background(), "foreign-lease"); err == nil {
				t.Fatal("recovered using a foreign lease")
			}
			if err := m.RecoverStop(context.Background(), gate.Lease); err != nil {
				t.Fatal(err)
			}
			_ = m.Start(context.Background())
			if starts != 2 {
				t.Fatal("could not restart restored executable after recovery")
			}
		})
	}
}

func TestRecoveryRefusesReleasedStartupGate(t *testing.T) {
	paths := state.Paths{Root: t.TempDir()}
	exe := filepath.Join(paths.Root, "messh.exe")
	gate := state.UpdateStartup{Lease: strings.Repeat("cd", 32), Expires: time.Now().Add(110 * time.Second), ID: "device", Executable: exe}
	if err := paths.SaveUpdateStartup(gate, time.Now()); err != nil {
		t.Fatal(err)
	}
	m := &Manager{didStop: true, paths: paths, executable: exe, id: gate.ID}
	m.start = func(context.Context) error { return nil }
	m.recoverStop = func(context.Context, func(int) (bool, error)) error {
		t.Fatal("stopped an unprotected candidate")
		return nil
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := paths.RemoveUpdateStartup(gate); err != nil {
		t.Fatal(err)
	}
	if err := m.RecoverStop(context.Background(), gate.Lease); err == nil {
		t.Fatal("recovered a candidate after admission resumed")
	}
}

func TestTaskRecoveryRequiresOwnedInstance(t *testing.T) {
	const owned = "{12345678-1234-1234-1234-123456789abc}"
	const foreign = "{abcdef01-1234-1234-1234-123456789abc}"
	for _, tc := range []struct {
		name     string
		snapshot taskSnapshot
		instance string
		refuse   bool
	}{
		{"live-owned", taskSnapshot{State: "Running", Engines: []int{200}, Instances: []string{owned}}, owned, false},
		{"failed-child-pending-retry", taskSnapshot{State: "Ready", Result: 1}, owned, false},
		{"foreign-instance", taskSnapshot{State: "Running", Engines: []int{200}, Instances: []string{foreign}}, owned, true},
		{"ambiguous-start-error", taskSnapshot{State: "Ready"}, "", true},
		{"engine-without-instance", taskSnapshot{State: "Running", Engines: []int{200}}, owned, true},
		{"multiple-instances", taskSnapshot{State: "Running", Engines: []int{200, 201}, Instances: []string{owned, foreign}}, owned, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateTaskRecoveryInstance(tc.snapshot, tc.instance)
			if (err != nil) != tc.refuse {
				t.Fatalf("recovery error = %v; refuse = %v", err, tc.refuse)
			}
		})
	}
}

func TestRecoveryIntentCreationIsPrivateAndExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	powershell := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	const payload = `{"instance":"owned","enabled":true}`
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	script := `$ErrorActionPreference = 'Stop'
$sid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User
` + createTaskRecoveryIntentScript + `
$stored = [System.IO.File]::GetAccessControl($env:MESSH_UPDATE_INTENT_PATH)
$rules = @($stored.GetAccessRules($true, $true, [System.Security.Principal.SecurityIdentifier]))
@{protected=$stored.AreAccessRulesProtected;owner=$stored.GetOwner([System.Security.Principal.SecurityIdentifier]).Value;sid=$sid.Value;rules=@($rules | ForEach-Object { @{sid=$_.IdentityReference.Value;rights=[int]$_.FileSystemRights;allow=($_.AccessControlType -eq 'Allow')} })} | ConvertTo-Json -Depth 4 -Compress
`
	env := []string{"MESSH_UPDATE_INTENT_PATH=" + path, "MESSH_UPDATE_INTENT=" + payload}
	data, err := command(ctx, powershell, env, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script)
	if err != nil {
		t.Fatal(err)
	}
	var acl struct {
		Protected  bool
		Owner, SID string
		Rules      []struct {
			SID    string
			Rights int
			Allow  bool
		}
	}
	if err := json.Unmarshal(data, &acl); err != nil {
		t.Fatal(err)
	}
	if !acl.Protected || acl.Owner != acl.SID || len(acl.Rules) != 1 || acl.Rules[0].SID != acl.SID || !acl.Rules[0].Allow || acl.Rules[0].Rights != 0x1f01ff {
		t.Fatalf("recovery intent is not owner-only: %+v", acl)
	}
	if _, err := command(ctx, powershell, []string{"MESSH_UPDATE_INTENT_PATH=" + path, "MESSH_UPDATE_INTENT=replacement"}, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", script); err == nil {
		t.Fatal("overwrote an existing recovery transaction")
	}
	persisted, err := os.ReadFile(path)
	if err != nil || string(persisted) != payload {
		t.Fatalf("original recovery intent lost: %q, %v", persisted, err)
	}
}
