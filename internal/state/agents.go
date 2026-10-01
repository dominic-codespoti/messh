package state

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var agentNameRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidAgentName reports whether name can identify an agent.
func ValidAgentName(name string) bool { return agentNameRE.MatchString(name) }

func (p Paths) agentTokenFile(name string) string {
	return filepath.Join(p.AgentsDir(), name+".token")
}

func (p Paths) agentSettingsFile(name string) string {
	return filepath.Join(p.AgentsDir(), name+".json")
}

// Tool modes decide what an agent's MCP tools/list contains: compact shows
// only the mesh-level tools (devices' tools are reached via mesh_tools and
// mesh_call), full shows every device tool as <device>__<tool>.
const (
	ToolsCompact = "compact"
	ToolsFull    = "full"
)

// cliAgent is the name recorded for callers holding the control token.
const cliAgent = "cli"

// ValidAgentMode reports whether mode is a known tool mode.
func ValidAgentMode(mode string) bool { return mode == ToolsCompact || mode == ToolsFull }

type agentSettings struct {
	Tools string `json:"tools"`
}

// AgentMode returns an agent's tool mode: compact unless set otherwise, and
// always full for the control-token caller "cli".
func (p Paths) AgentMode(name string) (string, error) {
	if name == cliAgent {
		return ToolsFull, nil
	}
	if !ValidAgentName(name) {
		return "", fmt.Errorf("invalid agent name %q", name)
	}
	var s agentSettings
	err := readJSON(p.agentSettingsFile(name), &s)
	if errors.Is(err, fs.ErrNotExist) {
		return ToolsCompact, nil
	}
	if err != nil {
		return "", err
	}
	if s.Tools == "" {
		return ToolsCompact, nil
	}
	if !ValidAgentMode(s.Tools) {
		return "", fmt.Errorf("%s: unknown tool mode %q (want %s or %s)", p.agentSettingsFile(name), s.Tools, ToolsCompact, ToolsFull)
	}
	return s.Tools, nil
}

// SetAgentMode stores the tool mode of a registered agent.
func (p Paths) SetAgentMode(name, mode string) error {
	if !ValidAgentMode(mode) {
		return fmt.Errorf("invalid tool mode %q: use %s or %s", mode, ToolsCompact, ToolsFull)
	}
	if _, err := p.AgentToken(name); err != nil {
		return err
	}
	return writeJSON(p.agentSettingsFile(name), agentSettings{Tools: mode}, 0o600)
}

// AddAgent registers an agent and returns its bearer token. Re-adding an
// existing agent returns the existing token.
func (p Paths) AddAgent(name string) (string, error) {
	if !ValidAgentName(name) || name == cliAgent {
		return "", fmt.Errorf("invalid agent name %q: use 1-32 chars of a-z, 0-9, '-' (\"cli\" is reserved)", name)
	}
	return readOrCreateToken(p.agentTokenFile(name))
}

// AgentToken returns the token of a registered agent.
func (p Paths) AgentToken(name string) (string, error) {
	if !ValidAgentName(name) {
		return "", fmt.Errorf("invalid agent name %q", name)
	}
	data, err := os.ReadFile(p.agentTokenFile(name))
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("agent %q is not registered (run `messh agent add %s`)", name, name)
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// RemoveAgent deletes an agent's token and settings, revoking its access.
func (p Paths) RemoveAgent(name string) error {
	if !ValidAgentName(name) {
		return fmt.Errorf("invalid agent name %q", name)
	}
	err := os.Remove(p.agentTokenFile(name))
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("agent %q is not registered", name)
	}
	if err != nil {
		return err
	}
	if err := os.Remove(p.agentSettingsFile(name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Agents lists registered agent names.
func (p Paths) Agents() ([]string, error) {
	entries, err := os.ReadDir(p.AgentsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), ".token")
		if ok && !e.IsDir() && ValidAgentName(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, nil
}

// AgentForToken returns the agent whose token equals tok.
func (p Paths) AgentForToken(tok string) (string, bool) {
	names, err := p.Agents()
	if err != nil {
		return "", false
	}
	for _, name := range names {
		want, err := p.AgentToken(name)
		if err != nil || want == "" {
			continue
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(tok)) == 1 {
			return name, true
		}
	}
	return "", false
}
