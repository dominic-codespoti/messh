//go:build windows

package updatehost

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"messh/internal/state"
)

type windowsProcess struct {
	PID         int    `json:"pid"`
	Parent      int    `json:"parent"`
	Executable  string `json:"executable"`
	CommandLine string `json:"commandLine"`
	Created     string `json:"created"`
	SID         string `json:"sid"`
	Session     int    `json:"session"`
}
type taskSnapshot struct {
	XML           string           `json:"xml"`
	Configuration string           `json:"configuration"`
	State         string           `json:"state"`
	Result        uint32           `json:"result"`
	SID           string           `json:"sid"`
	Session       int              `json:"session"`
	Engines       []int            `json:"engines"`
	Instances     []string         `json:"instances"`
	Processes     []windowsProcess `json:"processes"`
}

// The program is fixed. User-controlled paths and IDs never become script text.
// GetRunningTasks identifies this task's engine, then CIM verifies the exact
// node-to-wrapper-to-engine ancestry. No global process-name search is used.
const inspectTaskScript = normalizedTaskDefinitionScript + `
$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)
$task = Get-ScheduledTask -TaskName 'messh' -TaskPath '\'
$info = Get-ScheduledTaskInfo -InputObject $task
$scheduler = New-Object -ComObject 'Schedule.Service'
$scheduler.Connect()
$running = @($scheduler.GetRunningTasks(0) | Where-Object { $_.Path -eq '\messh' })
$engines = @($running | ForEach-Object { [int]$_.EnginePID })
$instances = @($running | ForEach-Object { [string]$_.InstanceGuid })
$processes = @()
$next = [int]$env:MESSH_UPDATE_PID
for ($i = 0; $i -lt 5 -and $next -gt 0; $i++) {
 $p = Get-CimInstance Win32_Process -Filter ('ProcessId = ' + $next)
 if ($null -eq $p) { break }
 $sid = ''
 if ($i -lt 2) {
  $owner = Invoke-CimMethod -InputObject $p -MethodName GetOwnerSid
  if ($owner.ReturnValue -ne 0) { throw 'Cannot establish node or wrapper process ownership' }
  $sid = [string]$owner.Sid
 }
 $processes += @{ pid = [int]$p.ProcessId; parent = [int]$p.ParentProcessId; executable = [string]$p.ExecutablePath; commandLine = [string]$p.CommandLine; created = $p.CreationDate.ToUniversalTime().ToString('o'); sid = $sid; session = [int]$p.SessionId }
 if ($engines -contains [int]$p.ProcessId) { break }
 $next = [int]$p.ParentProcessId
}
$xml = [string]$scheduler.GetFolder('\').GetTask('messh').Xml
@{ xml = $xml; configuration = (Normalized-Definition $xml); state = [string]$task.State; result = [uint32]$info.LastTaskResult; sid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value; session = [int](Get-Process -Id $PID).SessionId; engines = $engines; instances = $instances; processes = $processes } | ConvertTo-Json -Depth 5 -Compress
`

