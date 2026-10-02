// Package state locates and manipulates messh's per-user state directory.
package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Paths resolves the files inside one state directory.
type Paths struct{ Root string }

// Resolve picks the state directory: explicit value, then $MESSH_STATE, then
// %LOCALAPPDATA%\messh on Windows or $XDG_STATE_HOME/messh (~/.local/state/messh) elsewhere.
func Resolve(explicit string) (Paths, error) {
	dir := explicit
	if dir == "" {
		dir = os.Getenv("MESSH_STATE")
	}
	if dir == "" {
		switch runtime.GOOS {
		case "windows":
			base := os.Getenv("LOCALAPPDATA")
			if base == "" {
				return Paths{}, errors.New("LOCALAPPDATA is not set")
			}
			dir = filepath.Join(base, "messh")
		default:
			base := os.Getenv("XDG_STATE_HOME")
			if base == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return Paths{}, err
				}
				base = filepath.Join(home, ".local", "state")
			}
			dir = filepath.Join(base, "messh")
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Paths{}, err
	}
	return Paths{Root: abs}, nil
}

func (p Paths) IdentityDir() string      { return filepath.Join(p.Root, "identity") }
func (p Paths) RosterFile() string       { return filepath.Join(p.Root, "peers.json") }
func (p Paths) ConfigFile() string       { return filepath.Join(p.Root, "config.json") }
func (p Paths) RunFile() string          { return filepath.Join(p.Root, "run.json") }
func (p Paths) ControlTokenFile() string { return filepath.Join(p.Root, "control.token") }
func (p Paths) AgentsDir() string        { return filepath.Join(p.Root, "agents") }
func (p Paths) RulesFile() string        { return filepath.Join(p.Root, "rules.json") }
func (p Paths) AuditFile() string        { return filepath.Join(p.Root, "audit.jsonl") }
func (p Paths) JobsDir() string          { return filepath.Join(p.Root, "jobs") }
func (p Paths) ServicesFile() string     { return filepath.Join(p.Root, "services.json") }
func (p Paths) BrowserFile() string      { return filepath.Join(p.Root, "browser.json") }
func (p Paths) BrowserDir() string       { return filepath.Join(p.Root, "browser") }
func (p Paths) SchedulesFile() string    { return filepath.Join(p.Root, "schedules.json") }

// Config is the persisted node configuration. Command-line flags override it.
type Config struct {
	Name string `json:"name"`
}

// LoadConfig returns the stored config, or a zero Config if none exists yet.
func (p Paths) LoadConfig() (Config, error) {
	var c Config
	err := readJSON(p.ConfigFile(), &c)
	if errors.Is(err, fs.ErrNotExist) {
		return Config{}, nil
	}
	return c, err
}

func (p Paths) SaveConfig(c Config) error { return writeJSON(p.ConfigFile(), c, 0o600) }

// RunInfo is written by a running node so CLI commands can find it.
type RunInfo struct {
	PID        int       `json:"pid"`
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Mesh       string    `json:"mesh"`
	Local      string    `json:"local"`
	Started    time.Time `json:"started"`
	Executable string    `json:"executable,omitempty"`
}

// ErrNodeNotRunning means run.json is missing or its node cannot be reached.
var ErrNodeNotRunning = errors.New("no running messh node")

func (p Paths) LoadRunInfo() (RunInfo, error) {
	var r RunInfo
	err := readJSON(p.RunFile(), &r)
	if errors.Is(err, fs.ErrNotExist) {
		return r, fmt.Errorf("%w for state dir %s (start one with `messh node`): %w", ErrNodeNotRunning, p.Root, err)
	}
	return r, err
}

func (p Paths) SaveRunInfo(r RunInfo) error { return writeJSON(p.RunFile(), r, 0o600) }

func (p Paths) RemoveRunInfo() error {
	err := os.Remove(p.RunFile())
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// ControlToken returns the token that authorizes CLI calls to the local node,
// creating it on first use.
func (p Paths) ControlToken() (string, error) {
	return readOrCreateToken(p.ControlTokenFile())
}

// WriteFileAtomic durably replaces path with data using a synced temporary file,
// an atomic platform replacement, and a parent-directory sync where supported.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = tmp.Close(); _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		cleanup()
		return fmt.Errorf("set temporary file permissions: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := replaceFile(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("replace %s: %w", path, err)
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync directory for %s: %w", path, err)
	}
	return nil
}
func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func writeJSON(path string, v any, perm os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(path, append(data, '\n'), perm)
}

func readOrCreateToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		tok := strings.TrimSpace(string(data))
		if tok != "" {
			return tok, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	tok, err := newToken()
	if err != nil {
		return "", err
	}
	if err := WriteFileAtomic(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

func newToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
