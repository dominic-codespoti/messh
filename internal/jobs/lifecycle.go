package jobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/files"
	"messh/internal/provider"
)

type submitResult struct {
	JobID     string `json:"job_id"`
	State     State  `json:"state"`
	Workspace string `json:"workspace"`
	Message   string `json:"message"`
}

// callSubmit records the job and returns at once; everything slow (input
// snapshots, the human's decision, the queue) happens in the background.
func (p *Provider) callSubmit(ctx context.Context, args json.RawMessage, caller provider.Caller) (*mcp.CallToolResult, error) {
	ticket, ok := provider.TicketFrom(ctx)
	if !ok {
		return nil, errors.New("job_submit must be approved by the device owner before it runs")
	}
	req, err := p.parseSubmit(args)
	if err != nil {
		return nil, err
	}

	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		return nil, errors.New("the job service is shutting down")
	}
	id := newID()
	for p.jobs[id] != nil {
		id = newID()
	}
	ws := req.Workspace
	if ws == "" {
		ws = id
	}
	if p.deleting[ws] {
		p.mu.Unlock()
		return nil, fmt.Errorf("workspace %q is being deleted; try again in a moment", ws)
	}
	if _, err := p.cwdDir(ws, req.Cwd); err != nil {
		p.mu.Unlock()
		return nil, fmt.Errorf("prepare workspace: %w", err)
	}
	if err := os.MkdirAll(p.jobDir(id), 0o700); err != nil {
		p.mu.Unlock()
		return nil, fmt.Errorf("prepare workspace: %w", err)
	}

	p.nextSeq++
	j := &job{
		Job: Job{
			ID: id, Label: req.Label, Owner: ownerOf(caller), State: StateSubmitted, Submitted: time.Now(),
			Path: req.Path, Args: req.Args, Shell: req.Shell, Line: req.Line,
			Workspace: ws, Cwd: req.Cwd, Env: req.Env, Claims: req.Claims, TimeoutSec: req.TimeoutSec,
		},
		seq: p.nextSeq, ticket: ticket, changed: make(chan struct{}), ready: make(chan struct{}),
	}
	// The approval wait must outlive this request, so it hangs off the provider.
	j.ctx, j.cancel = context.WithCancel(p.ctx)
	p.jobs[id] = j
	p.persistLocked(j)
	p.wg.Add(1)
	p.mu.Unlock()

	go p.prepare(j, req)

	// Usually instant (hard links); report awaiting_approval when it is.
	select {
	case <-j.ready:
	case <-time.After(time.Second):
	case <-ctx.Done():
	}
	p.mu.Lock()
	st := j.State
	p.mu.Unlock()
	msg := "Waiting for the owner of this device to approve the job. Poll with job_status or block with job_wait."
	if st.Terminal() {
		msg = "The job could not start; see job_status for the reason."
	}
	return provider.JSONResult(submitResult{JobID: id, State: st, Workspace: files.RootWorkspaces + "/" + ws, Message: msg})
}

// prepare snapshots inputs, proves the request still equals what was shown
// to the owner, then waits for the decision.
func (p *Provider) prepare(j *job, req *request) {
	defer p.wg.Done()
	readyClosed := false
	closeReady := func() {
		if !readyClosed {
			readyClosed = true
			close(j.ready)
		}
	}
	defer closeReady()

	fail := func(reason string) {
		j.ticket.Cancel()
		p.mu.Lock()
		p.finishLocked(j, StateFailed, reason, nil)
		p.mu.Unlock()
	}

	if err := p.snapshotInputs(j, req); err != nil {
		if j.ctx.Err() == nil {
			fail(err.Error())
		}
		return
	}
	if !p.takePending(ownerKey(j.Owner), req.exact()) {
		fail("the request changed between approval and execution (input files or the command differ from what was shown); submit again")
		return
	}

	p.mu.Lock()
	if j.State.Terminal() {
		p.mu.Unlock()
		return
	}
	j.Inputs = append([]Input(nil), req.Inputs...)
	j.Exact = req.exact()
	j.State = StateAwaitingApproval
	p.touchLocked(j)
	p.mu.Unlock()
	closeReady()

	ok, werr := j.ticket.Wait(j.ctx)

	p.mu.Lock()
	defer p.mu.Unlock()
	if j.State.Terminal() {
		return // cancelled while waiting
	}
	switch {
	case ok:
		now := time.Now()
		p.nextSeq++
		j.seq = p.nextSeq
		j.State, j.Queued = StateQueued, &now
		p.touchLocked(j)
		p.kick()
	case p.ctx.Err() != nil:
		p.finishLocked(j, StateInterrupted, "node shut down", nil)
	case isExpiry(werr):
		p.finishLocked(j, StateFailed, "expired", nil)
	default:
		p.finishLocked(j, StateFailed, "denied", nil)
	}
}

