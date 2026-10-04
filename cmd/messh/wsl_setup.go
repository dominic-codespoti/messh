//go:build windows

package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"golang.org/x/sys/windows"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"

	"messh/internal/state"
	"messh/internal/wslroute"
)

//go:embed wslscripts/wsl-route.ps1 wslscripts/wsl-start.ps1
var wslScripts embed.FS

// wslInterfaceAddress returns the current IPv4 address of one Windows adapter
// index. It reads local interface state only: no exec, no network, no
// privilege. Non-Windows builds never reach it (see wsl_other.go).
func wslInterfaceAddress(ctx context.Context, index int) (string, error) {
	_ = ctx
	if index <= 0 {
		return "", fmt.Errorf("interface index %d is not positive", index)
	}
	iface, err := net.InterfaceByIndex(index)
	if err != nil {
		return "", err
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return "", err
	}
	for _, a := range addrs {
		var ip net.IP
		switch v := a.(type) {
		case *net.IPNet:
			ip = v.IP
		case *net.IPAddr:
			ip = v.IP
		default:
			continue
		}
		if ip == nil {
			continue
		}
		addr, ok := netip.AddrFromSlice(ip)
		if !ok {
			continue
		}
		addr = addr.Unmap()
		if !addr.Is4() {
			continue
		}
		b := addr.As4()
		private := b[0] == 10 || (b[0] == 172 && b[1] >= 16 && b[1] <= 31) || (b[0] == 192 && b[1] == 168)
		if private && !addr.IsLoopback() {
			return addr.String(), nil
		}
	}
	return "", fmt.Errorf("interface %s (#%d) has no private IPv4 address", iface.Name, index)
}

// wslCheckDistro confirms the selected distro is installed without starting
// anything. It lists inventories only, never --exec.
func wslCheckDistro(ctx context.Context, distro string) error {
	names, err := wslListDistros(ctx, false)
	if err != nil {
		return err
	}
	for _, n := range names {
		if n == distro {
			return nil
		}
	}
	return withHint(fmt.Errorf("distro %q is not installed (wsl --list --quiet shows: %s)", distro, strings.Join(names, ", ")),
		"install it first, or re-run with the exact installed name")
}

// wslListDistros runs the fixed no-start inventory. The argv is fixed;
// nothing from the config, the network, or the caller can steer it, and it
// never executes inside a guest.
func wslListDistros(ctx context.Context, running bool) ([]string, error) {
	argv := []string{"--list", "--quiet"}
	if running {
		argv = []string{"--list", "--running", "--quiet"}
	}
	out, err := wslRunHidden(ctx, wslSystemPath("wsl.exe"), argv...)
	if err != nil {
		return nil, fmt.Errorf("cannot inventory WSL distributions: %v", err)
	}
	return parseWSLInventory(out), nil
}

// parseWSLInventory decodes `wsl --list --quiet` output: one distro per line.
// wsl.exe writes UTF-16LE; without a BOM, any NUL byte still means UTF-16LE,
// otherwise the bytes are plain UTF-8. Empty lines are dropped.
func parseWSLInventory(data []byte) []string {
	var out []string
	for _, line := range strings.Split(wslDecodeConsole(data), "\n") {
		if name := strings.TrimSpace(strings.TrimSuffix(line, "\r")); name != "" {
			out = append(out, name)
		}
	}
	return out
}

func decodeWSLUTF16LE(b []byte) string {
	u16 := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u16 = append(u16, uint16(b[i])|uint16(b[i+1])<<8)
	}
	s := ""
	for _, r := range u16 {
		if r == 0 {
			continue
		}
		s += string(rune(r))
	}
	return s
}

