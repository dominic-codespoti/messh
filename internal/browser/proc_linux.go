//go:build linux

package browser

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// linuxTree runs the upstream as the leader of its own process group, so one
// signal reaches node, npm and the browser.
type linuxTree struct {
	cmd   *exec.Cmd
	token string
}

func startTree(s treeSpec) (tree, error) {
	cmd := exec.Command(s.Path, s.Args...)
	cmd.Dir, cmd.Env = s.Dir, s.Env
	cmd.Stdout, cmd.Stderr = s.Output, s.Output
	cmd.WaitDelay = killWait
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &linuxTree{cmd: cmd, token: procToken(cmd.Process.Pid)}, nil
}

// procToken is the process start time in clock ticks since boot
// (field 22 of /proc/<pid>/stat).
func procToken(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')') // comm may contain spaces and parens
	if i < 0 {
		return ""
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 20 {
		return ""
	}
	return f[19]
}

func (t *linuxTree) PID() int      { return t.cmd.Process.Pid }
func (t *linuxTree) Token() string { return t.token }

func (t *linuxTree) Kill() { syscall.Kill(-t.cmd.Process.Pid, syscall.SIGKILL) }

func (t *linuxTree) Wait() error {
	err := t.cmd.Wait()
	t.Kill() // reap descendants the main process left behind
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return nil
	}
	return err
}

// killLeftover ends the process group a previous node left behind (a crash
// would otherwise leave the browser holding the profile). The group is only
// signalled while its leader still has the recorded start time.
func killLeftover(pid int, token string) {
	if pid > 1 && token != "" && procToken(pid) == token {
		syscall.Kill(-pid, syscall.SIGKILL)
	}
}

// browserRunning reports whether a process with one of the given command
// names (for example "chrome") is running.
func browserRunning(names ...string) bool {
	entries, err := filepath.Glob("/proc/[0-9]*/comm")
	if err != nil {
		return false
	}
	for _, e := range entries {
		b, err := os.ReadFile(e)
		if err != nil {
			continue
		}
		comm := strings.TrimSpace(string(b))
		for _, n := range names {
			if comm == n {
				return true
			}
		}
	}
	return false
}
