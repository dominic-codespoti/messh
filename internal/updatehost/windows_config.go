package updatehost

import (
	"encoding/xml"
	"fmt"
	"io"
	"regexp"
	"strings"
)

type taskDefinition struct {
	XMLName    xml.Name `xml:"Task"`
	Principals []struct {
		ID        string `xml:"id,attr"`
		UserID    string `xml:"UserId"`
		LogonType string `xml:"LogonType"`
		RunLevel  string `xml:"RunLevel"`
	} `xml:"Principals>Principal"`
	Actions struct {
		Context string `xml:"Context,attr"`
		Exec    []struct {
			Command          string `xml:"Command"`
			Arguments        string `xml:"Arguments"`
			WorkingDirectory string `xml:"WorkingDirectory"`
		} `xml:"Exec"`
		Other []struct{ XMLName xml.Name } `xml:",any"`
	} `xml:"Actions"`
	Settings struct {
		Enabled                 string `xml:"Enabled"`
		AllowStartOnDemand      string `xml:"AllowStartOnDemand"`
		MultipleInstancesPolicy string `xml:"MultipleInstancesPolicy"`
		ExecutionTimeLimit      string `xml:"ExecutionTimeLimit"`
	} `xml:"Settings"`
	Triggers struct {
		Entries []struct{ XMLName xml.Name } `xml:",any"`
	} `xml:"Triggers"`
}

// Split Windows argv with the documented backslash-before-quote rules. Empty
// quoted arguments are preserved. No expansion or shell evaluation is performed.
func windowsArgv(command string) ([]string, error) {
	var args []string
	for i := 0; i < len(command); {
		for i < len(command) && (command[i] == ' ' || command[i] == '\t') {
			i++
		}
		if i == len(command) {
			break
		}
		var arg strings.Builder
		quoted := false
		for i < len(command) {
			if !quoted && (command[i] == ' ' || command[i] == '\t') {
				break
			}
			slash := 0
			for i < len(command) && command[i] == '\\' {
				slash++
				i++
			}
			if i < len(command) && command[i] == '"' {
				arg.WriteString(strings.Repeat("\\", slash/2))
				if slash%2 == 1 {
					arg.WriteByte('"')
				} else {
					quoted = !quoted
				}
				i++
				continue
			}
			arg.WriteString(strings.Repeat("\\", slash))
			if i == len(command) {
				break
			}
			if !quoted && (command[i] == ' ' || command[i] == '\t') {
				break
			}
			arg.WriteByte(command[i])
			i++
		}
		if quoted {
			return nil, fmt.Errorf("unterminated Windows command-line quote")
		}
		args = append(args, arg.String())
	}
	return args, nil
}
func windowsPathEqual(a, b string) bool {
	return strings.EqualFold(strings.ReplaceAll(a, "/", "\\"), strings.ReplaceAll(b, "/", "\\"))
}
func powershellArgs(args []string, launcher string) bool {
	expected := []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-WindowStyle", "Hidden", "-File", launcher}
	if len(args) != len(expected) {
		return false
	}
	for i := range args {
		if i == len(args)-1 {
			if !windowsPathEqual(args[i], expected[i]) {
				return false
			}
		} else if !strings.EqualFold(args[i], expected[i]) {
			return false
		}
	}
	return true
}
func validateTaskXML(data, sid, powershell, launcher string) error {
	var task taskDefinition
	decoder := xml.NewDecoder(strings.NewReader(data))
	// Export-ScheduledTask's XML declaration describes its original UTF-16
	// file, but PowerShell has already decoded it before JSON serialization.
	decoder.CharsetReader = func(charset string, input io.Reader) (io.Reader, error) {
		if strings.EqualFold(charset, "utf-16") {
			return input, nil
		}
		return nil, fmt.Errorf("unsupported task XML charset %q", charset)
	}
	if err := decoder.Decode(&task); err != nil {
		return manual("cannot parse scheduled task: " + err.Error())
	}
	if len(task.Principals) != 1 || task.Principals[0].UserID != sid || task.Principals[0].LogonType != "InteractiveToken" || (task.Principals[0].RunLevel != "" && task.Principals[0].RunLevel != "LeastPrivilege") || task.Actions.Context != task.Principals[0].ID {
		return manual("messh task must use the current user's limited interactive principal")
	}
	if len(task.Actions.Exec) != 1 || len(task.Actions.Other) != 0 {
		return manual("messh task must have exactly one executable action")
	}
	action := task.Actions.Exec[0]
	if !windowsPathEqual(action.Command, powershell) && !strings.EqualFold(action.Command, "powershell.exe") {
		return manual("messh task must launch the system Windows PowerShell")
	}
	args, err := windowsArgv(action.Arguments)
	if err != nil || !powershellArgs(args, launcher) {
		return manual("messh task does not use the supported start-node.ps1 action")
	}
	directoryEnd := strings.LastIndexAny(launcher, `\/`)
	if directoryEnd < 0 || (action.WorkingDirectory != "" && !windowsPathEqual(action.WorkingDirectory, launcher[:directoryEnd])) {
		return manual("task working directory differs from the expected state directory")
	}
	// Schema defaults: RunLevel=LeastPrivilege, Enabled=true, and
	// AllowStartOnDemand=true. Export-ScheduledTask omits these on existing tasks.
	if (task.Settings.Enabled != "" && task.Settings.Enabled != "true") || (task.Settings.AllowStartOnDemand != "" && task.Settings.AllowStartOnDemand != "true") || task.Settings.MultipleInstancesPolicy != "IgnoreNew" || task.Settings.ExecutionTimeLimit != "PT0S" {
		return manual("messh task must be enabled, demand-startable, single-instance, and have no execution time limit")
	}
	for _, trigger := range task.Triggers.Entries {
		if trigger.XMLName.Local != "LogonTrigger" {
			return manual("scheduled task triggers other than logon could race replacement")
		}
	}
	return nil
}

