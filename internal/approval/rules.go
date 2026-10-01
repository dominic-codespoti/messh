package approval

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"messh/internal/provider"
	"messh/internal/state"
)

// Rule is a saved "Always allow". It covers one calling device and agent, one
// class, and every request whose exact hash or scope key equals Key.
type Rule struct {
	ID       string         `json:"id"`
	DeviceID string         `json:"device_id"`
	Device   string         `json:"device"` // name when the rule was saved, for display only
	Agent    string         `json:"agent"`
	Class    provider.Class `json:"class"`
	Key      string         `json:"key"`
	Label    string         `json:"label"`
	Broad    bool           `json:"broad,omitempty"`
	Created  time.Time      `json:"created"`
}

func (r Rule) matches(req Request) bool {
	if r.DeviceID != req.Caller.DeviceID || r.Agent != req.Caller.Agent || r.Class != req.Class {
		return false
	}
	if r.Key == ExactKey(req.Approval.Exact) {
		return true
	}
	for _, s := range req.Approval.Scopes {
		if s.Key != "" && s.Key == r.Key {
			return true
		}
	}
	return false
}

type rulesFile struct {
	Rules []Rule `json:"rules"`
}

func loadRules(path string) ([]Rule, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var f rulesFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%s: %w (fix or remove the file; refusing to start with no rules)", path, err)
	}
	return f.Rules, nil
}

func saveRules(path string, rules []Rule) error {
	if rules == nil {
		rules = []Rule{}
	}
	data, err := json.MarshalIndent(rulesFile{Rules: rules}, "", "  ")
	if err != nil {
		return err
	}
	return state.WriteFileAtomic(path, append(data, '\n'), 0o600)
}