// wslCheckCollisions refuses unrelated portproxy rows, firewall rules, and
// scheduled tasks that would collide with this target instead of overwriting
// them. It reads only; setup/refresh change nothing here.
func wslCheckCollisions(ctx context.Context, paths state.Paths, cfg state.WSLTargetConfig) error {
	if out, err := wslRunHidden(ctx, wslSystemPath("netsh.exe"), "interface", "portproxy", "show", "v4tov4"); err == nil {
		if clash := classifyWSLPortproxy(wslDecodeConsole(out), cfg); clash != "" {
			return fmt.Errorf("%s", clash)
		}
	}
	if out, err := wslRunHidden(ctx, wslSystemPath("powershell.exe"), "-NoProfile", "-NonInteractive", "-Command", wslFirewallInspectScript(cfg)); err == nil {
		if clash := strings.TrimSpace(wslDecodeConsole(out)); clash != "" {
			return fmt.Errorf("%s", clash)
		}
	}
	for _, name := range []string{cfg.TaskName, cfg.RouteTaskName} {
		if err := wslRejectForeignTask(ctx, name); err != nil {
			return err
		}
	}
	return nil
}

// wslPlanSetup resolves the host address on the selected interface (unless
// explicitly given) and runs the full validation plus collision checks that
// do not change anything.
func wslPlanSetup(ctx context.Context, paths state.Paths, cfg state.WSLTargetConfig, hostExplicit bool) (state.WSLTargetConfig, error) {
	if !hostExplicit {
		addr, err := wslInterfaceAddress(ctx, cfg.HostInterfaceIndex)
		if err != nil {
			return state.WSLTargetConfig{}, withHint(fmt.Errorf("cannot resolve --interface-index %d: %v", cfg.HostInterfaceIndex, err),
				"pick the adapter from `messh doctor`, or pass --host-address explicitly (it must still be that interface's address)")
		}
		cfg.HostAddress = addr
	} else {
		addr, err := wslInterfaceAddress(ctx, cfg.HostInterfaceIndex)
		if err != nil {
			return state.WSLTargetConfig{}, withHint(fmt.Errorf("cannot resolve --interface-index %d: %v", cfg.HostInterfaceIndex, err),
				"pick the adapter from `messh doctor`")
		}
		if addr != cfg.HostAddress {
			return state.WSLTargetConfig{}, usageErrorf("--host-address %s is not on --interface-index %d (it holds %s): pass the matching pair", cfg.HostAddress, cfg.HostInterfaceIndex, addr)
		}
	}
	if err := cfg.Validate(); err != nil {
		return state.WSLTargetConfig{}, usageErrorf("%v", err)
	}
	if err := wslCheckDistro(ctx, cfg.Distro); err != nil {
		return state.WSLTargetConfig{}, err
	}
	if err := wslCheckCollisions(ctx, paths, cfg); err != nil {
		return state.WSLTargetConfig{}, err
	}
	return cfg, nil
}

// wslPlanRefresh re-resolves the host address on the captured interface so a
// DHCP move is picked up, and reports whether the capture went stale.
func wslPlanRefresh(ctx context.Context, cfg state.WSLTargetConfig) (state.WSLTargetConfig, string, bool, error) {
	previous := cfg.HostAddress
	addr, err := wslInterfaceAddress(ctx, cfg.HostInterfaceIndex)
	if err != nil {
		return state.WSLTargetConfig{}, previous, false, withHint(fmt.Errorf("cannot resolve captured interface index %d: %v", cfg.HostInterfaceIndex, err),
			"if the adapter changed, re-run `messh wsl setup` with the new --interface-index")
	}
	cfg.HostAddress = addr
	if err := cfg.Validate(); err != nil {
		return state.WSLTargetConfig{}, previous, false, fmt.Errorf("the captured target no longer validates: %v", err)
	}
	if err := wslCheckCollisions(ctx, state.Paths{}, cfg); err != nil {
		return state.WSLTargetConfig{}, previous, false, err
	}
	return cfg, previous, addr != previous, nil
}

