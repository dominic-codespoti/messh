//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// detach lets the browser window outlive an interrupted CLI.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000200} // CREATE_NEW_PROCESS_GROUP
}
