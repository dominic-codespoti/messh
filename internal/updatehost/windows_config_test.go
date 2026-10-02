package updatehost

import (
	"reflect"
	"strings"
	"testing"
)

func TestWindowsArgumentBoundaries(t *testing.T) {
	cases := []struct {
		input string
		want  []string
	}{
		{`"C:\Program Files\messh.exe" node --state "C:\Users\A B\state" --json`, []string{`C:\Program Files\messh.exe`, "node", "--state", `C:\Users\A B\state`, "--json"}},
		{`a "" "C:\a b\\" "say\"hello"`, []string{"a", "", `C:\a b\`, `say"hello`}},
		{`-File "C:/Users/A B/start-node.ps1"`, []string{"-File", "C:/Users/A B/start-node.ps1"}},
	}
	for _, tc := range cases {
		got, err := windowsArgv(tc.input)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Errorf("argv(%q) = %q, %v; want %q", tc.input, got, err, tc.want)
		}
	}
	if _, err := windowsArgv(`node --state "unfinished`); err == nil {
		t.Fatal("accepted unterminated quoted state argument")
	}
}

func wrapperFixture(exe, root string) string {
	stateArgument := "$state"
	if strings.ContainsAny(root, " \t") {
		stateArgument = `('"' + $state + '"')`
	}
	return "$ErrorActionPreference = 'Stop'\n$binary = '" + exe + "'\n$state = '" + root + "'\n$logs = Join-Path $state 'logs'\n[void][System.IO.Directory]::CreateDirectory($logs)\n$node = Start-Process -FilePath $binary -ArgumentList @('node', '--state', " + stateArgument + ", '--json') -WorkingDirectory $state -NoNewWindow -RedirectStandardOutput (Join-Path $logs 'node.stdout.log') -RedirectStandardError (Join-Path $logs 'node.stderr.log') -PassThru -Wait\nexit $node.ExitCode\n"
}
func TestWrapperMustWaitAndPropagateSuccessfulExit(t *testing.T) {
	exe, root := `C:\Users\A B\messh.exe`, `C:\Users\A B\state`
	fixture := wrapperFixture(exe, root)
	if err := validateWrapper([]byte(fixture), exe, root); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"unquoted-spaced-state": strings.Replace(fixture, `('"' + $state + '"')`, "$state", 1),
		"detached":              strings.Replace(fixture, " -Wait", "", 1),
		"hidden-failure":        strings.Replace(fixture, "exit $node.ExitCode", "exit 0", 1),
		"wrong-state":           strings.Replace(fixture, root, `C:\other`, 1),
		"wrong-executable":      strings.Replace(fixture, exe, `C:\other.exe`, 1),
		"extra-statement":       fixture + "Start-Process evil.exe\n",
		"shell-expansion":       strings.Replace(fixture, "$binary", "$(Get-Command messh)", 1),
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validateWrapper([]byte(source), exe, root); err == nil {
				t.Fatal("accepted unsafe or unrelated launcher")
			}
		})
	}
}
func taskFixture() string {
	return `<?xml version="1.0" encoding="UTF-16"?><Task xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task"><Principals><Principal id="Author"><UserId>S-1-5-21-123</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals><Triggers><LogonTrigger/></Triggers><Settings><Enabled>true</Enabled><AllowStartOnDemand>true</AllowStartOnDemand><MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy><ExecutionTimeLimit>PT0S</ExecutionTimeLimit></Settings><Actions Context="Author"><Exec><Command>powershell.exe</Command><Arguments>-NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -WindowStyle Hidden -File "C:\Users\A B\state\start-node.ps1"</Arguments></Exec></Actions></Task>`
}
func TestTaskOnlyUsesCurrentInteractiveUserAndExactAction(t *testing.T) {
	fixture := taskFixture()
	validate := func(data string) error {
		return validateTaskXML(data, "S-1-5-21-123", `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`, `C:\Users\A B\state\start-node.ps1`)
	}
	if err := validate(fixture); err != nil {
		t.Fatal(err)
	}
	existing := strings.ReplaceAll(strings.ReplaceAll(fixture, "<RunLevel>LeastPrivilege</RunLevel>", ""), "<Enabled>true</Enabled>", "")
	existing = strings.ReplaceAll(existing, "<AllowStartOnDemand>true</AllowStartOnDemand>", "")
	existing = strings.Replace(existing, "</Exec>", `<WorkingDirectory>C:\Users\A B\state</WorkingDirectory></Exec>`, 1)
	if err := validate(existing); err != nil {
		t.Fatalf("existing interactive task with schema defaults: %v", err)
	}
	cases := map[string]string{
		"other-owner":             strings.Replace(fixture, "S-1-5-21-123", "S-1-5-21-999", 1),
		"elevated":                strings.Replace(fixture, "LeastPrivilege", "HighestAvailable", 1),
		"noninteractive":          strings.Replace(fixture, "InteractiveToken", "Password", 1),
		"other-shell":             strings.Replace(fixture, "powershell.exe", "pwsh.exe", 1),
		"other-action":            strings.Replace(fixture, "</Actions>", "<ComHandler/></Actions>", 1),
		"extra-argument":          strings.Replace(fixture, "</Arguments>", " -Command evil</Arguments>", 1),
		"periodic-race":           strings.Replace(fixture, "LogonTrigger", "TimeTrigger", 1),
		"multiple-instances":      strings.Replace(fixture, "IgnoreNew", "Parallel", 1),
		"other-working-directory": strings.Replace(fixture, "</Exec>", `<WorkingDirectory>C:\other</WorkingDirectory></Exec>`, 1),
		"disabled":                strings.Replace(fixture, "<Enabled>true", "<Enabled>false", 1),
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			if err := validate(source); err == nil {
				t.Fatal("accepted unrelated or unsafe task")
			}
		})
	}
}