// classifyWSLPortproxy is pure for collision checks. A private guest-IP row
// at this listen address/port is replaceable because NAT addresses move.
func classifyWSLPortproxy(netsh string, cfg state.WSLTargetConfig) string {
	wantPort := strconv.Itoa(cfg.MeshPort)
	for _, line := range strings.Split(netsh, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		listenAddr, listenPort, connectAddr, connectPort := fields[0], fields[1], fields[2], fields[3]
		if listenAddr != cfg.HostAddress || listenPort != wantPort {
			continue
		}
		if connectPort == wantPort && connectAddr != cfg.HostAddress && wslroute.IsGuestIPv4(connectAddr) {
			continue
		}
		return fmt.Sprintf("portproxy %s:%s forwards to %s:%s instead of a private WSL guest address: remove it with netsh, then re-run messh wsl refresh elevated", listenAddr, listenPort, connectAddr, connectPort)
	}
	return ""
}

// wslFirewallInspectScript reports a collision string (empty when fine) for
// the single expected rule: an unrelated rule under our name, or a rule
// whose scope is broader than our explicit peers. Values travel as
// single-quoted literals built by psQuote; the script holds no double quote.
func wslFirewallInspectScript(cfg state.WSLTargetConfig) string {
	peerList := strings.Join(cfg.AllowedPeers, ",")
	return "$ErrorActionPreference='Stop'; " +
		"$r=Get-NetFirewallRule -DisplayName " + psQuote(state.WSLFirewallRuleName(cfg.MeshPort)) + " -ErrorAction SilentlyContinue; " +
		"if ($null -eq $r) { '' } else { " +
		"$f=$r | Get-NetFirewallPortFilter; $a=$r | Get-NetFirewallAddressFilter; " +
		"$wantPort='" + strconv.Itoa(cfg.MeshPort) + "'; $wantPeers=" + psQuote(peerList) + "; " +
		"if ($f.Protocol -ne 'TCP' -or ([string]$f.LocalPort) -ne $wantPort) { 'unrelated firewall rule ' + $r.DisplayName + ' collides (protocol ' + $f.Protocol + ' port ' + $f.LocalPort + '): remove it in wf.msc, then re-run `messh wsl refresh` elevated' } " +
		"elseif (([string]$a.RemoteAddress) -ne $wantPeers) { 'firewall rule ' + $r.DisplayName + ' allows ' + $a.RemoteAddress + ' instead of ' + $wantPeers + ': narrow it in wf.msc or re-run `messh wsl setup` elevated' } " +
		"else { '' } }"
}

// psQuote makes s a single-quoted PowerShell string literal.
func psQuote(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for _, r := range s {
		if r == '\'' || r == '‘' || r == '’' || r == '‚' || r == '‛' {
			b.WriteRune(r)
		}
		b.WriteRune(r)
	}
	b.WriteByte('\'')
	return b.String()
}

// wslDecodeConsole decodes console program output: UTF-16LE with a BOM (what
// wsl.exe and schtasks /XML write), bare-NUL UTF-16LE without one, or plain
// UTF-8 otherwise (PowerShell already emits UTF-8 after the installer sets
// OutputEncoding; netsh is ANSI text). Callers parsing output must use this,
// never a raw string conversion, or non-UTF-8 inventories read as garbage
// and ownership checks silently mismatch.
func wslDecodeConsole(out []byte) string {
	if len(out) >= 2 && out[0] == 0xFF && out[1] == 0xFE {
		return strings.TrimSpace(decodeWSLUTF16LE(out[2:]))
	}
	if bytes.IndexByte(out, 0) >= 0 {
		return strings.TrimSpace(decodeWSLUTF16LE(out))
	}
	return strings.TrimSpace(strings.TrimPrefix(string(out), "\ufeff"))
}

// wslSystemPath resolves a system binary from SystemRoot, never from PATH.
func wslSystemPath(base string) string {
	sys := os.Getenv("SystemRoot")
	if sys == "" {
		sys = `C:\Windows`
	}
	return filepath.Join(sys, "System32", base)
}

func newWSLContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), timeout)
}

