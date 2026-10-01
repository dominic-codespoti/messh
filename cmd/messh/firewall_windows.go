//go:build windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"messh/internal/netcheck"
	"messh/internal/state"
)

func firewallAllowRun(c *Context, ctx context.Context, paths state.Paths) error {
	private, dryRun := c.String("private"), c.Bool("dry-run")
	exe := netcheck.ExecutablePath(netcheck.LoadNode(ctx, paths))
	fw := netcheck.ProbeWindows(ctx)
	alias := ""
	if private != "" {
		if !fw.ProfilesKnown {
			return fmt.Errorf("cannot list this PC's networks to check %q: %s", private, strings.Join(fw.Notes, "; "))
		}
		p, err := netcheck.ValidateAlias(private, fw.Profiles)
		if err != nil {
			return err
		}
		switch netcheck.ProfileForCategory(p.Category) {
		case netcheck.ProfilePrivate:
			if !c.JSON {
				fmt.Fprintf(c.Stdout, "%s (%s) is already Private.\n", p.Alias, p.Name)
			}
		case netcheck.ProfileDomain:
			if !c.JSON {
				fmt.Fprintf(c.Stdout, "%s (%s) is a domain network; its category is managed by the domain and stays as it is.\n", p.Alias, p.Name)
			}
		default:
			alias = p.Alias
		}
	}
	var blocks []netcheck.Rule
	if fw.RulesKnown {
		blocks = netcheck.ProgramBlockRules(fw.Rules, exe)
	} else if !c.JSON {
		fmt.Fprintf(c.Stdout, "Could not read the firewall rules (%s); existing block rules for messh are not removed.\n", strings.Join(fw.Notes, "; "))
	}
	plan, err := netcheck.PlanWindowsAllow(exe, blocks, alias)
	if err != nil {
		return err
	}
	if !c.JSON {
		fmt.Fprintf(c.Stdout, "Program: %s\nThe rules admit TCP and UDP 7519 for this program only from the local subnet (LocalSubnet), on Private and Domain networks.\n", exe)
		if strings.Contains(strings.ToLower(exe), `\go-build`) {
			fmt.Fprintln(c.Stdout, "Warning: this is a temporary `go run` binary; build messh.exe and run this command with it instead.")
		}
		for _, p := range fw.Profiles {
			if p.Alias != alias && netcheck.ProfileForCategory(p.Category) == netcheck.ProfilePublic {
				fmt.Fprintf(c.Stdout, "Note: %s (%s) is Public, where these rules do not apply. If it is your own network, add --private %q.\n", p.Alias, p.Name, p.Alias)
			}
		}
	}
	return runPlan(c, ctx, paths, plan, dryRun)
}

func firewallRemoveRun(c *Context, ctx context.Context, paths state.Paths) error {
	return runPlan(c, ctx, paths, netcheck.PlanWindowsRemove(), c.Bool("dry-run"))
}