func isExpiry(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "expire") || strings.Contains(s, "timed out") || strings.Contains(s, "timeout")
}

// snapshotInputs puts every input into the job workspace, preferring a hard
// link (instant, immune to a later rename or replace of the source) over a
// copy, and records the hash of what it holds.
func (p *Provider) snapshotInputs(j *job, req *request) error {
	taken := map[string]string{}
	for i := range req.Inputs {
		in := &req.Inputs[i]
		in.Rel, in.InPlace = inputRel(in.Ref, j.Workspace)
		if prev, dup := taken[in.Rel]; dup {
			return fmt.Errorf("inputs %s and %s would land at the same place (%s) in the workspace", prev, in.Ref, in.Rel)
		}
		taken[in.Rel] = in.Ref
		src, err := files.Resolve(p.paths, files.Ref(in.Ref))
		if err != nil {
			return fmt.Errorf("input %s: %w", in.Ref, err)
		}
		if in.InPlace {
			sum, st, at, err := p.hashes.hashFile(src)
			if err != nil {
				return fmt.Errorf("input %s: %w", in.Ref, err)
			}
			in.SHA256, in.Size, in.ModTime, in.HashedAt = sum, st.Size(), st.ModTime().UnixNano(), at
			continue
		}
		dstRef, err := files.ParseRef(files.RootWorkspaces + "/" + j.Workspace + "/" + in.Rel)
		if err != nil {
			return fmt.Errorf("input %s: %w", in.Ref, err)
		}
		dst, err := files.Resolve(p.paths, dstRef)
		if err != nil {
			return fmt.Errorf("input %s: %w", in.Ref, err)
		}
		if err := p.place(j.ctx, src, dst, in); err != nil {
			return fmt.Errorf("input %s: %w", in.Ref, err)
		}
	}
	return nil
}

// place makes dst hold src's bytes and fills in the hash fields.
func (p *Provider) place(ctx context.Context, src, dst string, in *Input) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	srcSt, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !srcSt.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	if dstSt, err := os.Lstat(dst); err == nil {
		// A workspace reused by several jobs may already hold this file.
		if !dstSt.Mode().IsRegular() {
			return fmt.Errorf("the workspace already has something else at %s", in.Rel)
		}
		a, _, _, err := p.hashes.hashFile(src)
		if err != nil {
			return err
		}
		b, st, at, err := p.hashes.hashFile(dst)
		if err != nil {
			return err
		}
		if a != b {
			return fmt.Errorf("the workspace already has a different file at %s; use another workspace or remove it", in.Rel)
		}
		in.SHA256, in.Size, in.ModTime, in.HashedAt = b, st.Size(), st.ModTime().UnixNano(), at
		return nil
	}
	if err := os.Link(src, dst); err == nil {
		// Same inode: the source's hash is the snapshot's hash.
		sum, st, at, err := p.hashes.hashFile(dst)
		if err != nil {
			return err
		}
		in.SHA256, in.Size, in.ModTime, in.HashedAt = sum, st.Size(), st.ModTime().UnixNano(), at
		return nil
	}
	at := time.Now().UnixNano() // dst is new: every byte in it is written after this
	sum, err := copyHashing(ctx, src, dst)
	if err != nil {
		os.Remove(dst)
		return err
	}
	st, err := os.Stat(dst)
	if err != nil {
		return err
	}
	in.SHA256, in.Size, in.ModTime, in.HashedAt = sum, st.Size(), st.ModTime().UnixNano(), at
	return nil
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(b []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(b)
}