// wslRunHidden runs a console program with no window flash and returns its
func wslRunHidden(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	cmd.WaitDelay = time.Second
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("%s %s: %v %s", filepath.Base(name), strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}

// wslRejectForeignTask refuses a scheduled-task name already taken by an
// unrelated task. Absent is fine (free to create); a task whose XML mentions
// our start/route scripts is ours and will be replaced by the installer.
// Anything else is a collision: setup never adopts or overwrites it.
func wslRejectForeignTask(ctx context.Context, name string) error {
	out, err := wslRunHidden(ctx, wslSystemPath("schtasks.exe"), "/Query", "/TN", name, "/XML")
	if err != nil {
		return nil
	}
	text := wslDecodeConsole(out)
	if strings.Contains(text, "wsl-start.ps1") || strings.Contains(text, "wsl-route.ps1") {
		return nil
	}
	return fmt.Errorf("unrelated scheduled task %q already exists: rename it in Task Scheduler, or re-run `messh wsl setup` with different task names", name)
}

// wslWriteOwnerStartScript installs the owner-context boot script in the
// owner's state directory from the embedded template. Placeholders are filled
// with already-validated values only; the file stays owner-writable and is
// therefore never executed by a privileged task.
func wslWriteOwnerStartScript(paths state.Paths, cfg state.WSLTargetConfig) error {
	raw, err := wslScripts.ReadFile("wslscripts/wsl-start.ps1")
	if err != nil {
		return err
	}
	text := string(raw)
	reps := map[string]string{
		"@@DISTRO@@":     cfg.Distro,
		"@@USER@@":       cfg.User,
		"@@GUESTSTATE@@": cfg.GuestState,
		"@@MESHPORT@@":   strconv.Itoa(cfg.MeshPort),
		"@@LOCALPORT@@":  strconv.Itoa(cfg.LocalPort),
	}
	for k, v := range reps {
		text = strings.ReplaceAll(text, k, v)
	}
	if strings.Contains(text, "@@") {
		return fmt.Errorf("internal error: unfilled placeholder in the WSL start script")
	}
	path := filepath.Join(paths.Root, "wsl-start.ps1")
	if fi, err := os.Lstat(path); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 || !fi.Mode().IsRegular() {
			return fmt.Errorf("refusing to replace non-regular %s: remove the link/special file first", path)
		}
	}
	return state.WriteFileAtomic(path, []byte(text), 0o600)
}

// wslEnsureLogonTask creates or replaces the owner-context logon task that
// boots the distro and starts the guest service. It uses schtasks in the
// owner's own context: no UAC, no credentials stored, least privilege. The
// action runs the owner's wsl-start.ps1 through system PowerShell; the exact
// distro/user travel inside that already-validated script, never as task argv
// the owner could later edit into a different distro.
func wslEnsureLogonTask(ctx context.Context, paths state.Paths, cfg state.WSLTargetConfig) (bool, error) {
	return wslEnsureLogonTaskIn(ctx, cfg, filepath.Join(paths.Root, "wsl-start.ps1"))
}

