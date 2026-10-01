//go:build linux

package jobs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// linuxProc runs the job as the leader of its own process group, usually
// inside a transient systemd user scope so descendants that call setsid
// still stay inside one cgroup that can be killed and memory-limited.
type linuxProc struct {
	cmd    *exec.Cmd
	token  string
	unit   string
	notes  []string
	grace  time.Duration
	exited chan struct{}
	once   sync.Once
}

// systemd probing is slow-ish and static for the node's lifetime.
var (
	scopeOnce sync.Once
	scopeBin  string // systemd-run path, empty when scopes do not work
	scopeMem  bool   // MemoryMax is accepted for user scopes
)

func probeScope() {
	exe, err := exec.LookPath("systemd-run")
	if err != nil {
		return
	}
	try := func(extra ...string) bool {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		args := append([]string{"--user", "--scope", "--quiet", "--collect"}, extra...)
		args = append(args, "--", "true")
		return exec.CommandContext(ctx, exe, args...).Run() == nil
	}
	if try() {
		scopeBin = exe
		scopeMem = try("-p", "MemoryMax=512M")
	}
}

func startProcess(s launchSpec) (process, error) {
	argv := append([]string{s.Path}, s.Args...)
	if s.Shell {
		argv = []string{s.Path, "-c", s.Line}
	}
	p := &linuxProc{grace: s.KillGrace, exited: make(chan struct{})}
	if p.grace <= 0 {
		p.grace = 5 * time.Second
	}

	var cmd *exec.Cmd
	if !s.NoScope {
		scopeOnce.Do(probeScope)
	}
	if scopeBin != "" && !s.NoScope {
		p.unit = "messh-job-" + s.ID
		pre := []string{"--user", "--scope", "--quiet", "--collect", "--unit=" + p.unit}
		if s.MemLimitMB > 0 {
			if scopeMem {
				pre = append(pre, "-p", fmt.Sprintf("MemoryMax=%dM", s.MemLimitMB), "-p", "MemorySwapMax=0")
				p.notes = append(p.notes, fmt.Sprintf("mem_mb=%d enforced as systemd MemoryMax", s.MemLimitMB))
			} else {
				p.notes = append(p.notes, "mem_mb is a scheduling claim only: this systemd user manager does not delegate the memory controller")
			}
		}
		cmd = exec.Command(scopeBin, append(append(pre, "--"), argv...)...)
	} else {
		cmd = exec.Command(argv[0], argv[1:]...)
		if s.MemLimitMB > 0 {
			p.notes = append(p.notes, "mem_mb is a scheduling claim only: systemd-run is not available for a user scope")
		}
	}
	cmd.Dir, cmd.Env = s.Dir, s.Env
	cmd.Stdout, cmd.Stderr = s.Stdout, s.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	p.cmd = cmd
	p.token = procToken(cmd.Process.Pid)
	return p, nil
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
	return f[19] // field 22 overall; the first two (pid, comm) precede f[0]=state
}

func (p *linuxProc) PID() int        { return p.cmd.Process.Pid }
func (p *linuxProc) Token() string   { return p.token }
func (p *linuxProc) Unit() string    { return p.unit }
func (p *linuxProc) Notes() []string { return p.notes }

func (p *linuxProc) signalGroup(sig syscall.Signal) {
	syscall.Kill(-p.cmd.Process.Pid, sig)
}

func (p *linuxProc) Terminate() {
	p.signalGroup(syscall.SIGTERM)
	go func() {
		t := time.NewTimer(p.grace)
		defer t.Stop()
		select {
		case <-p.exited:
		case <-t.C:
			p.Kill()
		}
	}()
}

func (p *linuxProc) Kill() {
	p.signalGroup(syscall.SIGKILL)
	stopUnit(p.unit)
}

func (p *linuxProc) Wait() (int, error) {
	err := p.cmd.Wait()
	p.once.Do(func() { close(p.exited) })
	p.Kill() // reap descendants the job left behind
	code := -1
	if ps := p.cmd.ProcessState; ps != nil {
		code = ps.ExitCode()
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		err = nil
	}
	return code, err
}

func stopUnit(unit string) {
	if unit == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exec.CommandContext(ctx, "systemctl", "--user", "kill", "--kill-whom=all", "--signal=SIGKILL", unit+".scope").Run()
}

// killLeftover ends a job a previous node instance started. The group is
// only signalled if the leader's PID still has the recorded start time.
func killLeftover(pid int, token, unit string) {
	stopUnit(unit)
	if pid > 1 && token != "" && procToken(pid) == token {
		syscall.Kill(-pid, syscall.SIGKILL)
		syscall.Kill(pid, syscall.SIGKILL)
	}
}
