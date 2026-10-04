//go:build windows

package node

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"messh/internal/state"
)

// probeDesktopTargets is the Windows inventory: fixed no-start wsl.exe
// distro lists, one netsh portproxy read, selected-interface facts from
// ifaceAddrs, and bounded host-local TCP observations. The argv below is
// fixed; nothing from the config, the network, or the caller can steer it.
// In particular this never runs wsl --exec, which could boot a stopped
// guest or race its shutdown.
func probeDesktopTargets(ctx context.Context, cfg *state.WSLTargetConfig) desktopFacts {
	var f desktopFacts
	distros, errAll := runWSLQuiet(ctx, "--list", "--quiet")
	running, errRunning := runWSLQuiet(ctx, "--list", "--running", "--quiet")
	switch {
	case errAll != nil:
		f.InvErr = "wsl --list --quiet failed: " + errAll.Error()
		// A failed first inventory proves nothing: leave Running empty even
		// if the second call succeeded, so classify reports unknown rather
		// than a half-observed state.
	case errRunning != nil:
		f.InvErr = "wsl --list --running --quiet failed: " + errRunning.Error()
	default:
		f.Distros, f.Running = distros, running
	}
	name, current, err := desktopIfaceAddrs(cfg.HostInterfaceIndex)
	if err != nil {
		f.IfaceErr = err.Error()
	} else {
		f.IfaceName, f.Current = name, current
	}
	f.PortProxy = desktopPortproxyLabel(ctx, cfg.MeshPort)
	if desktopValidPort(cfg.MeshPort) && cfg.HostAddress != "" {
		f.LoopTarget = net.JoinHostPort(cfg.HostAddress, strconv.Itoa(cfg.MeshPort))
		f.Loopback = desktopDialLabel(ctx, f.LoopTarget)
		lan := cfg.HostAddress
		if lan != "" && desktopValidPort(cfg.MeshPort) {
			f.LANTarget = net.JoinHostPort(lan, strconv.Itoa(cfg.MeshPort))
			f.LAN = desktopDialLabel(ctx, f.LANTarget)
		}
	}
	return f
}

// runWSLQuiet runs one fixed no-start inventory and parses its lines. The
// fixed wsl.exe argv is the whole surface: this never executes inside a
// guest (--exec is banned: it can boot a stopped distribution).
func runWSLQuiet(ctx context.Context, argv ...string) ([]string, error) {
	out, err := runHiddenNoWindow(ctx, "wsl.exe", argv...)
	if err != nil {
		return nil, err
	}
	return parseWSLQuietList(out)
}

// portproxyLabel reports whether the expected v4tov4 listen on the LAN mesh
// port to 127.0.0.1 exists (present), is missing (absent), or the netsh read
// itself failed (unknown — not evidence either way).
func desktopPortproxyLabel(ctx context.Context, meshPort int) string {
	if !desktopValidPort(meshPort) {
		return "unknown"
	}
	out, err := runHiddenNoWindow(ctx, "netsh.exe", "interface", "portproxy", "show", "v4tov4")
	if err != nil {
		return "unknown"
	}
	return classifyDesktopPortproxy(string(out), meshPort)
}

// runHiddenNoWindow runs a console program with no window flash and returns
// its stdout. It is the same shape as the established netcheck helper,
// local to this file so the diagnostics stay self-contained.
func runHiddenNoWindow(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	cmd.WaitDelay = time.Second
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.Bytes(), fmt.Errorf("%s %s: %v %s", name, strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.Bytes(), nil
}