// wslEnsureLogonTaskIn is the testable core: scriptPath is the owner's
// wsl-start.ps1, already written by wslWriteOwnerStartScript.
func wslEnsureLogonTaskIn(ctx context.Context, cfg state.WSLTargetConfig, scriptPath string) (bool, error) {
	if fi, err := os.Lstat(scriptPath); err != nil || !fi.Mode().IsRegular() || fi.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("owner start script %s is not a regular file", scriptPath)
	}
	pwsh := wslSystemPath("WindowsPowerShell\\v1.0\\powershell.exe")
	taskArg := "-NoLogo -NoProfile -NonInteractive -ExecutionPolicy Bypass -WindowStyle Hidden -File " + quoteWinArg(scriptPath)
	out, err := wslRunHidden(ctx, wslSystemPath("schtasks.exe"), "/Query", "/TN", cfg.TaskName, "/XML")
	if err == nil {
		text := wslDecodeConsole(out)
		if !strings.Contains(text, "wsl-start.ps1") {
			return false, fmt.Errorf("unrelated scheduled task %q already exists: rename it in Task Scheduler, or re-run `messh wsl setup` with different task names", cfg.TaskName)
		}
		if _, err := wslRunHidden(ctx, wslSystemPath("schtasks.exe"), "/Delete", "/TN", cfg.TaskName, "/F"); err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "access is denied") {
				return false, withHint(fmt.Errorf("logon task %q exists but cannot be replaced without administrator rights: %v", cfg.TaskName, err),
					"re-run this exact `messh wsl setup` command from an elevated terminal to replace it")
			}
			return false, fmt.Errorf("cannot replace logon task %q: %v", cfg.TaskName, err)
		}
	}
	create := []string{"/Create", "/TN", cfg.TaskName, "/SC", "ONLOGON", "/RL", "LIMITED",
		"/TR", pwsh + " " + taskArg, "/F"}
	if out, err := wslRunHidden(ctx, wslSystemPath("schtasks.exe"), create...); err != nil {
		text := strings.TrimSpace(wslDecodeConsole(out))
		if strings.Contains(strings.ToLower(text+err.Error()), "access is denied") {
			return false, withHint(fmt.Errorf("cannot create logon task %q without administrator rights (Task Scheduler denies Network Service and standard-user creation on this device): %v %s", cfg.TaskName, err, text),
				"approve the Windows UAC prompt: re-run this exact `messh wsl setup` command from an elevated terminal, or ask an administrator to create the logon task")
		}
		return false, fmt.Errorf("cannot create logon task %q: %v %s", cfg.TaskName, err, text)
	}
	if _, err := wslRunHidden(ctx, wslSystemPath("schtasks.exe"), "/Run", "/TN", cfg.TaskName); err != nil {
		return true, withHint(fmt.Errorf("logon task %q installed but the first boot failed: %v", cfg.TaskName, err),
			fmt.Sprintf("run `schtasks /Run /TN %s` after logging on, then `messh wsl status`", cfg.TaskName))
	}
	return true, nil
}

// wslApplyMachineRouteForSetup always replaces the secured script and task so
// a newly installed binary cannot leave an older resolver active.
func wslApplyMachineRouteForSetup(ctx context.Context, c *Context, cfg state.WSLTargetConfig) (bool, bool, error) {
	return wslApplyMachineRouteElevated(ctx, c, cfg)
}

// wslApplyMachineRoute reuses the secured SYSTEM task when its admin-owned
// config is current, otherwise it installs everything through UAC. Refresh
// retains this reuse path; setup uses wslApplyMachineRouteForSetup.
func wslApplyMachineRoute(ctx context.Context, c *Context, cfg state.WSLTargetConfig) (bool, bool, error) {
	if started, err := wslTriggerRouteTask(ctx, cfg); err == nil && started {
		if ok, err := wslVerifyRoute(ctx, cfg); err == nil && ok {
			return true, true, nil
		}
		// The task ran but the route is not verifiable yet: fall through to
		// the elevated install so setup still reports an honest result.
	} else if err != nil {
		// A foreign/broken route task is a hard stop, not a fallback.
		if strings.Contains(err.Error(), "unrelated") || strings.Contains(err.Error(), "refusing") {
			return false, false, err
		}
	}
	return wslApplyMachineRouteElevated(ctx, c, cfg)
}

// wslTriggerRouteTask starts the SYSTEM route task when it exists. The owner
// may trigger it but cannot modify its action or config; the task reads only
// its administrator-owned route.json and runs only host-native networking.
func wslTriggerRouteTask(ctx context.Context, cfg state.WSLTargetConfig) (bool, error) {
	out, err := wslRunHidden(ctx, wslSystemPath("schtasks.exe"), "/Query", "/TN", cfg.RouteTaskName, "/XML")
	if err != nil {
		return false, nil
	}
	text := wslDecodeConsole(out)
	if !strings.Contains(text, "wsl-route.ps1") {
		return false, fmt.Errorf("unrelated scheduled task %q already exists: rename it in Task Scheduler, or re-run `messh wsl setup` with different task names", cfg.RouteTaskName)
	}
	if _, err := wslRunHidden(ctx, wslSystemPath("schtasks.exe"), "/Run", "/TN", cfg.RouteTaskName); err != nil {
		return false, fmt.Errorf("cannot start route task %q: %v", cfg.RouteTaskName, err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return true, nil
		case <-time.After(500 * time.Millisecond):
		}
		if ok, _ := wslVerifyRoute(ctx, cfg); ok {
			return true, nil
		}
	}
	return true, nil
}