var wrapperPattern = regexp.MustCompile(`(?s)\A\s*\$ErrorActionPreference\s*=\s*'Stop'\s+\$binary\s*=\s*'((?:[^']|'')*)'\s+\$state\s*=\s*'((?:[^']|'')*)'\s+\$logs\s*=\s*Join-Path\s+\$state\s+'logs'\s+\[void\]\[System\.IO\.Directory\]::CreateDirectory\(\$logs\)\s+\$node\s*=\s*Start-Process\s+-FilePath\s+\$binary\s+-ArgumentList\s+@\(\s*'node'\s*,\s*'--state'\s*,\s*(\$state|\(\s*'"'\s*\+\s*\$state\s*\+\s*'"'\s*\))\s*,\s*'--json'\s*\)\s+-WorkingDirectory\s+\$state\s+-NoNewWindow\s+-RedirectStandardOutput\s+\(Join-Path\s+\$logs\s+'node\.stdout\.log'\)\s+-RedirectStandardError\s+\(Join-Path\s+\$logs\s+'node\.stderr\.log'\)\s+-PassThru\s+-Wait\s+exit\s+\$node\.ExitCode\s*\z`)

func validateWrapper(data []byte, exe, root string) error {
	// This deliberately recognizes a small, auditable grammar, never executes or
	// sources an arbitrary script to learn how it launches the process.
	text := strings.TrimPrefix(string(data), "\ufeff")
	match := wrapperPattern.FindStringSubmatch(text)
	if len(match) != 4 || !windowsPathEqual(strings.ReplaceAll(match[1], "''", "'"), exe) || !windowsPathEqual(strings.ReplaceAll(match[2], "''", "'"), root) {
		return manual("start-node.ps1 is not the supported wait-and-propagate-exit-code launcher for this executable and state directory")
	}
	if strings.ContainsAny(root, " \t") && match[3] == "$state" {
		return manual("start-node.ps1 must quote the state argument inside Start-Process ArgumentList when the state path contains spaces")
	}
	return nil
}