func inspectTask(ctx context.Context, powershell string, pid int) (taskSnapshot, error) {
	var result taskSnapshot
	data, err := command(ctx, powershell, []string{"MESSH_UPDATE_PID=" + strconv.Itoa(pid)}, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", inspectTaskScript)
	if err != nil {
		return result, manual("cannot inspect current-user scheduled task: " + err.Error())
	}
	if err := json.Unmarshal(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf}), &result); err != nil {
		return result, fmt.Errorf("decode scheduled task inspection: %w", err)
	}
	return result, nil
}
func validateWindowsInstance(s taskSnapshot, exe, root, powershell, launcher string, pid int) error {
	if err := validateTaskXML(s.XML, s.SID, powershell, launcher); err != nil {
		return err
	}
	if s.State != "Running" || len(s.Processes) < 2 || len(s.Engines) != 1 {
		return manual("messh task is not the unique running launcher")
	}
	node, wrapper := s.Processes[0], s.Processes[1]
	if node.PID != pid || node.Parent != wrapper.PID || node.SID != s.SID || wrapper.SID != s.SID || node.Session != s.Session || wrapper.Session != s.Session {
		return manual("node and launcher must belong to the current user's interactive session")
	}
	if !windowsPathEqual(node.Executable, exe) || !windowsPathEqual(wrapper.Executable, powershell) {
		return manual("node or launcher executable does not match")
	}
	args, err := windowsArgv(node.CommandLine)
	if err != nil || len(args) != 5 || !windowsPathEqual(args[0], exe) || args[1] != "node" || args[2] != "--state" || !windowsPathEqual(args[3], root) || args[4] != "--json" {
		return manual("node command line does not match the supported state directory and arguments")
	}
	args, err = windowsArgv(wrapper.CommandLine)
	if err != nil || len(args) == 0 || (!windowsPathEqual(args[0], powershell) && !strings.EqualFold(args[0], "powershell.exe")) || !powershellArgs(args[1:], launcher) {
		return manual("parent process does not match the configured PowerShell action")
	}
	engineFound := false
	for _, p := range s.Processes[1:] {
		if p.PID == s.Engines[0] {
			engineFound = true
		}
	}
	if !engineFound {
		return manual("node ancestry is not owned by the messh scheduled task engine")
	}
	return nil
}
func discoverPlatform(ctx context.Context, exe, root string, run state.RunInfo) (*Manager, error) {
	// SystemRoot is the system installation, not PATH or a user-selected shell.
	powershell := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	if !filepath.IsAbs(powershell) {
		return nil, manual("cannot locate system Windows PowerShell")
	}
	launcher := filepath.Join(root, "start-node.ps1")
	source, err := os.ReadFile(launcher)
	if err != nil {
		return nil, manual("cannot read start-node.ps1: " + err.Error())
	}
	if err := validateWrapper(source, exe, root); err != nil {
		return nil, err
	}
	original, err := inspectTask(ctx, powershell, run.PID)
	if err != nil {
		return nil, err
	}
	if err := validateWindowsInstance(original, exe, root, powershell, launcher, run.PID); err != nil {
		return nil, err
	}
	configurationUnchanged := func(s taskSnapshot) error {
		current, err := os.ReadFile(launcher)
		if err != nil {
			return err
		}
		if s.Configuration == "" || s.Configuration != original.Configuration || !bytes.Equal(current, source) || s.SID != original.SID || s.Session != original.Session {
			return manual("scheduled task, launcher, or interactive session changed during update")
		}
		return nil
	}
	m := &Manager{kind: "scheduled-task"}
	var instance string
	var launchIssued bool
	m.validate = func(ctx context.Context) error {
		current, err := inspectTask(ctx, powershell, run.PID)
		if err != nil {
			return err
		}
		if err := configurationUnchanged(current); err != nil {
			return err
		}
		if err := validateWindowsInstance(current, exe, root, powershell, launcher, run.PID); err != nil {
			return err
		}
		if current.Processes[0].Created != original.Processes[0].Created || current.Processes[1].Created != original.Processes[1].Created {
			return manual("node or wrapper process changed during update")
		}
		return nil
	}
	m.stopped = func(ctx context.Context) (bool, error) {
		current, err := inspectTask(ctx, powershell, run.PID)
		if err != nil {
			return false, err
		}
		if err := configurationUnchanged(current); err != nil {
			return false, err
		}
		if len(current.Processes) != 0 && current.Processes[0].Created == original.Processes[0].Created {
			return false, nil
		}
		if current.State == "Ready" && len(current.Engines) == 0 {
			if current.Result != 0 {
				return false, manual("scheduled task did not exit successfully; its restart-on-failure policy may still be active")
			}
			return true, nil
		}
		if current.State != "Running" {
			return false, manual("scheduled task entered an unexpected shutdown state")
		}
		return false, nil
	}
	m.start = func(ctx context.Context) error {
		instance, launchIssued = "", false
		if err := RecoverInterrupted(ctx, m.paths); err != nil {
			return err
		}
		current, err := inspectTask(ctx, powershell, run.PID)
		if err != nil {
			return err
		}
		if err := configurationUnchanged(current); err != nil {
			return err
		}
		if current.State != "Ready" || len(current.Engines) != 0 {
			return manual("scheduled task is no longer safely stopped")
		}
		launchIssued = true
		data, err := command(ctx, powershell, []string{"MESSH_UPDATE_XML=" + current.XML}, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", startTaskScript)
		// Preserve the exact COM-created instance even when the command exits
		// unsuccessfully after RunEx. Never infer a GUID from a process name.
		instance = strings.TrimSpace(string(bytes.TrimPrefix(data, []byte{0xef, 0xbb, 0xbf})))
		if !validTaskInstance(instance) {
			instance = ""
			if err == nil {
				return manual("scheduler did not return a candidate instance identifier")
			}
		}
		return err
	}
	m.recoverStop = func(ctx context.Context, graceful func(int) (bool, error)) error {
		pid := 0
		if candidate, err := m.paths.LoadRunInfo(); err == nil {
			pid = candidate.PID
		}
		current, err := inspectTask(ctx, powershell, pid)
		if err != nil {
			return err
		}
		if err := configurationUnchanged(current); err != nil {
			return err
		}
		if !launchIssued {
			if current.State == "Ready" && len(current.Engines) == 0 {
				return nil
			}
			return manual("task became active without an updater start")
		}
		if err := validateTaskRecoveryInstance(current, instance); err != nil {
			return err
		}
		var created string
		if len(current.Processes) > 0 {
			if err := validateWindowsInstance(current, exe, root, powershell, launcher, pid); err != nil {
				return err
			}
			created = current.Processes[0].Created
			accepted, err := graceful(pid)
			if err != nil {
				return err
			}
			if accepted {
				for {
					current, err = inspectTask(ctx, powershell, pid)
					if err != nil {
						return err
					}
					if err := configurationUnchanged(current); err != nil {
						return err
					}
					if err := validateTaskRecoveryInstance(current, instance); err != nil {
						return err
					}
					if len(current.Processes) == 0 || current.Processes[0].Created != created {
						break
					}
					select {
					case <-ctx.Done():
						return ctx.Err()
					case <-time.After(150 * time.Millisecond):
					}
				}
			}
		}
		if err := m.checkGate(); err != nil {
			return err
		}
		// The disable intent is durable before the scheduler setting changes.
		// Leave the task disabled until rollback has replaced the executable.
		// Start (or the next explicit update's RecoverInterrupted) restores it.
		if err := disableCandidateTask(ctx, powershell, m.paths, current.XML, launcher, source, instance, *m.gate); err != nil {
			return err
		}
		current, err = inspectTask(ctx, powershell, pid)
		if err != nil {
			return err
		}
		if current.State != "Disabled" || len(current.Engines) != 0 {
			return manual("candidate launcher did not remain disabled and inactive")
		}
		if created != "" && len(current.Processes) > 0 && current.Processes[0].Created == created {
			return manual("candidate process survived its task stop; executable cannot be replaced")
		}
		return nil
	}
	return m, nil
}

func validateTaskRecoveryInstance(current taskSnapshot, instance string) error {
	if !validTaskInstance(instance) {
		return manual("candidate start result is ambiguous; no scheduler instance identifier was received")
	}
	if len(current.Instances) > 1 || len(current.Instances) != len(current.Engines) {
		return manual("candidate task has ambiguous scheduler ownership")
	}
	if len(current.Instances) == 1 && !strings.EqualFold(current.Instances[0], instance) {
		return manual("a foreign scheduler instance replaced the candidate")
	}
	if current.State == "Running" && len(current.Instances) == 0 {
		return manual("running candidate has no scheduler instance identity")
	}
	return nil
}