// wslVerifyRoute requires the exact destination currently observed on the
// default WSL2 NAT adapter, not any plausible private address.
func wslVerifyRoute(ctx context.Context, cfg state.WSLTargetConfig) (bool, error) {
	out, err := wslRunHidden(ctx, wslSystemPath("netsh.exe"), "interface", "portproxy", "show", "v4tov4")
	if err != nil {
		return false, err
	}
	guestAddress, err := wslroute.ResolveGuestAddress(ctx)
	if err != nil {
		return false, err
	}
	return wslroute.PortproxyStatus(wslDecodeConsole(out), cfg.HostAddress, cfg.MeshPort, guestAddress) == "present", nil
}

// wslApplyMachineRouteElevated stages the administrator-owned ProgramData
// script/config and installs the SYSTEM route task plus the route itself
// through one visible UAC prompt. Nothing privileged reads user-writable
// state: the elevated script takes the route parameters as its own argv,
// written by this unprivileged process only into the UAC command line (never
// into a file the elevated side executes).
func wslApplyMachineRouteElevated(ctx context.Context, c *Context, cfg state.WSLTargetConfig) (bool, bool, error) {
	routeBody, err := wslScripts.ReadFile("wslscripts/wsl-route.ps1")
	if err != nil {
		return false, false, err
	}
	routeScript := append([]byte(wslroute.GuestAddressScript+"\n"), routeBody...)
	routeCfg := map[string]any{
		"host_address":  cfg.HostAddress,
		"mesh_port":     cfg.MeshPort,
		"rule_name":     state.WSLFirewallRuleName(cfg.MeshPort),
		"allowed_peers": append([]string(nil), cfg.AllowedPeers...),
	}
	cfgJSON, err := json.Marshal(routeCfg)
	if err != nil {
		return false, false, err
	}
	stageDir, err := os.MkdirTemp("", "messh-wsl-route-*")
	if err != nil {
		return false, false, err
	}
	defer os.RemoveAll(stageDir)
	scriptPath := filepath.Join(stageDir, "wsl-route.ps1")
	if err := os.WriteFile(scriptPath, routeScript, 0o600); err != nil {
		return false, false, err
	}
	cfgPath := filepath.Join(stageDir, "route.json")
	if err := os.WriteFile(cfgPath, append(cfgJSON, '\n'), 0o600); err != nil {
		return false, false, err
	}
	// The elevated Administrator process cannot reliably read the owner's Temp
	// staging, so share the same bytes through ProgramData (admins can read
	// there) and hand the elevated installer those paths.
	sharedDir := filepath.Join(os.Getenv("ProgramData"), "messh", "wsl-stage")
	if sharedDir == filepath.Join("", "messh", "wsl-stage") {
		sharedDir = `C:\ProgramData\messh\wsl-stage`
	}
	if err := os.MkdirAll(sharedDir, 0o700); err != nil {
		return false, false, err
	}
	sharedScript := filepath.Join(sharedDir, "wsl-route.ps1")
	sharedCfg := filepath.Join(sharedDir, "route.json")
	if err := os.WriteFile(sharedScript, routeScript, 0o600); err != nil {
		return false, false, err
	}
	if err := os.WriteFile(sharedCfg, append(cfgJSON, '\n'), 0o600); err != nil {
		return false, false, err
	}
	defer os.Remove(sharedScript)
	// Keep sharedCfg for the SYSTEM route task refresh path; it holds only
	// host address, mesh port, rule name and allowed peers (no secrets).
	// The admin-owned wsl/route.json remains the runtime source.
	scriptPath, cfgPath = sharedScript, sharedCfg
	installScript := wslInstallScript(cfg)
	if !c.JSON {
		fmt.Fprintln(c.Stdout, "Asking Windows for administrator rights (UAC prompt) to install the WSL host route:")
		fmt.Fprintf(c.Stdout, "  portproxy %s\n", wslProxyLabel(cfg))
		fmt.Fprintf(c.Stdout, "  firewall %s from %s on Private\n", state.WSLFirewallRuleName(cfg.MeshPort), strings.Join(cfg.AllowedPeers, ", "))
	}
	logFile, err := os.CreateTemp("", "messh-wsl-*.log")
	if err != nil {
		return false, false, err
	}
	logFile.Close()
	defer os.Remove(logFile.Name())
	sys, err := windows.GetSystemDirectory()
	if err != nil {
		return false, false, err
	}
	params := "-NoProfile -NonInteractive -EncodedCommand " + quoteWinArg(wslEncodePowerShell(installScript, scriptPath, cfgPath, logFile.Name()))
	code, err := runElevated(ctx, filepath.Join(sys, `WindowsPowerShell\v1.0\powershell.exe`), params)
	if isCancelled(err) {
		return false, false, needsPerson("cancelled at the UAC prompt; nothing changed",
			"re-run `messh wsl setup` (or `messh wsl refresh`) and approve the prompt")
	}
	if err != nil {
		return false, false, err
	}
	if code != 0 {
		lines := wslReadLog(logFile.Name())
		for _, l := range lines {
			fmt.Fprintln(c.Stderr, "  "+l)
		}
		return false, false, fmt.Errorf("elevated route install failed (exit code %d); see the output above", code)
	}
	if ok, err := wslVerifyRoute(ctx, cfg); err == nil && ok {
		return true, false, nil
	}
	return false, false, fmt.Errorf("elevated install succeeded but the route is not present: re-run `messh wsl refresh` elevated")
}

