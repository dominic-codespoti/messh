package browser

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"messh/internal/state"
)

// PinnedVersion is the @playwright/mcp release this code was written and
// tested against. The published tool schemas (tools.json) come from it; a
// different version still runs but status reports the mismatch.
const PinnedVersion = "0.0.83"

// Modes of operation.
const (
	// ModeProfile drives a dedicated browser profile only agents use.
	ModeProfile = "profile"
	// ModeExtension attaches to the owner's running browser through the
	// Playwright browser extension, using their logged-in sessions.
	ModeExtension = "extension"
)

// Defaults.
const (
	DefaultIdleTimeout = 15 * time.Minute
	DefaultChannel     = "chrome"
)

// Config is browser.json. A missing file or enabled:false publishes no
// browser tools at all.
type Config struct {
	Enabled bool   `json:"enabled"`
	Mode    string `json:"mode,omitempty"`    // "profile" (default) or "extension"
	Channel string `json:"channel,omitempty"` // "chrome" (default) or "msedge"
	// Executable overrides the browser binary (Chromium, Brave, a portable
	// Chrome); the channel then only selects the profile layout.
	Executable string `json:"executable,omitempty"`
	// ProfileDir is the agent profile directory in profile mode (default
	// <state>/browser/profile) and the browser profile folder name
	// ("Profile 1") in extension mode (default: the last used profile).
	ProfileDir string `json:"profile_dir,omitempty"`
	// Command starts Playwright MCP: the program followed by its arguments.
	// Default: npx -y @playwright/mcp@<PinnedVersion>. A JSON string is split
	// on spaces (double quotes group); an array is used as is.
	Command Command `json:"command,omitempty"`
	// Headless hides the agent browser (profile mode only).
	Headless bool `json:"headless,omitempty"`
	// IdleTimeout stops the browser after this long without a call, e.g. "15m";
	// "0" disables. Default 15m.
	IdleTimeout string `json:"idle_timeout,omitempty"`
	// AllowOrigins are websites whose pages may be viewed without asking.
	// Interacting still asks. Entries: [scheme://]host[:port], host may start
	// with "*." to cover every subdomain (not the bare domain).
	AllowOrigins []string `json:"allow_origins,omitempty"`
	// DenyOrigins are never reachable, even when approved elsewhere.
	DenyOrigins []string `json:"deny_origins,omitempty"`
	// AllowScript publishes browser_evaluate (page JavaScript).
	AllowScript bool `json:"allow_script,omitempty"`
	// ExtensionToken is the PLAYWRIGHT_MCP_EXTENSION_TOKEN shown on the
	// extension's status page. With it the browser connects without the
	// owner approving each session; without it they pick the tab every time.
	ExtensionToken string `json:"extension_token,omitempty"`
}

// Command is a program with arguments that accepts a JSON string or array.
type Command []string

// UnmarshalJSON implements json.Unmarshaler.
func (c *Command) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		argv, err := SplitCommand(s)
		*c = argv
		return err
	}
	var arr []string
	if err := json.Unmarshal(b, &arr); err != nil {
		return errors.New(`"command" must be a string or an array of strings`)
	}
	*c = arr
	return nil
}

// SplitCommand splits a command line on spaces; double quotes group words.
func SplitCommand(s string) ([]string, error) {
	var out []string
	var cur strings.Builder
	inQuote, have := false, false
	for _, r := range s {
		switch {
		case r == '"':
			inQuote, have = !inQuote, true
		case (r == ' ' || r == '\t') && !inQuote:
			if have {
				out = append(out, cur.String())
				cur.Reset()
				have = false
			}
		default:
			cur.WriteRune(r)
			have = true
		}
	}
	if inQuote {
		return nil, errors.New("command has an unterminated double quote")
	}
	if have {
		out = append(out, cur.String())
	}
	return out, nil
}

// DefaultCommand is the command used when none is configured.
func DefaultCommand() Command {
	return Command{"npx", "-y", "@playwright/mcp@" + PinnedVersion}
}

// Normalized returns c with defaults applied.
func (c Config) Normalized() Config {
	if c.Mode == "" {
		c.Mode = ModeProfile
	}
	if c.Channel == "" {
		c.Channel = DefaultChannel
	}
	if len(c.Command) == 0 {
		c.Command = DefaultCommand()
	}
	return c
}

// Idle returns the idle timeout (0 = never stop).
func (c Config) Idle() time.Duration {
	if c.IdleTimeout == "" {
		return DefaultIdleTimeout
	}
	d, err := time.ParseDuration(c.IdleTimeout)
	if err != nil || d < 0 {
		return DefaultIdleTimeout
	}
	return d
}

// Validate reports every problem in c. A config with problems is not run.
func (c Config) Validate() []string {
	var errs []string
	switch c.Mode {
	case "", ModeProfile, ModeExtension:
	default:
		errs = append(errs, fmt.Sprintf("mode %q: use %q or %q", c.Mode, ModeProfile, ModeExtension))
	}
	switch c.Channel {
	case "", "chrome", "msedge":
	default:
		errs = append(errs, fmt.Sprintf("channel %q: use chrome or msedge (set \"executable\" for Chromium or Brave)", c.Channel))
	}
	if c.IdleTimeout != "" {
		if d, err := time.ParseDuration(c.IdleTimeout); err != nil || d < 0 {
			errs = append(errs, fmt.Sprintf("idle_timeout %q: use a duration such as \"15m\" or \"0\"", c.IdleTimeout))
		}
	}
	if len(c.Command) > 0 && strings.TrimSpace(c.Command[0]) == "" {
		errs = append(errs, "command is empty")
	}
	if c.Mode == ModeExtension && c.Headless {
		errs = append(errs, "headless cannot be used in extension mode (it drives your visible browser)")
	}
	for _, p := range c.AllowOrigins {
		if _, err := ParsePattern(p, false); err != nil {
			errs = append(errs, "allow_origins: "+err.Error())
		}
	}
	for _, p := range c.DenyOrigins {
		if _, err := ParsePattern(p, true); err != nil {
			errs = append(errs, "deny_origins: "+err.Error())
		}
	}
	return errs
}

// Active reports whether the provider should publish tools.
func (c Config) Active() bool { return c.Enabled && len(c.Validate()) == 0 }

// AgentProfileDir is where the agent-only profile lives.
func AgentProfileDir(p state.Paths, c Config) string {
	if c.Mode != ModeExtension && c.ProfileDir != "" {
		return c.ProfileDir
	}
	return filepath.Join(p.BrowserDir(), "profile")
}

// LoadConfig reads browser.json; a missing file is a disabled configuration.
func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Config{}, nil
	}
	if err != nil {
		return Config{}, err
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// SaveConfig writes browser.json atomically with owner-only permissions.
func SaveConfig(path string, c Config) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return state.WriteFileAtomic(path, append(data, '\n'), 0o600)
}
