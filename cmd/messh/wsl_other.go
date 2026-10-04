//go:build !windows

package main

import (
	"context"
	"errors"
	"time"

	"messh/internal/state"
)

func newWSLContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), timeout)
}

func wslPlanSetup(ctx context.Context, paths state.Paths, cfg state.WSLTargetConfig, hostExplicit bool) (state.WSLTargetConfig, error) {
	_ = ctx
	_ = paths
	_ = cfg
	_ = hostExplicit
	return state.WSLTargetConfig{}, errors.New("`messh wsl setup` runs only on the Windows host: run it there to capture the WSL target, then inspect it from here with `messh wsl status WINDOWS_DEVICE`")
}

func wslPlanRefresh(ctx context.Context, cfg state.WSLTargetConfig) (state.WSLTargetConfig, string, bool, error) {
	_ = ctx
	_ = cfg
	return state.WSLTargetConfig{}, "", false, errors.New("`messh wsl refresh` runs only on the Windows host: run it there after a DHCP move or reboot, then inspect it from here with `messh wsl status WINDOWS_DEVICE`")
}

func wslWriteOwnerStartScript(paths state.Paths, cfg state.WSLTargetConfig) error {
	_ = paths
	_ = cfg
	return errors.New("WSL host routing is Windows-only")
}

func wslEnsureLogonTask(ctx context.Context, paths state.Paths, cfg state.WSLTargetConfig) (bool, error) {
	_ = ctx
	_ = paths
	_ = cfg
	return false, errors.New("WSL host routing is Windows-only")
}

func wslApplyMachineRoute(ctx context.Context, c *Context, cfg state.WSLTargetConfig) (bool, bool, error) {
	_ = ctx
	_ = c
	_ = cfg
	return false, false, errors.New("WSL host routing is Windows-only")
}