func wslReadLog(path string) []string {
	var lines []string
	out, err := os.ReadFile(path)
	if err != nil || len(out) == 0 {
		return nil
	}
	for line := range strings.Lines(wslDecodeConsole(out)) {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

// wslInstallScript returns the elevated installer: it validates its parameters,
// rejects reparse paths and insecure ownership, protects the ProgramData
// directory/files with admin-only ACLs, installs the SYSTEM route-only task
// (triggerable but not modifiable by the owner), then runs the staged route
// script once. Paths are bound only as quoted string literals by
// wslEncodePowerShell; the installer itself remains fixed privileged code.
func wslInstallScript(cfg state.WSLTargetConfig) string {
	progdata := "$env:ProgramData + '\\messh\\wsl'"
	script := "$ErrorActionPreference='Stop'; " +
		"[Console]::OutputEncoding=New-Object System.Text.UTF8Encoding($false); " +
		"function WLog([string]$m) { $m | Out-File -LiteralPath $logPath -Append -Encoding utf8 }; " +
		"foreach ($p in @($stageScript,$stageCfg)) { $it=Get-Item -LiteralPath $p -Force; " +
		"if (($it.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw ('refusing reparse staging path: ' + $p) } }; " +
		"$dest=" + progdata + "; " +
		"if (Test-Path -LiteralPath $dest) { $di=Get-Item -LiteralPath $dest -Force; " +
		"if (($di.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw ('refusing reparse destination: ' + $dest) } " +
		"if (-not $di.PSIsContainer) { throw ('refusing non-directory destination: ' + $dest) } } " +
		"else { New-Item -ItemType Directory -Path $dest -Force | Out-Null }; " +
		"$di=Get-Item -LiteralPath $dest -Force; " +
		"if (($di.Attributes -band [IO.FileAttributes]::ReparsePoint) -ne 0) { throw ('refusing reparse destination: ' + $dest) } " +
		"if (-not $di.PSIsContainer) { throw ('refusing non-directory destination: ' + $dest) }; " +
		"& icacls $dest /inheritance:r | Out-Null; " +
		"& icacls $dest /grant:r 'SYSTEM:(OI)(CI)F' 'BUILTIN\\Administrators:(OI)(CI)F' | Out-Null; " +
		"if ($LASTEXITCODE -ne 0) { throw 'cannot protect route directory' }; " +
		"$targetScript=Join-Path $dest 'wsl-route.ps1'; $targetCfg=Join-Path $dest 'route.json'; " +
		"Copy-Item -LiteralPath $stageScript -Destination $targetScript -Force; " +
		"Copy-Item -LiteralPath $stageCfg -Destination $targetCfg -Force; " +
		"& icacls $targetScript /inheritance:r /grant:r 'SYSTEM:F' 'BUILTIN\\Administrators:F' | Out-Null; " +
		"if ($LASTEXITCODE -ne 0) { throw 'cannot protect route script' }; " +
		"& icacls $targetCfg /inheritance:r /grant:r 'SYSTEM:F' 'BUILTIN\\Administrators:F' | Out-Null; " +
		"if ($LASTEXITCODE -ne 0) { throw 'cannot protect route config' }; " +
		"$taskName=" + psQuote(cfg.RouteTaskName) + "; " +
		"$pw=Join-Path $env:SystemRoot 'System32\\WindowsPowerShell\\v1.0\\powershell.exe'; " +
		"$actionArg='-NoProfile -NonInteractive -ExecutionPolicy Bypass -File ' + $targetScript; " +
		"$act=New-ScheduledTaskAction -Execute $pw -Argument $actionArg; " +
		"$prin=New-ScheduledTaskPrincipal -UserId 'SYSTEM' -LogonType ServiceAccount -RunLevel Highest; " +
		"$set=New-ScheduledTaskSettingsSet -MultipleInstances IgnoreNew; " +
		"Register-ScheduledTask -TaskName $taskName -TaskPath '\\' -Action $act -Principal $prin -Settings $set -Force | Out-Null; " +
		"WLog('installed route task ' + $taskName); " +
		"& $pw -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $targetScript 2>&1 | Out-File -LiteralPath $logPath -Append -Encoding utf8; " +
		"if ($LASTEXITCODE -ne 0) { throw 'route script failed' }; " +
		"WLog('route applied')"
	return script
}

// wslEncodePowerShell passes the fixed installer as an encoded PowerShell
// scriptblock and binds staging paths as quoted string literal parameters.
// -Command consumes its remaining command line as code, so appended paths never
// reached $args and caused powershell.exe to reject the invocation immediately.
func wslEncodePowerShell(script, stageScript, stageCfg, logPath string) string {
	command := "& { param($stageScript,$stageCfg,$logPath)\n" + script + "\n} " +
		psQuote(stageScript) + " " + psQuote(stageCfg) + " " + psQuote(logPath)
	encoded := make([]byte, 0, 2*len(command))
	for _, r := range command {
		if r <= 0xFFFF {
			encoded = append(encoded, byte(r), byte(r>>8))
			continue
		}
		hi, lo := utf16.EncodeRune(r)
		encoded = append(encoded, byte(hi), byte(hi>>8), byte(lo), byte(lo>>8))
	}
	return base64.StdEncoding.EncodeToString(encoded)
}

// quoteWinArg quotes one argv element with CommandLineToArgvW rules.
func quoteWinArg(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\"") {
		return s
	}
	var b strings.Builder
	b.WriteByte('"')
	slashes := 0
	for _, r := range s {
		switch r {
		case '\\':
			slashes++
		case '"':
			b.WriteString(strings.Repeat(`\`, slashes+1))
			slashes = 0
		default:
			slashes = 0
		}
		b.WriteRune(r)
	}
	b.WriteString(strings.Repeat(`\`, slashes))
	b.WriteByte('"')
	return b.String()
}

func isCancelled(err error) bool {
	return err != nil && errors.Is(err, windows.ERROR_CANCELLED)
}
