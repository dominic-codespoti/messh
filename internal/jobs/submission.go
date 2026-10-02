package jobs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"io"
	"messh/internal/files"
	"messh/internal/provider"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

var requestIDPattern = regexp.MustCompile("^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$")

// SubmissionID returns a deterministic, owner-scoped identifier.
func SubmissionID(deviceID, agent, requestID string) string {
	h := sha256.New()
	for _, s := range []string{deviceID, agent, requestID} {
		_, _ = fmt.Fprintf(h, "%d:", len(s))
		_, _ = io.WriteString(h, s)
	}
	return "job-" + hex.EncodeToString(h.Sum(nil)[:16])
}

// SubmissionHash hashes canonical JSON with request_id excluded.
func SubmissionHash(raw json.RawMessage) (string, error) {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var v map[string]any
	if e := d.Decode(&v); e != nil {
		return "", e
	}
	if v == nil {
		return "", errors.New("submission must be a JSON object")
	}
	if e := requireEOF(d); e != nil {
		return "", e
	}
	delete(v, "request_id")
	b, e := json.Marshal(v)
	if e != nil {
		return "", e
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:]), nil
}
func requireEOF(d *json.Decoder) error {
	var v any
	if e := d.Decode(&v); e != io.EOF {
		if e == nil {
			return errors.New("multiple JSON values")
		}
		return e
	}
	return nil
}

// ValidateSubmission validates request_id and recovery shapes without host lookups.
func ValidateSubmission(raw json.RawMessage) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var o map[string]json.RawMessage
	if e := d.Decode(&o); e != nil {
		return e
	}
	if o == nil {
		return errors.New("submission must be a JSON object")
	}
	if e := requireEOF(d); e != nil {
		return e
	}
	allowed := map[string]bool{"request_id": true, "recovery": true, "command": true, "args": true, "cwd": true, "env": true, "shell": true, "inputs": true, "workspace": true, "resources": true, "timeout_seconds": true, "label": true}
	for k := range o {
		if !allowed[k] {
			return fmt.Errorf("unknown job_submit field %q", k)
		}
	}
	if b, ok := o["request_id"]; ok {
		var id string
		if e := json.Unmarshal(b, &id); e != nil {
			return errors.New("request_id must be a string")
		}
		if !requestIDPattern.MatchString(id) {
			return errors.New("request_id must be 1-128 characters: letters, digits, dot, underscore, colon or hyphen")
		}
	}
	if b, ok := o["recovery"]; ok && string(b) != "null" {
		var rm map[string]json.RawMessage
		if e := json.Unmarshal(b, &rm); e != nil || rm == nil {
			return errors.New("recovery must be an object")
		}
		for k := range rm {
			if k != "checkpoint" && k != "args" {
				return fmt.Errorf("unknown recovery field %q", k)
			}
		}
		var r struct {
			Checkpoint string   `json:"checkpoint"`
			Args       []string `json:"args"`
		}
		d := json.NewDecoder(bytes.NewReader(b))
		d.DisallowUnknownFields()
		if e := d.Decode(&r); e != nil {
			return fmt.Errorf("recovery: %w", e)
		}
		clean := path.Clean(r.Checkpoint)
		if strings.TrimSpace(r.Checkpoint) == "" || len(r.Checkpoint) > maxCommandLen || strings.ContainsAny(r.Checkpoint, "\\:") || strings.ContainsRune(r.Checkpoint, 0) || path.IsAbs(r.Checkpoint) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
			return errors.New("recovery.checkpoint must be a workspace-relative path without escapes")
		}
		if len(r.Args) > maxArgs {
			return errors.New("recovery.args has too many arguments")
		}
		n := 0
		for _, a := range r.Args {
			n += len(a)
			if strings.ContainsRune(a, 0) {
				return errors.New("recovery.args must not contain NUL bytes")
			}
		}
		if n > 4*maxCommandLen {
			return errors.New("recovery.args are too long")
		}
		var shell bool
		_ = json.Unmarshal(o["shell"], &shell)
		if shell {
			return errors.New("recovery is not supported for shell jobs")
		}
	}
	return nil
}

type submissionRecord struct {
	Hash      string `json:"hash"`
	JobID     string `json:"job_id"`
	Owner     Owner  `json:"owner"`
	Workspace string `json:"workspace"`
	Deleted   bool   `json:"deleted,omitempty"`
}

// LookupSubmission looks up the accepted receipt before source inputs are re-hashed.
func (p *Provider) LookupSubmission(raw json.RawMessage, c provider.Caller) (*mcp.CallToolResult, bool, error) {
	if e := ValidateSubmission(raw); e != nil {
		return nil, false, e
	}
	var a struct {
		RequestID string `json:"request_id"`
	}
	if e := json.Unmarshal(raw, &a); e != nil {
		return nil, false, e
	}
	if a.RequestID == "" {
		return nil, false, nil
	}
	hash, e := SubmissionHash(raw)
	if e != nil {
		return nil, false, e
	}
	id := SubmissionID(c.DeviceID, c.Agent, a.RequestID)
	p.mu.Lock()
	r, ok := p.submissions[id]
	p.mu.Unlock()
	if !ok {
		b, x := os.ReadFile(p.submissionPath(id))
		if x == nil {
			if x = json.Unmarshal(b, &r); x != nil {
				return nil, false, x
			}
			ok = true
			p.mu.Lock()
			p.submissions[id] = r
			p.mu.Unlock()
		} else if !os.IsNotExist(x) {
			return nil, false, x
		}
	}
	if !ok || r.Owner.DeviceID != c.DeviceID || r.Owner.Agent != c.Agent {
		return nil, false, nil
	}
	if r.Hash != hash {
		return nil, true, fmt.Errorf("request_id %q was already used for a different payload", a.RequestID)
	}
	p.mu.Lock()
	j := p.jobs[r.JobID]
	if j == nil {
		p.mu.Unlock()
		if !r.Deleted {
			return nil, true, fmt.Errorf("request_id %q has a receipt for unavailable job %s", a.RequestID, r.JobID)
		}
		return nil, true, fmt.Errorf("request_id %q is a durable deletion tombstone for job %s", a.RequestID, r.JobID)
	}
	st, ws := j.State, j.Workspace
	p.mu.Unlock()
	res, e := provider.JSONResult(submitResult{JobID: r.JobID, State: st, Workspace: files.RootWorkspaces + "/" + ws, Message: "Previously accepted submission; inspect with job_status."})
	return res, true, e
}
func (p *Provider) submissionPath(id string) string {
	return filepath.Join(p.paths.JobsDir(), ".submission-"+id+".json")
}

func (p *Provider) saveSubmissionLocked(id string, r submissionRecord) error {
	b, e := json.Marshal(r)
	if e != nil {
		return e
	}
	if e = p.writeState(p.submissionPath(id), b, 0600); e != nil {
		return e
	}
	p.submissions[id] = r
	return nil
}
