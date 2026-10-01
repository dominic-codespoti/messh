//go:build windows

package browser

import (
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	createSuspended = 0x00000004
	createNoWindow  = 0x08000000
)

// winTree owns a Job Object holding the whole tree. KILL_ON_JOB_CLOSE makes
// Windows end every member if the node dies without cleaning up.
type winTree struct {
	cmd   *exec.Cmd
	token string

	mu  sync.Mutex
	job windows.Handle // 0 once closed
}

func startTree(s treeSpec) (tree, error) {
	cmd := exec.Command(s.Path, s.Args...)
	cmd.Dir, cmd.Env = s.Dir, s.Env
	cmd.Stdout, cmd.Stderr = s.Output, s.Output
	cmd.WaitDelay = killWait
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createSuspended | createNoWindow}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("create job object: %w", err)
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
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
	t := &winTree{cmd: cmd, job: job}
	ph, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(cmd.Process.Pid))
	if err == nil {
		err = windows.AssignProcessToJobObject(job, ph)
		if err == nil {
			t.token = processToken(ph)
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
	return t, nil
}

func processToken(h windows.Handle) string {
	var created, exit, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(h, &created, &exit, &kernel, &user); err != nil {
		return ""
	}
	return strconv.FormatUint(uint64(created.HighDateTime)<<32|uint64(created.LowDateTime), 10)
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

func (t *winTree) PID() int      { return t.cmd.Process.Pid }
func (t *winTree) Token() string { return t.token }

func (t *winTree) Kill() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.job == 0 {
		return
	}
	windows.TerminateJobObject(t.job, 1)
	windows.CloseHandle(t.job)
	t.job = 0
}

func (t *winTree) Wait() error {
	err := t.cmd.Wait()
	t.Kill() // reap anything the tree left running
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return nil
	}
	return err
}

// killLeftover is a no-op on Windows: the job object dies with the node that
// created it, taking the tree along.
func killLeftover(pid int, token string) {}

// browserRunning reports whether a process with one of the executable names
// (for example "chrome.exe") is running in this session.
func browserRunning(names ...string) bool {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false
	}
	defer windows.CloseHandle(snap)
	pe := windows.ProcessEntry32{Size: uint32(unsafe.Sizeof(windows.ProcessEntry32{}))}
	for err = windows.Process32First(snap, &pe); err == nil; err = windows.Process32Next(snap, &pe) {
		exe := windows.UTF16ToString(pe.ExeFile[:])
		for _, n := range names {
			if strings.EqualFold(exe, n) {
				return true
			}
		}
	}
	return false
}
