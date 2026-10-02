package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/files"
	"messh/internal/provider"
)

const (
	defaultTailLines = 20
	maxOutputsListed = 200
	maxOutputsWalked = 5000
)

func decodeArgs(raw json.RawMessage, v any) error {
	if len(raw) == 0 {
		return nil
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	return nil
}

// ownedLocked finds a job the caller submitted. Other agents' jobs look
// exactly like missing ones.
func (p *Provider) ownedLocked(id string, c provider.Caller) (*job, error) {
	j := p.jobs[id]
	if j == nil || j.deleting || !j.Owner.same(c) {
		return nil, fmt.Errorf("no such job %q", id)
	}
	return j, nil
}

// Status is the job_status / job_wait result.
type Status struct {
	JobID            string       `json:"job_id"`
	Label            string       `json:"label,omitempty"`
	State            State        `json:"state"`
	Reason           string       `json:"reason,omitempty"`
	Submitted        time.Time    `json:"submitted"`
	Queued           *time.Time   `json:"queued,omitempty"`
	Started          *time.Time   `json:"started,omitempty"`
	Finished         *time.Time   `json:"finished,omitempty"`
	ExitCode         *int         `json:"exit_code,omitempty"`
	Command          []string     `json:"command"`
	Workspace        string       `json:"workspace"`
	Cwd              string       `json:"cwd,omitempty"`
	Resources        Claims       `json:"resources"`
	TimeoutSeconds   int          `json:"timeout_seconds,omitempty"`
	QueuePosition    int          `json:"queue_position,omitempty"`
	WaitingFor       string       `json:"waiting_for,omitempty"`
	Inputs           []InputView  `json:"inputs,omitempty"`
	Outputs          []OutputFile `json:"output_files"`
	OutputsTruncated bool         `json:"output_files_truncated,omitempty"`
	StdoutTail       []string     `json:"stdout_tail"`
	StderrTail       []string     `json:"stderr_tail"`
	LogsOmitted      bool         `json:"earlier_log_output_omitted,omitempty"`
	Notes            []string     `json:"notes,omitempty"`
	Recovery         *Recovery    `json:"recovery,omitempty"`
	Attempt          int          `json:"attempt,omitempty"`
	CheckpointSHA256 string       `json:"checkpoint_sha256,omitempty"`
}

// InputView is an input as reported to agents.
type InputView struct {
	Ref    string `json:"ref"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// OutputFile is a file in the job workspace, as a ref the file tools accept.
type OutputFile struct {
	Ref      string    `json:"ref"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

func (p *Provider) statusOf(id string, c provider.Caller, tail int) (Status, error) {
	p.mu.Lock()
	j, err := p.ownedLocked(id, c)
	if err != nil {
		p.mu.Unlock()
		return Status{}, err
	}
	rec := j.Job
	waiting := j.waiting
	notes := rec.Notes
	if j.State == StateRunning {
		// A fresh slice: rec.Notes shares its array with the live job.
		notes = append(append([]string(nil), rec.Notes...), "sleep_inhibit: "+p.sleepStatusLocked())
	}
	pos := 0
	if j.State == StateQueued {
		for _, o := range p.jobs {
			if o.State == StateQueued && o.seq <= j.seq {
				pos++
			}
		}
	}
	p.mu.Unlock()

	st := Status{
		JobID: rec.ID, Label: rec.Label, State: rec.State, Reason: rec.Reason, Submitted: rec.Submitted,
		Queued: rec.Queued, Started: rec.Started, Finished: rec.Finished, ExitCode: rec.ExitCode,
		Command: rec.Argv(), Workspace: files.RootWorkspaces + "/" + rec.Workspace, Cwd: rec.Cwd, Resources: rec.Claims,
		TimeoutSeconds: rec.TimeoutSec, QueuePosition: pos, WaitingFor: waiting, Notes: notes, Recovery: rec.Recovery, Attempt: rec.Attempt, CheckpointSHA256: rec.CheckpointSHA256,
		Outputs: []OutputFile{}, StdoutTail: []string{}, StderrTail: []string{},
	}
	for _, in := range rec.Inputs {
		st.Inputs = append(st.Inputs, InputView{Ref: in.Ref, SHA256: in.SHA256, Size: in.Size})
	}
	if rec.Started != nil {
		st.Outputs, st.OutputsTruncated = p.listOutputs(rec)
	}
	dir := p.jobDir(rec.ID)
	if lines, more := openLog(dir, "stdout").tailLines(tail); lines != nil {
		st.StdoutTail, st.LogsOmitted = lines, more
	}
	if lines, more := openLog(dir, "stderr").tailLines(tail); lines != nil {
		st.StderrTail, st.LogsOmitted = lines, st.LogsOmitted || more
	}
	return st, nil
}

// listOutputs lists the workspace's files except inputs the job left alone.
func (p *Provider) listOutputs(rec Job) ([]OutputFile, bool) {
	root, err := workspaceDir(p.paths, rec.Workspace)
	if err != nil {
		return []OutputFile{}, false
	}
	untouched := map[string]Input{}
	for _, in := range rec.Inputs {
		untouched[in.Rel] = in
	}
	out := []OutputFile{}
	truncated, walked := false, 0
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if walked++; walked > maxOutputsWalked {
			truncated = true
			return fs.SkipAll
		}
		if !d.Type().IsRegular() {
			return nil // symlinks and devices are not shareable
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		info, err := d.Info()
		if err != nil {
			return nil
		}
		if in, ok := untouched[rel]; ok && info.Size() == in.Size && info.ModTime().UnixNano() == in.ModTime {
			return nil
		}
		ref, err := files.ParseRef(files.RootWorkspaces + "/" + rec.Workspace + "/" + rel)
		if err != nil {
			return nil // a name no ref can express
		}
		if len(out) >= maxOutputsListed {
			truncated = true
			return fs.SkipAll
		}
		out = append(out, OutputFile{Ref: string(ref), Size: info.Size(), Modified: info.ModTime().UTC()})
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Ref < out[j].Ref })
	return out, truncated
}

type idArgs struct {
	JobID string `json:"job_id"`
}

func (p *Provider) callStatus(_ context.Context, raw json.RawMessage, c provider.Caller) (*mcp.CallToolResult, error) {
	var a struct {
		idArgs
		TailLines *int `json:"tail_lines"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	tail := defaultTailLines
	if a.TailLines != nil {
		tail = min(max(*a.TailLines, 0), 200)
	}
	st, err := p.statusOf(a.JobID, c, tail)
	if err != nil {
		return nil, err
	}
	return provider.JSONResult(st)
}

func (p *Provider) callWait(ctx context.Context, raw json.RawMessage, c provider.Caller) (*mcp.CallToolResult, error) {
	var a struct {
		idArgs
		TimeoutSeconds *int `json:"timeout_seconds"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	secs := 30
	if a.TimeoutSeconds != nil {
		secs = *a.TimeoutSeconds
	}
	if secs < 1 || secs > maxWaitSeconds {
		return nil, fmt.Errorf("timeout_seconds must be between 1 and %d", maxWaitSeconds)
	}
	timer := time.NewTimer(time.Duration(secs) * time.Second)
	defer timer.Stop()
	for {
		p.mu.Lock()
		j, err := p.ownedLocked(a.JobID, c)
		if err != nil {
			p.mu.Unlock()
			return nil, err
		}
		done, changed := j.State.Terminal(), j.changed
		p.mu.Unlock()
		if done {
			break
		}
		select {
		case <-changed:
			continue
		case <-timer.C:
		case <-ctx.Done():
		case <-p.ctx.Done():
		}
		break
	}
	st, err := p.statusOf(a.JobID, c, defaultTailLines)
	if err != nil {
		return nil, err
	}
	return provider.JSONResult(st)
}

func (p *Provider) callLogs(raw json.RawMessage, c provider.Caller) (*mcp.CallToolResult, error) {
	var a struct {
		idArgs
		Stream    string `json:"stream"`
		TailLines *int   `json:"tail_lines"`
		Offset    *int64 `json:"offset"`
		MaxBytes  *int   `json:"max_bytes"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if err := validStream(a.Stream); err != nil {
		return nil, err
	}
	if a.Stream == "" {
		a.Stream = "both"
	}
	if a.Offset != nil && (a.Stream == "both" || *a.Offset < 0) {
		return nil, errors.New("offset needs stream stdout or stderr and must not be negative")
	}
	p.mu.Lock()
	j, err := p.ownedLocked(a.JobID, c)
	if err != nil {
		p.mu.Unlock()
		return nil, err
	}
	id, state := j.ID, j.State
	p.mu.Unlock()

	tail := 100
	if a.TailLines != nil {
		tail = min(max(*a.TailLines, 1), 2000)
	}
	maxBytes := 64 << 10
	if a.MaxBytes != nil {
		maxBytes = min(max(*a.MaxBytes, 1), 1<<20)
	}
	chunk := func(name string) logChunk {
		l := openLog(p.jobDir(id), name)
		if a.Offset != nil {
			return l.chunkFrom(*a.Offset, maxBytes)
		}
		return l.chunkTail(tail)
	}
	res := struct {
		JobID  string    `json:"job_id"`
		State  State     `json:"state"`
		Stdout *logChunk `json:"stdout,omitempty"`
		Stderr *logChunk `json:"stderr,omitempty"`
	}{JobID: id, State: state}
	if a.Stream != "stderr" {
		c := chunk("stdout")
		res.Stdout = &c
	}
	if a.Stream != "stdout" {
		c := chunk("stderr")
		res.Stderr = &c
	}
	return provider.JSONResult(res)
}

func (p *Provider) callCancel(ctx context.Context, raw json.RawMessage, c provider.Caller) (*mcp.CallToolResult, error) {
	var a idArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	p.mu.Lock()
	j, err := p.ownedLocked(a.JobID, c)
	if err != nil {
		p.mu.Unlock()
		return nil, err
	}
	if j.State.Terminal() {
		p.mu.Unlock()
		st, err := p.statusOf(a.JobID, c, defaultTailLines)
		if err != nil {
			return nil, err
		}
		return provider.JSONResult(st)
	}
	if err := p.cancelLocked(j); err != nil {
		p.mu.Unlock()
		return nil, err
	}
	changed := j.changed
	p.mu.Unlock()

	// A running job needs a moment to die; give an honest answer if it is quick.
	if st := func() State { p.mu.Lock(); defer p.mu.Unlock(); return j.State }(); !st.Terminal() {
		select {
		case <-changed:
		case <-time.After(3 * time.Second):
		case <-ctx.Done():
		}
	}
	st, err := p.statusOf(a.JobID, c, defaultTailLines)
	if err != nil {
		return nil, err
	}
	return provider.JSONResult(st)
}

// Summary is one row of job_list.
type Summary struct {
	JobID         string     `json:"job_id"`
	Label         string     `json:"label,omitempty"`
	State         State      `json:"state"`
	Reason        string     `json:"reason,omitempty"`
	Command       string     `json:"command"`
	Workspace     string     `json:"workspace"`
	Submitted     time.Time  `json:"submitted"`
	Finished      *time.Time `json:"finished,omitempty"`
	ExitCode      *int       `json:"exit_code,omitempty"`
	QueuePosition int        `json:"queue_position,omitempty"`
}

func (p *Provider) callList(raw json.RawMessage, c provider.Caller) (*mcp.CallToolResult, error) {
	var a struct {
		State State `json:"state"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.State != "" && !a.State.valid() {
		return nil, fmt.Errorf("unknown state %q", a.State)
	}
	p.mu.Lock()
	var recs []*job
	for _, j := range p.jobs {
		if j.Owner.same(c) && !j.deleting && (a.State == "" || j.State == a.State) {
			recs = append(recs, j)
		}
	}
	pos := map[string]int{}
	for i, q := range p.queuedLocked() {
		pos[q.ID] = i + 1
	}
	list := make([]Summary, 0, len(recs))
	for _, j := range recs {
		list = append(list, Summary{
			JobID: j.ID, Label: j.Label, State: j.State, Reason: j.Reason, Command: truncate(strings.Join(j.Argv(), " "), 200),
			Workspace: files.RootWorkspaces + "/" + j.Workspace, Submitted: j.Submitted, Finished: j.Finished,
			ExitCode: j.ExitCode, QueuePosition: pos[j.ID],
		})
	}
	p.mu.Unlock()
	sort.Slice(list, func(i, k int) bool { return list[i].Submitted.After(list[k].Submitted) })
	return provider.JSONResult(struct {
		Jobs []Summary `json:"jobs"`
	}{list})
}

func (p *Provider) callDelete(raw json.RawMessage, c provider.Caller) (*mcp.CallToolResult, error) {
	var a idArgs
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	p.mu.Lock()
	j, err := p.ownedLocked(a.JobID, c)
	if err != nil {
		p.mu.Unlock()
		return nil, err
	}
	if !j.State.Terminal() {
		st := j.State
		p.mu.Unlock()
		return nil, fmt.Errorf("job %s is %s; cancel it and let it finish before deleting", a.JobID, st)
	}
	j.deleting = true
	ws := j.Workspace
	shared := false
	for _, o := range p.jobs {
		if o != j && !o.deleting && o.Workspace == ws {
			shared = true
		}
	}
	if !shared {
		p.deleting[ws] = true
	}
	receipt, hasReceipt := p.submissions[a.JobID]
	if hasReceipt {
		receipt.Deleted = true
		if err := p.saveSubmissionLocked(a.JobID, receipt); err != nil {
			j.deleting = false
			if !shared {
				delete(p.deleting, ws)
			}
			p.mu.Unlock()
			return nil, fmt.Errorf("persist job deletion tombstone: %w", err)
		}
	}
	p.mu.Unlock()

	rmErr := os.RemoveAll(p.jobDir(a.JobID))
	jobRmErr := rmErr
	removedWS := false
	if !shared {
		dir, err := workspaceDir(p.paths, ws)
		if err == nil {
			err = os.RemoveAll(dir)
		}
		if rmErr == nil {
			rmErr = err
		}
		removedWS = err == nil
	}

	p.mu.Lock()
	if jobRmErr == nil {
		delete(p.jobs, a.JobID)
	} else {
		j.deleting = false
		if hasReceipt {
			receipt.Deleted = false
			if err := p.saveSubmissionLocked(a.JobID, receipt); err != nil {
				p.log.Error("restore submission receipt after failed delete", "job", a.JobID, "error", err)
			}
		}
	}
	if !shared {
		delete(p.deleting, ws)
	}
	p.mu.Unlock()
	if rmErr != nil {
		return nil, fmt.Errorf("delete job: %w", rmErr)
	}
	note := ""
	if shared {
		note = "workspace kept: another job uses it"
	}
	return provider.JSONResult(struct {
		Deleted          string `json:"deleted"`
		WorkspaceRemoved bool   `json:"workspace_removed"`
		Note             string `json:"note,omitempty"`
	}{Deleted: a.JobID, WorkspaceRemoved: removedWS, Note: note})
}

type gpuView struct {
	GPUState
	ClaimedBy string `json:"claimed_by,omitempty"`
}

func (p *Provider) callResources(ctx context.Context, c provider.Caller) (*mcp.CallToolResult, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	snap, err := p.res.Snapshot(cctx)
	if err != nil {
		return nil, fmt.Errorf("read hardware state: %w", err)
	}
	p.mu.Lock()
	gpus := make([]gpuView, 0, len(snap.GPUs))
	for _, g := range snap.GPUs {
		v := gpuView{GPUState: g}
		if by := p.gpuBusy[g.Index]; by != "" {
			v.ClaimedBy = "another agent's job"
			if o := p.jobs[by]; o != nil && o.Owner.same(c) {
				v.ClaimedBy = by
			}
		}
		gpus = append(gpus, v)
	}
	queued := len(p.queuedLocked())
	view := struct {
		CPUThreads    int       `json:"cpu_threads"`
		CPUBudget     int       `json:"cpu_threads_claimable"`
		CPUClaimed    int       `json:"cpu_threads_claimed"`
		MemTotalMB    int64     `json:"ram_total_mb"`
		MemAvailMB    int64     `json:"ram_available_mb"`
		GPUs          []gpuView `json:"gpus"`
		Running       int       `json:"jobs_running"`
		Queued        int       `json:"jobs_queued"`
		MaxConcurrent int       `json:"max_concurrent_jobs"`
		SleepInhibit  string    `json:"sleep_inhibit"`
	}{snap.CPUThreads, p.opts.CPUBudget, p.cpusUsed, snap.MemTotalMB, snap.MemAvailMB, gpus, p.running, queued, p.opts.MaxConcurrent, p.sleepStatusLocked()}
	p.mu.Unlock()
	return provider.JSONResult(view)
}
