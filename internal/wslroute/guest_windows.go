//go:build windows

package wslroute

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	guestAddressInvocation = "\ntry { Get-MesshWSLGuestAddress } catch { [Console]::Error.WriteLine($_.Exception.Message); exit 1 }"
	guestAddressPowerShell = GuestAddressScript + guestAddressInvocation
)

// ResolveGuestAddress queries the Windows host's default WSL2 NAT neighbor
// table. It never starts or executes a distro and never reads user state.
func ResolveGuestAddress(ctx context.Context) (string, error) {
	root := os.Getenv("SystemRoot")
	if root == "" {
		return "", fmt.Errorf("SystemRoot is not set")
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	powershell := filepath.Join(root, "System32", "WindowsPowerShell", "v1.0", "powershell.exe")
	cmd := exec.CommandContext(ctx, powershell, "-NoProfile", "-NonInteractive", "-Command", guestAddressPowerShell)
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	cmd.WaitDelay = time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("resolve WSL guest address: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	address := strings.TrimSpace(stdout.String())
	if !IsGuestIPv4(address) {
		return "", fmt.Errorf("resolver returned invalid WSL guest IPv4 address %q", address)
	}
	return address, nil
}
