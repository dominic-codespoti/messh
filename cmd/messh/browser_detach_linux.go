//go:build linux

package main

import (
	"os/exec"
	"syscall"
)

// detach lets the browser window outlive an interrupted CLI.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