func copyHashing(ctx context.Context, src, dst string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, h), ctxReader{ctx, in})
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verifyInputs re-checks, just before launch, that every input still holds
// the approved bytes. A cheap size+mtime comparison covers the normal case,
// but only for files last modified well before they were hashed: a write in
// the same timestamp tick as the hash would leave size and mtime unchanged.
func (p *Provider) verifyInputs(j *job) error {
	for _, in := range j.Inputs {
		ref, err := files.ParseRef(files.RootWorkspaces + "/" + j.Workspace + "/" + in.Rel)
		if err != nil {
			return err
		}
		abs, err := files.Resolve(p.paths, ref)
		if err != nil {
			return err
		}
		st, err := os.Stat(abs)
		if err != nil {
			return fmt.Errorf("input %s is gone from the workspace: %w", in.Ref, err)
		}
		if st.Size() == in.Size && st.ModTime().UnixNano() == in.ModTime && statClean(in.ModTime, in.HashedAt) {
			continue
		}
		sum, _, _, err := p.hashes.hashFile(abs)
		if err != nil {
			return fmt.Errorf("input %s: %w", in.Ref, err)
		}
		if sum != in.SHA256 {
			return fmt.Errorf("input %s changed after it was approved", in.Ref)
		}
	}
	return nil
}

// --- scheduling ---

func (p *Provider) loop() {
	defer p.wg.Done()
	t := time.NewTicker(p.opts.PollInterval)
	defer t.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-p.wake:
		case <-t.C:
		}
		p.schedule()
	}
}

func (p *Provider) queuedLocked() []*job {
	var q []*job
	for _, j := range p.jobs {
		if j.State == StateQueued {
			q = append(q, j)
		}
	}
	sort.Slice(q, func(a, b int) bool { return q[a].seq < q[b].seq })
	return q
}

func (c Claims) needsProbe() bool { return len(c.GPUs) > 0 || c.VRAMMB > 0 || c.MemMB > 0 }

// claimKeys name the contended resources of a claim set; two queued jobs
// conflict when they share a key.
func claimKeys(c Claims) []string {
	var k []string
	for _, g := range c.GPUs {
		k = append(k, fmt.Sprintf("gpu %d", g))
	}
	if c.VRAMMB > 0 {
		k = append(k, "vram")
	}
	if c.MemMB > 0 {
		k = append(k, "ram")
	}
	if c.CPUs > 0 {
		k = append(k, "cpu threads")
	}
	return k
}

