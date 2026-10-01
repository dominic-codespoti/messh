package approval

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Audit record kinds.
const (
	KindDecision   = "decision"
	KindCompletion = "completion"
)

// auditMaxBytes is the size at which the log rolls over to <file>.1,
// replacing the previous roll-over, so history is bounded to about twice this.
const auditMaxBytes = 10 << 20

// AuditRecord is one line of the audit log. A "decision" record is written
// when a request is decided; a "completion" record follows when the allowed
// call has finished.
type AuditRecord struct {
	Time       time.Time `json:"time"`
	Kind       string    `json:"kind"`
	ID         string    `json:"id"` // request ID; ties a completion to its decision
	DeviceID   string    `json:"device_id"`
	Device     string    `json:"device"`
	Agent      string    `json:"agent"`
	Tool       string    `json:"tool"`
	Class      string    `json:"class"`
	Title      string    `json:"title"`
	Exact      string    `json:"exact"`
	Decision   string    `json:"decision,omitempty"` // one of the Outcome* values
	RuleID     string    `json:"rule_id,omitempty"`
	Auto       string    `json:"auto,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Surface    string    `json:"surface,omitempty"` // where the person answered, if a person did
	OK         *bool     `json:"ok,omitempty"`      // completion records only
	Error      string    `json:"error,omitempty"`
	DurationMS int64     `json:"duration_ms,omitempty"`
}

type auditLog struct {
	path string
	max  int64
	mu   sync.Mutex
}

func (a *auditLog) append(rec AuditRecord) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(a.path), 0o700); err != nil {
		return err
	}
	// Open and close per record: it is rare, and a held handle would block
	// the rename below on Windows.
	if fi, err := os.Stat(a.path); err == nil && fi.Size()+int64(len(line)) > a.max {
		if err := os.Rename(a.path, a.path+".1"); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(a.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(line)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// tail returns the newest n records, oldest first, reaching into the
// rolled-over file when the current one is short.
func (a *auditLog) tail(n int) ([]AuditRecord, error) {
	if n <= 0 {
		n = 50
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var recs []AuditRecord
	for _, path := range []string{a.path + ".1", a.path} {
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for line := range bytes.Lines(data) {
			var r AuditRecord
			if json.Unmarshal(bytes.TrimSpace(line), &r) == nil && r.Kind != "" {
				recs = append(recs, r)
			}
		}
	}
	if len(recs) > n {
		recs = recs[len(recs)-n:]
	}
	return recs, nil
}
