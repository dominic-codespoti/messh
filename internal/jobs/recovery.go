package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"messh/internal/files"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// recoveredRequest validates the durable accepted request and rebuilds its
// execution shape exclusively from the accepted Job snapshot. In particular,
// it never resolves the original command or re-reads source input paths.
func (p *Provider) recoveredRequest(j *job) (*request, error) {
	if len(j.Submission) == 0 {
		return nil, errors.New("legacy submitted job has no original submission record")
	}
	if err := ValidateSubmission(j.Submission); err != nil {
		return nil, fmt.Errorf("invalid persisted submission: %w", err)
	}
	var original submitArgs
	if err := json.Unmarshal(j.Submission, &original); err != nil {
		return nil, err
	}
	if original.RequestID != j.RequestID {
		return nil, errors.New("persisted request_id does not match accepted job")
	}
	hash, err := SubmissionHash(j.Submission)
	if err != nil || hash != j.SubmissionHash {
		return nil, errors.New("persisted submission hash does not match accepted job")
	}
	if j.RequestID != "" && j.ID != SubmissionID(j.Owner.DeviceID, j.Owner.Agent, j.RequestID) {
		return nil, errors.New("persisted request identity does not match accepted job")
	}
	if len(j.Exact) != sha256.Size*2 || !filepath.IsAbs(j.Path) || j.Workspace == "" {
		return nil, errors.New("accepted job is missing its resolved executable or approval snapshot")
	}
	if _, err := hex.DecodeString(j.Exact); err != nil {
		return nil, errors.New("accepted job has an invalid approval snapshot")
	}
	cwd, err := cleanCwd(original.Cwd)
	if err != nil || original.Shell != j.Shell || (original.Shell && original.Command != j.Line) || (!original.Shell && !equalStrings(original.Args, j.Args)) || cwd != j.Cwd || original.TimeoutSeconds != j.TimeoutSec {
		return nil, errors.New("persisted command settings do not match original submission")
	}
	refs := make([]string, 0, len(original.Inputs))
	seen := make(map[string]bool, len(original.Inputs))
	for _, input := range original.Inputs {
		device, ref := files.SplitQualified(input)
		if device != "" {
			return nil, fmt.Errorf("accepted input %q names another device", input)
		}
		parsed, err := files.ParseRef(ref)
		if err != nil {
			return nil, fmt.Errorf("invalid accepted input %q: %w", input, err)
		}
		if !seen[string(parsed)] {
			seen[string(parsed)] = true
			refs = append(refs, string(parsed))
		}
	}
	sort.Strings(refs)
	if len(refs) != len(j.Inputs) {
		return nil, errors.New("accepted input list does not match persisted input snapshots")
	}
	for i, ref := range refs {
		if j.Inputs[i].Ref != ref {
			return nil, errors.New("accepted input references do not match persisted input snapshots")
		}
		in := j.Inputs[i]
		if in.SHA256 == "" || len(in.SHA256) != sha256.Size*2 {
			return nil, fmt.Errorf("accepted input %q has no persisted content hash", ref)
		}
		if _, err := hex.DecodeString(in.SHA256); err != nil {
			return nil, fmt.Errorf("accepted input %q has an invalid content hash", ref)
		}
		if !in.InPlace && (in.Rel == "" || filepath.IsAbs(in.Rel) || strings.HasPrefix(filepath.Clean(in.Rel), ".."+string(filepath.Separator))) {
			return nil, fmt.Errorf("accepted input %q has an invalid workspace snapshot path", ref)
		}
	}
	var recovery *recoveryArgs
	if original.Recovery != nil {
		recovery = &recoveryArgs{Checkpoint: original.Recovery.Checkpoint, Args: append([]string(nil), original.Recovery.Args...)}
	}
	return &request{RequestID: j.RequestID, Recovery: recovery, Path: j.Path, Args: append([]string(nil), j.Args...), Shell: j.Shell, Line: j.Line, Workspace: j.Workspace, Cwd: j.Cwd, Env: cloneEnv(j.Env), Claims: j.Claims, TimeoutSec: j.TimeoutSec, Inputs: append([]Input(nil), j.Inputs...), Label: j.Label}, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func cloneEnv(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// validateSubmissionReceipts rejects malformed/conflicting receipts and
// rebuilds a missing receipt only from a live authoritative accepted Job.
func (p *Provider) validateSubmissionReceipts(loaded []*job) error {
	byID := make(map[string]*job, len(loaded))
	for _, j := range loaded {
		byID[j.ID] = j
	}
	entries, err := os.ReadDir(p.paths.JobsDir())
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, ".submission-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		key := strings.TrimSuffix(strings.TrimPrefix(name, ".submission-"), ".json")
		j := byID[key]
		data, err := os.ReadFile(p.submissionPath(key))
		if err != nil {
			return err
		}
		var rec submissionRecord
		if err := json.Unmarshal(data, &rec); err != nil {
			return fmt.Errorf("corrupt submission receipt %s: %w", key, err)
		}
		if key == "" || rec.JobID == "" || rec.Hash == "" || rec.Owner.DeviceID == "" {
			return fmt.Errorf("corrupt submission receipt %s: missing identity", key)
		}
		if key != rec.JobID || len(key) != len("job-")+32 || !strings.HasPrefix(key, "job-") || len(rec.Hash) != sha256.Size*2 {
			return fmt.Errorf("corrupt submission receipt %s: invalid identity", key)
		}
		if _, err := hex.DecodeString(rec.Hash); err != nil {
			return fmt.Errorf("corrupt submission receipt %s: invalid hash", key)
		}
		if j != nil && rec.Deleted {
			rec.Deleted = false
			b, err := json.Marshal(rec)
			if err != nil {
				return err
			}
			if err := p.writeState(p.submissionPath(key), b, 0o600); err != nil {
				return fmt.Errorf("restore live submission receipt %s: %w", key, err)
			}
		}
		if prior, ok := p.submissions[key]; ok && prior != rec {
			prior.Deleted = false
			if prior != rec {
				return fmt.Errorf("conflicting submission receipt %s", key)
			}
		}
		if j != nil {
			if j.RequestID == "" || SubmissionID(j.Owner.DeviceID, j.Owner.Agent, j.RequestID) != key || j.SubmissionHash != rec.Hash || j.Owner.DeviceID != rec.Owner.DeviceID || j.Owner.Agent != rec.Owner.Agent || j.Workspace != rec.Workspace {
				return fmt.Errorf("submission receipt %s conflicts with accepted job %s", key, rec.JobID)
			}
		} else if !rec.Deleted {
			return fmt.Errorf("submission receipt %s points to missing job without deletion tombstone", key)
		}
		p.submissions[key] = rec
	}
	for _, j := range loaded {
		if j.RequestID == "" || len(j.Submission) == 0 {
			continue
		}
		if _, err := p.recoveredRequest(j); err != nil {
			return fmt.Errorf("accepted job %s is not a valid receipt source: %w", j.ID, err)
		}
		key := SubmissionID(j.Owner.DeviceID, j.Owner.Agent, j.RequestID)
		rec := submissionRecord{Hash: j.SubmissionHash, JobID: j.ID, Owner: j.Owner, Workspace: j.Workspace}
		if prior, ok := p.submissions[key]; ok {
			if prior != rec {
				return fmt.Errorf("submission receipt %s conflicts with accepted job %s", key, j.ID)
			}
			continue
		}
		if err := p.saveSubmissionLocked(key, rec); err != nil {
			return fmt.Errorf("rebuild submission receipt %s: %w", key, err)
		}
	}
	return nil
}
