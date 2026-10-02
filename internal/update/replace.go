package update

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// ErrLocked means another updater owns this installed executable.
var ErrLocked = errors.New("another update is already in progress")

func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// Refuse symlinks, including ancestor aliases: replacing a link or updating a
// different target than the service uses would violate the installed-path contract.
func executablePath(name string) (string, error) {
	absolute, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("update executable must be a regular file, not a symlink")
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	if !samePath(absolute, resolved) {
		return "", errors.New("update executable path must not contain symlinks")
	}
	return absolute, nil
}

// AcquireLock holds a nonblocking OS process lock. The marker deliberately stays
// on disk; unlinking it would let two processes lock different inodes. The OS
// releases ownership when a process exits, including after a crash.
func AcquireLock(executable string) (func(), error) {
	executable, err := executablePath(executable)
	if err != nil {
		return nil, err
	}
	lockPath := executable + ".update.lock"
	if info, err := os.Lstat(lockPath); err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("update lock is not a regular file")
	} else if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := lockFile(file); err != nil {
		file.Close()
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { unlockFile(file); file.Close() }) }, nil
}

// Replacement retains the old executable at Backup until a later successful
// update replaces it. Callers hold AcquireLock through health verification.
type Replacement struct {
	Executable string
	Backup     string
	staged     string
	rename     func(string, string) error
	finished   bool
}

func Swap(executable, staged string) (*Replacement, error) {
	return swapWithRename(executable, staged, os.Rename)
}

func swapWithRename(executable, staged string, rename func(string, string) error) (*Replacement, error) {
	executable, err := executablePath(executable)
	if err != nil {
		return nil, err
	}
	staged, err = executablePath(staged)
	if err != nil {
		return nil, err
	}
	if samePath(executable, staged) {
		return nil, errors.New("staged executable is the installed executable")
	}
	parent := filepath.Dir(executable)
	if !samePath(filepath.Dir(staged), parent) && !samePath(filepath.Dir(filepath.Dir(staged)), parent) {
		return nil, errors.New("staged executable must be beside the installed executable")
	}
	backup := executable + ".previous"
	if samePath(staged, backup) {
		return nil, errors.New("cannot stage from the retained backup")
	}
	if info, err := os.Lstat(backup); err == nil {
		if !info.Mode().IsRegular() {
			return nil, errors.New("previous executable is not a regular file")
		}
		if err := os.Remove(backup); err != nil {
			return nil, fmt.Errorf("remove prior backup: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := rename(executable, backup); err != nil {
		return nil, fmt.Errorf("retain installed executable: %w", err)
	}
	if err := rename(staged, executable); err != nil {
		if rollbackErr := rename(backup, executable); rollbackErr != nil {
			return nil, errors.Join(fmt.Errorf("install staged executable: %w", err), fmt.Errorf("restore installed executable from %s: %w", backup, rollbackErr))
		}
		return nil, fmt.Errorf("install staged executable (original restored): %w", err)
	}
	return &Replacement{Executable: executable, Backup: backup, staged: staged, rename: rename}, nil
}

func (r *Replacement) Rollback() error {
	if r == nil || r.finished {
		return nil
	}
	// Move the failed new image aside first, so rollback also works on Windows
	// where replacing a running mapped executable directly is not permitted.
	if err := r.rename(r.Executable, r.staged); err != nil {
		return fmt.Errorf("move failed executable aside: %w", err)
	}
	if err := r.rename(r.Backup, r.Executable); err != nil {
		return errors.Join(fmt.Errorf("restore retained executable: %w", err), r.rename(r.staged, r.Executable))
	}
	r.finished = true
	if err := os.Remove(r.staged); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (r *Replacement) Commit() error {
	if r == nil || r.finished {
		return nil
	}
	r.finished = true
	return nil
}