// runPlan prints the plan and, unless dryRun, runs it in one elevated
// PowerShell (a single UAC prompt), then re-checks interfaces and firewall.
func runPlan(c *Context, ctx context.Context, paths state.Paths, plan netcheck.WinPlan, dryRun bool) error {
	res := firewallResult{DryRun: dryRun, Commands: plan.Lines()}
	if dryRun {
		return c.Emit(res, func(w io.Writer) {
			for _, s := range plan.Skipped {
				fmt.Fprintln(w, "Leaving block rule", s)
			}
			fmt.Fprintln(w, "Would run elevated (one UAC prompt):")
			for _, l := range plan.Lines() {
				fmt.Fprintln(w, "  "+l)
			}
		})
	}
	logFile, err := os.CreateTemp("", "messh-firewall-*.log")
	if err != nil {
		return err
	}
	logFile.Close()
	defer os.Remove(logFile.Name())
	script, err := plan.Script(logFile.Name())
	if err != nil {
		return err
	}
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return err
	}
	if !c.JSON {
		fmt.Fprintln(c.Stdout, "Asking Windows for administrator rights (UAC prompt) to run:")
		for _, l := range plan.Lines() {
			fmt.Fprintln(c.Stdout, "  "+l)
		}
	}
	code, err := runElevated(ctx, filepath.Join(sys, `WindowsPowerShell\v1.0\powershell.exe`),
		`-NoProfile -NonInteractive -Command "`+script+`"`)
	if errors.Is(err, windows.ERROR_CANCELLED) {
		return errors.New("cancelled at the UAC prompt; nothing changed")
	}
	if err != nil {
		return err
	}
	var lines []string
	if out, err := os.ReadFile(logFile.Name()); err == nil && len(out) > 0 {
		text := strings.ReplaceAll(string(out), "\ufeff", "")
		for line := range strings.Lines(text) {
			if line = strings.TrimRight(line, "\r\n"); strings.TrimSpace(line) != "" {
				lines = append(lines, "  "+line)
			}
		}
	}
	rep := netcheck.Run(ctx, paths).Filter("iface", "firewall")
	res.Applied = code == 0
	res.Checks = rep.Checks
	if err := c.Emit(res, func(w io.Writer) {
		for _, s := range plan.Skipped {
			fmt.Fprintln(w, "Leaving block rule", s)
		}
		for _, l := range lines {
			fmt.Fprintln(w, l)
		}
		fmt.Fprintln(w, "\nChecking the result:")
		rep.Render(w)
	}); err != nil {
		return err
	}
	if code != 0 {
		return fmt.Errorf("some commands failed (exit code %d); see their output above", code)
	}
	return nil
}

var procShellExecuteExW = windows.NewLazySystemDLL("shell32.dll").NewProc("ShellExecuteExW")

// shellExecuteInfo is SHELLEXECUTEINFOW.
type shellExecuteInfo struct {
	cbSize         uint32
	fMask          uint32
	hwnd           windows.Handle
	lpVerb         *uint16
	lpFile         *uint16
	lpParameters   *uint16
	lpDirectory    *uint16
	nShow          int32
	hInstApp       windows.Handle
	lpIDList       uintptr
	lpClass        *uint16
	hkeyClass      windows.Handle
	dwHotKey       uint32
	hIconOrMonitor windows.Handle
	hProcess       windows.Handle
}

const (
	seeMaskNoCloseProcess = 0x00000040
	seeMaskNoAsync        = 0x00000100
)

// runElevated starts file with params through the "runas" verb (the UAC
// prompt) and waits for it. ShellExecuteEx rather than ShellExecute: only it
// returns the process handle needed to wait and read the exit code.
func runElevated(ctx context.Context, file, params string) (uint32, error) {
	verb, err := windows.UTF16PtrFromString("runas")
	if err != nil {
		return 0, err
	}
	f, err := windows.UTF16PtrFromString(file)
	if err != nil {
		return 0, err
	}
	p, err := windows.UTF16PtrFromString(params)
	if err != nil {
		return 0, err
	}
	info := shellExecuteInfo{fMask: seeMaskNoCloseProcess | seeMaskNoAsync, lpVerb: verb, lpFile: f, lpParameters: p, nShow: windows.SW_HIDE}
	info.cbSize = uint32(unsafe.Sizeof(info))
	if r, _, e := procShellExecuteExW.Call(uintptr(unsafe.Pointer(&info))); r == 0 {
		return 0, e
	}
	if info.hProcess == 0 {
		return 0, errors.New("elevated process did not start")
	}
	defer windows.CloseHandle(info.hProcess)
	for {
		ev, err := windows.WaitForSingleObject(info.hProcess, 250)
		if err != nil {
			return 0, err
		}
		if ev == windows.WAIT_OBJECT_0 {
			break
		}
		if ctx.Err() != nil {
			return 0, errors.New("gave up waiting for the elevated commands")
		}
	}
	var code uint32
	if err := windows.GetExitCodeProcess(info.hProcess, &code); err != nil {
		return 0, err
	}
	return code, nil
}
