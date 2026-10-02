//go:build windows

package updatehost

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"messh/internal/state"
)

const startTaskScript = `
$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)
$s = New-Object -ComObject 'Schedule.Service'
$s.Connect()
$t = $s.GetFolder('\').GetTask('messh')
if ($t.Xml -cne $env:MESSH_UPDATE_XML -or @($t.GetInstances(0)).Count -ne 0 -or -not $t.Enabled) { throw 'Task changed before RunEx' }
$r = $t.RunEx($null, 4, [int](Get-Process -Id $PID).SessionId, $null)
[Console]::Write([string]$r.InstanceGuid)
[Console]::Out.Flush()
`

func validTaskInstance(value string) bool {
	if len(value) == 38 && value[0] == '{' && value[37] == '}' {
		value = value[1:37]
	}
	if len(value) != 36 {
		return false
	}
	for i, c := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

func recoveryIntentPath(paths state.Paths) string {
	return filepath.Join(paths.Root, "update-host-recovery.json")
}

// Both disabling and restoration verify the original definition, wrapper bytes,
// user SID and interactive session. Only the Settings.Enabled bit may differ.
// The private, write-through intent precedes any scheduler mutation. A crash
// after disabling therefore leaves an explicitly recoverable task, never a
// silently abandoned launcher configuration.
const normalizedTaskDefinitionScript = `
function Normalized-Definition([string]$text) {
 $doc = New-Object System.Xml.XmlDocument
 $doc.PreserveWhitespace = $false
 $doc.LoadXml($text)
 $ns = New-Object System.Xml.XmlNamespaceManager($doc.NameTable)
 $ns.AddNamespace('t', 'http://schemas.microsoft.com/windows/2004/02/mit/task')
 $enabled = $doc.SelectSingleNode('/t:Task/t:Settings/t:Enabled', $ns)
 # Enabled defaults to true and may be omitted in an existing registration.
 # Ignore only that setting when comparing the temporary disabled definition.
 if ($null -ne $enabled) { [void]$enabled.ParentNode.RemoveChild($enabled) }
 return $doc.OuterXml
}
`

const taskRecoveryPrelude = normalizedTaskDefinitionScript + `
$ErrorActionPreference = 'Stop'
[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)
$scheduler = New-Object -ComObject 'Schedule.Service'
$scheduler.Connect()
$task = $scheduler.GetFolder('\').GetTask('messh')
$sid = [System.Security.Principal.WindowsIdentity]::GetCurrent().User
function Assert-Intent($intent) {
 if ($intent.version -ne 1 -or $intent.sid -cne $sid.Value -or $intent.session -ne [int](Get-Process -Id $PID).SessionId) { throw 'Recovery intent belongs to another user or session' }
 if ((Normalized-Definition $task.Xml) -cne (Normalized-Definition $intent.xml)) { throw 'Task definition changed; refusing recovery' }
 if ((Get-FileHash -LiteralPath $intent.launcher -Algorithm SHA256).Hash.ToLowerInvariant() -cne $intent.sourceHash) { throw 'Task wrapper changed; refusing recovery' }
}
function Assert-Candidate($intent) {
 foreach ($running in @($task.GetInstances(0))) {
  if ([string]$running.InstanceGuid -ine [string]$intent.instance) { throw 'Foreign task instance replaced updater candidate' }
 }
}
`

// Apply the private DACL at creation. A FileAccess.Write stream does not carry
// WRITE_DAC, so SetAccessControl on that handle fails for a normal user.
const createTaskRecoveryIntentScript = `
$acl = New-Object System.Security.AccessControl.FileSecurity
$acl.SetOwner($sid)
$acl.SetAccessRuleProtection($true, $false)
$acl.AddAccessRule((New-Object System.Security.AccessControl.FileSystemAccessRule($sid, 'FullControl', 'Allow')))
$stream = New-Object System.IO.FileStream($env:MESSH_UPDATE_INTENT_PATH, [System.IO.FileMode]::CreateNew, [System.Security.AccessControl.FileSystemRights]::FullControl, [System.IO.FileShare]::None, 4096, [System.IO.FileOptions]::WriteThrough, $acl)
try {
 $data = [System.Text.Encoding]::UTF8.GetBytes($env:MESSH_UPDATE_INTENT)
 $stream.Write($data, 0, $data.Length)
 $stream.Flush($true)
} finally { $stream.Dispose() }
`

const disableTaskScript = taskRecoveryPrelude + `
$intent = $env:MESSH_UPDATE_INTENT | ConvertFrom-Json
Assert-Intent $intent
if (-not $task.Enabled -or $task.Xml -cne $intent.xml) { throw 'Task changed before recovery disable' }
Assert-Candidate $intent
$gate = Get-Content -LiteralPath $env:MESSH_UPDATE_GATE -Raw | ConvertFrom-Json
if ($gate.lease -cne $env:MESSH_UPDATE_LEASE -or [DateTimeOffset]::Parse($gate.expires) -le [DateTimeOffset]::UtcNow.AddSeconds(15)) { throw 'Startup gate no longer protects candidate' }
` + createTaskRecoveryIntentScript + `
# Disable first: no restart-on-failure or trigger can launch another candidate
# while its executable is being replaced. Stop is scoped to this registration.
Assert-Intent $intent
Assert-Candidate $intent
$task.Enabled = $false
Assert-Candidate $intent
$task.Stop(0)
$deadline = [DateTime]::UtcNow.AddSeconds(6)
do {
 $task = $scheduler.GetFolder('\').GetTask('messh')
 Assert-Intent $intent
 if ($task.Enabled) { throw 'Task was enabled concurrently during recovery' }
 if (@($task.GetInstances(0)).Count -eq 0) { exit 0 }
 Start-Sleep -Milliseconds 100
} while ([DateTime]::UtcNow -lt $deadline)
throw 'Candidate task did not stop; leave intent and disabled task for explicit recovery'
`

const restoreTaskScript = taskRecoveryPrelude + `
$acl = [System.IO.File]::GetAccessControl($env:MESSH_UPDATE_INTENT_PATH)
if ($acl.GetOwner([System.Security.Principal.SecurityIdentifier]).Value -cne $sid.Value) { throw 'Recovery intent is not owned by current user' }
$intent = Get-Content -LiteralPath $env:MESSH_UPDATE_INTENT_PATH -Raw | ConvertFrom-Json
Assert-Intent $intent
# Never terminate a live node when recovering an interrupted CLI. It may have
# resumed normal admissions after the startup gate expired. Restoring its
# original Enabled setting is safe; only an inactive registration is stopped
# again to clear any queued retry before re-enabling.
if (-not $task.Enabled -and @($task.GetInstances(0)).Count -eq 0) { $task.Stop(0) }
Assert-Intent $intent
$task.Enabled = [bool]$intent.enabled
$task = $scheduler.GetFolder('\').GetTask('messh')
if ((Normalized-Definition $task.Xml) -cne (Normalized-Definition $intent.xml) -or $task.Enabled -ne [bool]$intent.enabled) { throw 'Original task configuration was not restored exactly' }
Remove-Item -LiteralPath $env:MESSH_UPDATE_INTENT_PATH -Force
`

func disableCandidateTask(ctx context.Context, powershell string, paths state.Paths, xml, launcher string, source []byte, instance string, gate state.UpdateStartup) error {
	intent := struct {
		Version    int    `json:"version"`
		XML        string `json:"xml"`
		Launcher   string `json:"launcher"`
		SourceHash string `json:"sourceHash"`
		Instance   string `json:"instance"`
		SID        string `json:"sid"`
		Session    int    `json:"session"`
		Enabled    bool   `json:"enabled"`
	}{Version: 1, XML: xml, Launcher: launcher, SourceHash: fmt.Sprintf("%x", sha256.Sum256(source)), Instance: instance, Enabled: true}
	snapshot, err := inspectTask(ctx, powershell, 0)
	if err != nil {
		return err
	}
	intent.SID, intent.Session = snapshot.SID, snapshot.Session
	data, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	_, err = command(ctx, powershell, []string{
		"MESSH_UPDATE_INTENT=" + string(data),
		"MESSH_UPDATE_INTENT_PATH=" + recoveryIntentPath(paths),
		"MESSH_UPDATE_GATE=" + paths.UpdateStartupFile(),
		"MESSH_UPDATE_LEASE=" + gate.Lease,
	}, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", disableTaskScript)
	return err
}

// RecoverInterrupted restores only the exact task temporarily disabled by an
// interrupted RecoverStop. Call before an explicit update, never from --check.
// It does not replace executables, stop a live node, or remove the startup gate.
// Foreign configuration, wrapper, owner, or session changes require manual
// recovery. A successful call restores the original Enabled bit and deletes
// the durable intent; other task settings and registration remain untouched.
func RecoverInterrupted(ctx context.Context, paths state.Paths) error {
	info, err := os.Lstat(recoveryIntentPath(paths))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("launcher recovery intent is not a regular file")
	}
	powershell := filepath.Join(os.Getenv("SystemRoot"), "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	if !filepath.IsAbs(powershell) {
		return fmt.Errorf("cannot locate system Windows PowerShell")
	}
	ctx, cancel := context.WithTimeout(ctx, operationTimeout)
	defer cancel()
	_, err = command(ctx, powershell, []string{"MESSH_UPDATE_INTENT_PATH=" + recoveryIntentPath(paths)}, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command", restoreTaskScript)
	return err
}