// schedule starts every queued job that fits and is not queued behind an
// earlier job that wants the same resource.
func (p *Provider) schedule() {
	p.schedMu.Lock()
	defer p.schedMu.Unlock()

	p.mu.Lock()
	q := p.queuedLocked()
	p.mu.Unlock()
	if len(q) == 0 {
		return
	}
	probe := false
	for _, j := range q {
		probe = probe || j.Claims.needsProbe()
	}
	var snap Snapshot
	var perr error
	if probe {
		ctx, cancel := context.WithTimeout(p.ctx, 30*time.Second)
		snap, perr = p.res.Snapshot(ctx)
		cancel()
		if p.ctx.Err() != nil {
			return
		}
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closing {
		return
	}
	ahead := map[string]string{} // contended resource → earlier waiting job
	for _, j := range p.queuedLocked() {
		keys := claimKeys(j.Claims)
		blocked := ""
		for _, k := range keys {
			if by, ok := ahead[k]; ok {
				blocked = fmt.Sprintf("waiting for %s, which %s is queued for first", k, by)
				break
			}
		}
		if blocked == "" {
			fatal, wait := p.fitsLocked(j, snap, perr)
			switch {
			case fatal != "":
				p.finishLocked(j, StateFailed, fatal, nil)
				continue
			case wait == "":
				p.startLocked(j)
				continue
			default:
				blocked = wait
			}
		}
		if j.waiting != blocked {
			j.waiting = blocked
		}
		for _, k := range keys {
			if _, ok := ahead[k]; !ok {
				ahead[k] = j.ID
			}
		}
	}
}

// fitsLocked decides whether j can start now. fatal means it never will on
// this device; wait explains why not yet.
func (p *Provider) fitsLocked(j *job, snap Snapshot, perr error) (fatal, wait string) {
	c := j.Claims
	if p.running >= p.opts.MaxConcurrent {
		return "", fmt.Sprintf("waiting for a free slot (%d jobs running)", p.running)
	}
	if c.CPUs > p.opts.CPUBudget {
		return fmt.Sprintf("needs %d CPU threads but this device lets jobs claim %d", c.CPUs, p.opts.CPUBudget), ""
	}
	if p.cpusUsed+c.CPUs > p.opts.CPUBudget {
		return "", fmt.Sprintf("waiting for CPU threads (%d of %d claimed)", p.cpusUsed, p.opts.CPUBudget)
	}
	if c.needsProbe() && perr != nil {
		return "cannot read this device's hardware state: " + perr.Error(), ""
	}

	gpuByIdx := map[int]GPUState{}
	for _, g := range snap.GPUs {
		gpuByIdx[g.Index] = g
	}
	for _, idx := range c.GPUs {
		if _, ok := gpuByIdx[idx]; !ok {
			return fmt.Sprintf("GPU %d does not exist on this device", idx), ""
		}
	}
	for _, idx := range c.GPUs {
		if by := p.gpuBusy[idx]; by != "" {
			return "", fmt.Sprintf("waiting for GPU %d (in use by %s)", idx, by)
		}
	}

	var recentAll, recentUnpinned, recentMem int64
	for _, r := range p.jobs {
		if r.State == StateRunning && time.Since(r.startAt) < p.opts.SettleTime {
			recentAll += r.Claims.VRAMMB
			if len(r.Claims.GPUs) == 0 {
				recentUnpinned += r.Claims.VRAMMB
			}
			recentMem += r.Claims.MemMB
		}
	}

	if c.VRAMMB > 0 {
		cands := snap.GPUs
		if len(c.GPUs) > 0 {
			cands = nil
			for _, idx := range c.GPUs {
				cands = append(cands, gpuByIdx[idx])
			}
		}
		if len(cands) == 0 {
			return "needs VRAM but this device has no GPU", ""
		}
		fits, bigEnough, known := 0, 0, 0
		for _, g := range cands {
			if g.TotalMB >= c.VRAMMB {
				bigEnough++
			}
			if !g.FreeKnown {
				continue
			}
			known++
			reserved := recentUnpinned
			if len(c.GPUs) == 0 {
				reserved = recentAll
			}
			if g.FreeMB-reserved >= c.VRAMMB {
				fits++
			}
		}
		need := 1 // an unpinned job needs one suitable GPU; pinned jobs need every claimed one
		if len(c.GPUs) > 0 {
			need = len(cands)
		}
		if bigEnough < need {
			return fmt.Sprintf("needs %d MB VRAM but the GPU(s) it can use have less in total", c.VRAMMB), ""
		}
		if known == 0 {
			return "cannot read free VRAM on this device (no nvidia-smi)", ""
		}
		if fits < need {
			return "", fmt.Sprintf("waiting for %d MB of free VRAM", c.VRAMMB)
		}
	}
	if c.MemMB > 0 {
		if c.MemMB > snap.MemTotalMB {
			return fmt.Sprintf("needs %d MB RAM but the device has %d MB", c.MemMB, snap.MemTotalMB), ""
		}
		if snap.MemAvailMB-recentMem < c.MemMB {
			return "", fmt.Sprintf("waiting for %d MB of free RAM (%d MB available)", c.MemMB, snap.MemAvailMB-recentMem)
		}
	}
	return "", ""
}

func (p *Provider) startLocked(j *job) {
	now := time.Now()
	j.State, j.Started, j.startAt, j.waiting = StateRunning, &now, now, ""
	j.claimed = true
	for _, g := range j.Claims.GPUs {
		p.gpuBusy[g] = j.ID
	}
	p.cpusUsed += j.Claims.CPUs
	p.running++
	p.holdSleepLocked()
	p.touchLocked(j)
	p.wg.Add(1)
	go p.run(j)
}

// finishLocked moves j to a final state exactly once, releasing whatever it
// held.
func (p *Provider) finishLocked(j *job, st State, reason string, exit *int) {
	if j.State.Terminal() {
		return
	}
	if j.State == StateRunning {
		p.running--
		if p.running == 0 {
			p.inh.Release()
		}
	}
	if j.claimed {
		j.claimed = false
		for _, g := range j.Claims.GPUs {
			if p.gpuBusy[g] == j.ID {
				delete(p.gpuBusy, g)
			}
		}
		p.cpusUsed -= j.Claims.CPUs
	}
	now := time.Now()
	j.State, j.Reason, j.Finished, j.ExitCode, j.waiting = st, reason, &now, exit, ""
	j.PID, j.PIDToken, j.Unit = 0, "", ""
	j.proc = nil
	j.cancel()
	p.touchLocked(j)
	p.kick()
}

// --- running ---

func (p *Provider) run(j *job) {
	defer p.wg.Done()
	fail := func(format string, a ...any) {
		p.mu.Lock()
		p.finishLocked(j, StateFailed, fmt.Sprintf(format, a...), nil)
		p.mu.Unlock()
	}

	if err := p.verifyInputs(j); err != nil {
		fail("%v", err)
		return
	}
	wsDir, err := workspaceDir(p.paths, j.Workspace)
	if err != nil {
		fail("%v", err)
		return
	}
	dir, err := p.cwdDir(j.Workspace, j.Cwd)
	if err != nil {
		fail("working directory: %v", err)
		return
	}
	logDir := p.jobDir(j.ID)
	outSink, err := newLogSink(logDir, "stdout", p.opts.HeadBytes, p.opts.SegmentBytes)
	if err != nil {
		fail("open log: %v", err)
		return
	}
	errSink, err := newLogSink(logDir, "stderr", p.opts.HeadBytes, p.opts.SegmentBytes)
	if err != nil {
		outSink.Close()
		fail("open log: %v", err)
		return
	}
	defer outSink.Close()
	defer errSink.Close()

	or, ow, err := os.Pipe()
	if err != nil {
		fail("pipe: %v", err)
		return
	}
	er, ew, err := os.Pipe()
	if err != nil {
		or.Close()
		ow.Close()
		fail("pipe: %v", err)
		return
	}

	proc, err := startProcess(launchSpec{
		ID: j.ID, Path: j.Path, Args: j.Args, Shell: j.Shell, Line: j.Line,
		Dir: dir, Env: jobEnv(os.Environ(), &j.Job, wsDir), Stdout: ow, Stderr: ew,
		MemLimitMB: j.Claims.MemMB, KillGrace: p.opts.KillGrace, NoScope: p.opts.NoScope,
	})
	ow.Close()
	ew.Close()
	if err != nil {
		or.Close()
		er.Close()
		fail("could not start %s: %v", filepath.Base(j.Path), err)
		return
	}

	var copiers sync.WaitGroup
	copiers.Add(2)
	go func() { defer copiers.Done(); io.Copy(outSink, or); or.Close() }()
	go func() { defer copiers.Done(); io.Copy(errSink, er); er.Close() }()

	p.mu.Lock()
	j.proc = proc
	j.PID, j.PIDToken, j.Unit, j.Notes = proc.PID(), proc.Token(), proc.Unit(), proc.Notes()
	p.touchLocked(j)
	pending := j.kill
	p.mu.Unlock()
	if pending != killNone { // cancelled between "running" and the process existing
		proc.Terminate()
	}

	var timer *time.Timer
	if j.TimeoutSec > 0 {
		timer = time.AfterFunc(time.Duration(j.TimeoutSec)*time.Second, func() {
			p.mu.Lock()
			if j.kill == killNone {
				j.kill = killTimeout
			}
			p.mu.Unlock()
			proc.Terminate()
		})
	}
	exit, werr := proc.Wait()
	if timer != nil {
		timer.Stop()
	}

	// The tree is gone, so the pipes close; bound the wait anyway.
	done := make(chan struct{})
	go func() { copiers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		or.Close()
		er.Close()
		<-done
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	code := exit
	switch {
	case j.kill == killCancel:
		p.finishLocked(j, StateCancelled, "cancelled", &code)
	case j.kill == killTimeout:
		p.finishLocked(j, StateFailed, fmt.Sprintf("timed out after %ds", j.TimeoutSec), &code)
	case j.kill == killShutdown:
		p.finishLocked(j, StateInterrupted, "node shut down", &code)
	case werr != nil:
		p.finishLocked(j, StateFailed, "wait failed: "+werr.Error(), nil)
	case exit == 0:
		p.finishLocked(j, StateSucceeded, "", &code)
	case exit < 0:
		p.finishLocked(j, StateFailed, "killed by a signal", &code)
	default:
		p.finishLocked(j, StateFailed, fmt.Sprintf("exited with code %d", exit), &code)
	}
}

// cancelLocked stops j wherever it is in its life.
func (p *Provider) cancelLocked(j *job) {
	switch j.State {
	case StateSubmitted, StateAwaitingApproval:
		j.ticket.Cancel()
		p.finishLocked(j, StateCancelled, "cancelled", nil)
	case StateQueued:
		p.finishLocked(j, StateCancelled, "cancelled", nil)
	case StateRunning:
		if j.kill == killNone {
			j.kill = killCancel
		}
		if j.proc != nil {
			j.proc.Terminate()
		}
	}
}
