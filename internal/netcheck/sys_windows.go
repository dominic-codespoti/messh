//go:build windows

package netcheck

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
)

// probeTimeout bounds each PowerShell or netsh call; both normally finish in
// under two seconds.
const probeTimeout = 5 * time.Second

const createNoWindow = 0x08000000

// runHidden runs a console program without flashing a window and returns its stdout.
func runHidden(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
	cmd.WaitDelay = time.Second
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("%s timed out after %s", name, probeTimeout)
	}
	if err != nil {
		return out.Bytes(), fmt.Errorf("%s: %v %s", name, err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// ProbeWindows reads the connection profiles and the firewall through
// PowerShell, falling back to netsh for the rules and profile state when
// PowerShell is missing, slow or fails.
func ProbeWindows(ctx context.Context) WinFirewall {
	out, err := runHidden(ctx, "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", PowerShellScript)
	var fw WinFirewall
	if err == nil {
		fw, err = ParsePowerShell(out)
	}
	if err != nil {
		fw = WinFirewall{Notes: []string{"PowerShell: " + err.Error()}}
	}
	if fw.RulesKnown {
		return fw
	}
	text, err := runHidden(ctx, "netsh.exe", NetshRuleArgs...)
	var rules []Rule
	if err == nil {
		rules, err = ParseNetshRules(string(text))
	}
	if err != nil {
		fw.Notes = append(fw.Notes, "netsh: "+err.Error())
		return fw
	}
	for _, r := range rules {
		if r.Enabled && r.Inbound {
			fw.Rules = append(fw.Rules, r)
		}
	}
	fw.RulesKnown, fw.Source = true, "netsh"
	if prof, err := runHidden(ctx, "netsh.exe", NetshProfileArgs...); err == nil {
		fw.FirewallProfiles = ParseNetshProfiles(string(prof))
		for _, p := range fw.FirewallProfiles {
			fw.Current |= p.Type
		}
	}
	return fw
}

func osChecks(ctx context.Context, ifaces []Iface, exe string, ports Ports) []Check {
	return WindowsChecks(ProbeWindows(ctx), ifaces, exe, ports)
}

// processPath returns the executable of a running process.
func processPath(pid int) (string, error) {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return "", err
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &n); err != nil {
		return "", err
	}
	return windows.UTF16ToString(buf[:n]), nil
}
