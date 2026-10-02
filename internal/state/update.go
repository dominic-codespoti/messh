package state

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// UpdateStartup is the existing node's nonrenewable lease carried across a
// binary replacement. It changes admission only, never launcher arguments or
// user configuration.
type UpdateStartup struct {
	Lease      string    `json:"lease"`
	Expires    time.Time `json:"expires"`
	ID         string    `json:"id"`
	Executable string    `json:"executable"`
}

const UpdateStartupDuration = 2 * time.Minute

var (
	ErrUpdateStartupChanged = errors.New("update startup transaction changed")
	ErrUpdateStartupExpired = errors.New("update startup transaction expired")
)

func (p Paths) UpdateStartupFile() string { return filepath.Join(p.Root, "update-startup.json") }

// Validate checks the identity before considering expiry: a foreign marker
// must never turn into permission to start accepting work merely by expiring.
func (m UpdateStartup) Validate(id, executable string, now time.Time) error {
	token, err := hex.DecodeString(m.Lease)
	if err != nil || len(token) != 32 || m.ID == "" || !filepath.IsAbs(m.Executable) {
		return errors.New("invalid update startup transaction")
	}
	if m.ID != id || m.Executable != executable {
		return errors.New("update startup node identity or executable mismatch")
	}
	if m.Expires.IsZero() || m.Expires.After(now.Add(UpdateStartupDuration)) {
		return errors.New("update startup expiry exceeds the two-minute lease")
	}
	if !now.Before(m.Expires) {
		return ErrUpdateStartupExpired
	}
	return nil
}

// LoadUpdateStartup returns nil when no update transaction is pending.
// Callers must Validate the result against the actual node before using it.
func (p Paths) LoadUpdateStartup() (*UpdateStartup, error) {
	info, err := os.Lstat(p.UpdateStartupFile())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, errors.New("update startup marker must be a private regular file")
	}
	var marker UpdateStartup
	if err := readJSON(p.UpdateStartupFile(), &marker); err != nil {
		return nil, err
	}
	return &marker, nil
}

// SaveUpdateStartup publishes a private atomic marker before the old node is
// stopped. Existing transactions are never overwritten, including expired ones.
func (p Paths) SaveUpdateStartup(marker UpdateStartup, now time.Time) error {
	if err := marker.Validate(marker.ID, marker.Executable, now); err != nil {
		return err
	}
	unlock, err := p.lockUpdateStartup()
	if err != nil {
		return err
	}
	defer unlock()
	old, err := p.LoadUpdateStartup()
	if err != nil {
		return err
	}
	if old != nil {
		return ErrUpdateStartupChanged
	}
	return writeJSON(p.UpdateStartupFile(), marker, 0o600)
}

// RemoveUpdateStartup removes only the exact transaction observed by its
// caller. The OS-released lock covers comparison and removal across processes;
// an updater crash cannot leave a permanent lock or remove its successor.
func (p Paths) RemoveUpdateStartup(expected UpdateStartup) error {
	unlock, err := p.lockUpdateStartup()
	if err != nil {
		return err
	}
	defer unlock()
	actual, err := p.LoadUpdateStartup()
	if err != nil || actual == nil {
		return err
	}
	if actual.Lease != expected.Lease || actual.ID != expected.ID ||
		actual.Executable != expected.Executable || !actual.Expires.Equal(expected.Expires) {
		return ErrUpdateStartupChanged
	}
	return os.Remove(p.UpdateStartupFile())
}

// HoldUpdateStartup prevents release of a validated transaction while its
// launcher is being stopped for recovery. The caller must release the hold
// when that bounded operation ends. Reads remain available to startup/status.
func (p Paths) HoldUpdateStartup(expected UpdateStartup) (func(), error) {
	unlock, err := p.lockUpdateStartup()
	if err != nil {
		return nil, err
	}
	actual, err := p.LoadUpdateStartup()
	if err != nil {
		unlock()
		return nil, err
	}
	if actual == nil || actual.Lease != expected.Lease || actual.ID != expected.ID ||
		actual.Executable != expected.Executable || !actual.Expires.Equal(expected.Expires) {
		unlock()
		return nil, ErrUpdateStartupChanged
	}
	if err := actual.Validate(expected.ID, expected.Executable, time.Now()); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

func (p Paths) lockUpdateStartup() (func(), error) {
	if err := os.MkdirAll(p.Root, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(p.Root, ".update-startup.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := lockUpdateStartupFile(file); err != nil {
		file.Close()
		return nil, fmt.Errorf("lock update startup transaction: %w", err)
	}
	return func() { file.Close() }, nil
}
