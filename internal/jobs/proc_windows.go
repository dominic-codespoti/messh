//go:build windows

package jobs

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	createSuspended      = 0x00000004
	createNewProcessGrp  = 0x00000200
	createNoWindow       = 0x08000000
	processAccessForJobs = windows.PROCESS_SET_QUOTA | windows.PROCESS_TERMINATE | windows.PROCESS_QUERY_LIMITED_INFORMATION
)

// winProc owns a Job Object holding the child tree. The object is created
// with KILL_ON_JOB_CLOSE, so the tree also dies if the node does.
type winProc struct {
	cmd   *exec.Cmd
	token string
	notes []string

	mu  sync.Mutex
	job windows.Handle // 0 once closed
}

func startProcess(s launchSpec) (process, error) {
	cmd := exec.Command(s.Path, s.Args...)
	cmd.Dir, cmd.Env = s.Dir, s.Env
	cmd.Stdout, cmd.Stderr = s.Stdout, s.Stderr
	attr := &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: createSuspended | createNewProcessGrp | createNoWindow,
	}
	if s.Shell {
		// /S makes cmd strip exactly the outer quotes and run the rest verbatim.
		attr.CmdLine = `"` + s.Path + `" /S /C "` + s.Line + `"`
	}
	cmd.SysProcAttr = attr

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create job object: %w", err)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if s.MemLimitMB > 0 {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_JOB_MEMORY
		info.JobMemoryLimit = uintptr(s.MemLimitMB) << 20
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, fmt.Errorf("configure job object: %w", err)
	}

	// Start suspended and join the job before the first instruction runs, so
	// no descendant can be created outside it.
	if err := cmd.Start(); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	p := &winProc{cmd: cmd, job: job}
	ph, err := windows.OpenProcess(processAccessForJobs, false, uint32(cmd.Process.Pid))
	if err == nil {
		err = windows.AssignProcessToJobObject(job, ph)
		if err == nil {
			p.token, _ = processToken(ph)
		}
		windows.CloseHandle(ph)
	}
	if err == nil {
		err = resumeMainThread(uint32(cmd.Process.Pid))
	}
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		windows.CloseHandle(job)
		return nil, fmt.Errorf("attach process to job object: %w", err)
	}
	if s.MemLimitMB > 0 {
		p.notes = append(p.notes, fmt.Sprintf("mem_mb=%d enforced as a Windows job memory limit", s.MemLimitMB))
	}
	return p, nil
}

func processToken(h windows.Handle) (string, error) {
	var created, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exit, &kernel, &user); err != nil {
		return "", err
	}
	return strconv.FormatUint(uint64(created.HighDateTime)<<32|uint64(created.LowDateTime), 10), nil
}

func resumeMainThread(pid uint32) error {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snap)
	te := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	resumed := 0
	for err = windows.Thread32First(snap, &te); err == nil; err = windows.Thread32Next(snap, &te) {
		if te.OwnerProcessID != pid {
			continue
		}
		th, oerr := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, te.ThreadID)
		if oerr != nil {
			return oerr
		}
		_, rerr := windows.ResumeThread(th)
		windows.CloseHandle(th)
		if rerr != nil {
			return rerr
		}
		resumed++
	}
	if resumed == 0 {
		return errors.New("no thread to resume")
	}
	return nil
}

func (p *winProc) PID() int        { return p.cmd.Process.Pid }
func (p *winProc) Token() string   { return p.token }
func (p *winProc) Unit() string    { return "" }
func (p *winProc) Notes() []string { return p.notes }
func (p *winProc) Terminate()      { p.Kill() } // console-less processes have no graceful signal
func (p *winProc) Kill()           { p.closeJob() }

// closeJob terminates every process in the job and releases the handle.
func (p *winProc) closeJob() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.job == 0 {
		return
	}
	windows.TerminateJobObject(p.job, 1)
	windows.CloseHandle(p.job)
	p.job = 0
}

func (p *winProc) Wait() (int, error) {
	err := p.cmd.Wait()
	p.closeJob() // reap anything the job left running
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

// killLeftover ends a process a previous node instance started, if the PID
// still belongs to it. The job object already killed the tree when that node
// died; this covers an orphan that outlived it.
func killLeftover(pid int, token, unit string) {
	if pid <= 0 || token == "" {
		return
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(h)
	if t, err := processToken(h); err == nil && t == token {
		windows.TerminateProcess(h, 1)
	}
}
