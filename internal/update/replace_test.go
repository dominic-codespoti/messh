package update

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func candidate(t *testing.T, executable, contents string) string {
	t.Helper()
	dir, err := privateStageDir(filepath.Dir(executable))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	name := filepath.Join(dir, filepath.Base(executable))
	if err := os.WriteFile(name, []byte(contents), 0700); err != nil {
		t.Fatal(err)
	}
	return name
}

func expectFile(t *testing.T, name, expected string) {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil || string(data) != expected {
		t.Fatalf("%s = %q, %v; want %q", name, data, err, expected)
	}
}

func TestSwapRetainsOnePreviousAndRollbackRestores(t *testing.T) {
	executable := installed(t)
	replacement, err := Swap(executable, candidate(t, executable, "second"))
	if err != nil {
		t.Fatal(err)
	}
	expectFile(t, executable, "second")
	expectFile(t, replacement.Backup, "old-executable")
	if err := replacement.Commit(); err != nil {
		t.Fatal(err)
	}
	replacement, err = Swap(executable, candidate(t, executable, "third"))
	if err != nil {
		t.Fatal(err)
	}
	expectFile(t, executable, "third")
	expectFile(t, replacement.Backup, "second")
	if err := replacement.Rollback(); err != nil {
		t.Fatal(err)
	}
	expectFile(t, executable, "second")
	if _, err := os.Stat(replacement.Backup); !os.IsNotExist(err) {
		t.Fatalf("rollback left duplicate backup: %v", err)
	}
	if err := replacement.Rollback(); err != nil {
		t.Fatal(err)
	}
}

func TestSwapRestoresOriginalAfterPartialFailure(t *testing.T) {
	executable := installed(t)
	staged := candidate(t, executable, "new")
	injected := errors.New("simulated rename failure")
	_, err := swapWithRename(executable, staged, func(old, new string) error {
		if old == staged && new == executable {
			return injected
		}
		return os.Rename(old, new)
	})
	if !errors.Is(err, injected) {
		t.Fatalf("wrong failure: %v", err)
	}
	expectFile(t, executable, "old-executable")
	expectFile(t, staged, "new")
}

func TestConcurrentUpdatesRefusedAndLockReusable(t *testing.T) {
	executable := installed(t)
	unlock, err := AcquireLock(executable)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	second, err := AcquireLock(executable)
	if err == nil {
		second()
		t.Fatal("concurrent updater acquired lock")
	}
	if !errors.Is(err, ErrLocked) {
		t.Fatalf("wrong lock error: %v", err)
	}
	unlock()
	unlock()
	second, err = AcquireLock(executable)
	if err != nil {
		t.Fatalf("released lock stayed locked: %v", err)
	}
	second()
}

func TestLockProcessExit(t *testing.T) {
	if path := os.Getenv("MESSH_UPDATE_LOCK_CHILD"); path != "" {
		if _, err := AcquireLock(path); err != nil {
			os.Exit(24)
		}
		// Abrupt exit without invoking the unlock closure simulates a crash.
		os.Exit(23)
	}
	executable := installed(t)
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(testBinary, "-test.run=^TestLockProcessExit$")
	command.Env = append(os.Environ(), "MESSH_UPDATE_LOCK_CHILD="+executable)
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("lock subprocess: %s, %v", output, err)
	}
	unlock, err := AcquireLock(executable)
	if err != nil {
		t.Fatalf("crashed process stranded lock: %v", err)
	}
	unlock()
}

func TestExecutableSymlinkRefused(t *testing.T) {
	executable := installed(t)
	link := filepath.Join(filepath.Dir(executable), "alias.exe")
	if err := os.Symlink(executable, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if unlock, err := AcquireLock(link); err == nil {
		unlock()
		t.Fatal("accepted symlink executable")
	}
	if _, err := Swap(link, candidate(t, executable, "new")); err == nil {
		t.Fatal("replaced symlink executable")
	}
	expectFile(t, executable, "old-executable")
}
