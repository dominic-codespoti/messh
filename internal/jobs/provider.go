// Package jobs runs approved programs on this device as background jobs:
// queued against GPU/RAM/CPU claims, confined to a workspace under the state
// directory, killed as a whole tree when cancelled, and kept (with logs and
// outputs) until their owner deletes them.
package jobs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/provider"
	"messh/internal/state"
)

// Options configures a Provider. Zero values pick production defaults; the
// hooks exist so tests can run without hardware.
type Options struct {
	Paths state.Paths
	Log   *slog.Logger

	Resources Resources      // default: live sysinfo
	Inhibitor SleepInhibitor // default: the OS sleep inhibitor

	MaxConcurrent int           // most jobs running at once (default NumCPU)
	CPUBudget     int           // CPU threads jobs may claim (default NumCPU)
	KillGrace     time.Duration // SIGTERM → SIGKILL delay on Linux (default 5s)
	SettleTime    time.Duration // how long a new job's vram/mem claim is assumed not yet visible in probes (default 15s)
	PollInterval  time.Duration // queue re-check period while jobs wait (default 3s)
	HeadBytes     int64         // per-stream log head kept forever (default 4 MiB)
	SegmentBytes  int64         // per-stream rolling tail segment (default 4 MiB)
	NoScope       bool          // never use systemd-run scopes (tests)
}

type killKind string

const (
	killNone     killKind = ""
	killCancel   killKind = "cancel"
	killTimeout  killKind = "timeout"
	killShutdown killKind = "shutdown"
)

// job is a Job plus its in-memory control state. Everything is guarded by
// Provider.mu.
type job struct {
	Job
	seq      uint64
	ticket   provider.Ticket
	ctx      context.Context // ends the approval wait / input snapshot
	cancel   context.CancelFunc
	changed  chan struct{} // closed and replaced on every state change
	ready    chan struct{} // closed once inputs are snapshotted and the request verified
	proc     process
	kill     killKind
	claimed  bool
	waiting  string // why a queued job is not running yet
	startAt  time.Time
	deleting bool // job_delete is removing its files
}

type pendingKey struct{ owner, exact string }

// Provider implements provider.Provider and provider.Gated for the job tools.
type Provider struct {
	opts  Options
	log   *slog.Logger
	paths state.Paths
	res   Resources
	inh   SleepInhibitor

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	wake   chan struct{}

	schedMu sync.Mutex // one scheduling pass at a time

	mu       sync.Mutex
	jobs     map[string]*job
	nextSeq  uint64
	running  int
	cpusUsed int
	gpuBusy  map[int]string // gpu index → job id
	pending  map[pendingKey]pendingVal
	deleting map[string]bool // workspaces being removed
	closing  bool
	hashes   hashCache
	sleepErr string // last sleep-inhibitor Hold failure, logged once; "" after a good hold

	closeOnce sync.Once
}

type pendingVal struct {
	n   int
	exp time.Time
}

var (
	_ provider.Provider = (*Provider)(nil)
	_ provider.Gated    = (*Provider)(nil)
)

// New loads existing jobs (marking any that were live as interrupted) and
// starts the scheduler. ctx bounds the provider's lifetime; Close ends it
// early.
func New(ctx context.Context, opts Options) (*Provider, error) {
	if opts.Paths.Root == "" {
		return nil, errors.New("jobs: state directory required")
	}
	if opts.Log == nil {
		opts.Log = slog.Default()
	}
	if opts.Resources == nil {
		opts.Resources = newSysResources()
	}
	if opts.Inhibitor == nil {
		opts.Inhibitor = newSleepInhibitor(opts.Log)
	}
	if opts.MaxConcurrent <= 0 {
		opts.MaxConcurrent = runtime.NumCPU()
	}
	if opts.CPUBudget <= 0 {
		opts.CPUBudget = runtime.NumCPU()
	}
	if opts.KillGrace <= 0 {
		opts.KillGrace = 5 * time.Second
	}
	if opts.SettleTime <= 0 {
		opts.SettleTime = 15 * time.Second
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 3 * time.Second
	}
	if opts.HeadBytes <= 0 {
		opts.HeadBytes = defaultHeadBytes
	}
	if opts.SegmentBytes <= 0 {
		opts.SegmentBytes = defaultSegmentBytes
	}
	if err := os.MkdirAll(opts.Paths.JobsDir(), 0o700); err != nil {
		return nil, err
	}
	pctx, cancel := context.WithCancel(ctx)
	p := &Provider{
		opts: opts, log: opts.Log, paths: opts.Paths, res: opts.Resources, inh: opts.Inhibitor,
		ctx: pctx, cancel: cancel, wake: make(chan struct{}, 1),
		jobs: map[string]*job{}, gpuBusy: map[int]string{},
		pending: map[pendingKey]pendingVal{}, deleting: map[string]bool{},
	}
	if err := p.load(); err != nil {
		cancel()
		return nil, err
	}
	p.wg.Add(1)
	go p.loop()
	// A closed ctx must release running jobs just like Close does.
	go func() {
		<-pctx.Done()
		p.Close()
	}()
	return p, nil
}

func (p *Provider) Name() string { return "jobs" }

func (p *Provider) jobDir(id string) string { return filepath.Join(p.paths.JobsDir(), id) }

func newID() string {
	var b [4]byte
	rand.Read(b[:])
	return "job-" + hex.EncodeToString(b[:])
}

// load reads job records left by earlier runs. Anything that was still live
// was cut short by the restart.
func (p *Provider) load() error {
	entries, err := os.ReadDir(p.paths.JobsDir())
	if err != nil {
		return err
	}
	var loaded []*job
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(p.jobDir(e.Name()), "job.json"))
		if err != nil {
			continue
		}
		var rec Job
		if err := json.Unmarshal(data, &rec); err != nil || rec.ID != e.Name() || !rec.State.valid() {
			p.log.Warn("ignoring unreadable job record", "dir", e.Name(), "error", err)
			continue
		}
		j := &job{Job: rec, changed: make(chan struct{}), ready: make(chan struct{})}
		j.ctx, j.cancel = context.WithCancel(context.Background())
		close(j.ready)
		loaded = append(loaded, j)
	}
	sort.Slice(loaded, func(a, b int) bool { return loaded[a].Submitted.Before(loaded[b].Submitted) })
	for _, j := range loaded {
		p.nextSeq++
		j.seq = p.nextSeq
		if !j.State.Terminal() {
			if j.State == StateRunning {
				killLeftover(j.PID, j.PIDToken, j.Unit)
			}
			now := time.Now()
			j.State, j.Reason, j.Finished = StateInterrupted, "node restarted", &now
			j.PID, j.PIDToken, j.Unit = 0, "", ""
			p.persistLocked(j)
		}
		j.cancel()
		p.jobs[j.ID] = j
	}
	return nil
}

func (p *Provider) persistLocked(j *job) {
	data, err := json.MarshalIndent(&j.Job, "", "  ")
	if err == nil {
		err = state.WriteFileAtomic(filepath.Join(p.jobDir(j.ID), "job.json"), data, 0o600)
	}
	if err != nil {
		p.log.Error("persist job", "job", j.ID, "error", err)
	}
}

// touchLocked persists the job and wakes anyone waiting on it.
func (p *Provider) touchLocked(j *job) {
	p.persistLocked(j)
	close(j.changed)
	j.changed = make(chan struct{})
}

func (p *Provider) kick() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

// Approval builds the prompt and the hashes for a job_submit. Nothing is
// created on disk; Call re-derives and verifies the same request.
func (p *Provider) Approval(ctx context.Context, tool string, args json.RawMessage, caller provider.Caller) (provider.Approval, error) {
	if tool != toolSubmit {
		return provider.Approval{}, fmt.Errorf("%s needs no approval", tool)
	}
	req, err := p.parseSubmit(args)
	if err != nil {
		return provider.Approval{}, err
	}
	if err := p.hashInputs(req, req.Workspace); err != nil {
		return provider.Approval{}, err
	}
	shown := req.Workspace
	if shown == "" {
		shown = "<new, named after the job id>"
	}
	ap := req.approval(shown)
	p.addPending(ownerKey(ownerOf(caller)), ap.Exact)
	return ap, nil
}

func ownerKey(o Owner) string { return o.DeviceID + "\x00" + o.Agent }

const pendingTTL = 2 * time.Minute

func (p *Provider) addPending(owner, exact string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	for k, v := range p.pending {
		if now.After(v.exp) {
			delete(p.pending, k)
		}
	}
	k := pendingKey{owner, exact}
	v := p.pending[k]
	p.pending[k] = pendingVal{n: v.n + 1, exp: now.Add(pendingTTL)}
}

func (p *Provider) takePending(owner, exact string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	k := pendingKey{owner, exact}
	v, ok := p.pending[k]
	if !ok || time.Now().After(v.exp) {
		delete(p.pending, k)
		return false
	}
	if v.n <= 1 {
		delete(p.pending, k)
	} else {
		v.n--
		p.pending[k] = v
	}
	return true
}

func (p *Provider) Call(ctx context.Context, tool string, args json.RawMessage, caller provider.Caller) (*mcp.CallToolResult, error) {
	switch tool {
	case toolSubmit:
		return p.callSubmit(ctx, args, caller)
	case toolStatus:
		return p.callStatus(ctx, args, caller)
	case toolWait:
		return p.callWait(ctx, args, caller)
	case toolLogs:
		return p.callLogs(args, caller)
	case toolCancel:
		return p.callCancel(ctx, args, caller)
	case toolList:
		return p.callList(args, caller)
	case toolDelete:
		return p.callDelete(args, caller)
	case toolResources:
		return p.callResources(ctx, caller)
	}
	return nil, fmt.Errorf("unknown tool %q", tool)
}

// Close cancels everything: pending approvals are withdrawn, running jobs
// lose their process trees, and Close returns once they have all stopped.
func (p *Provider) Close() {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closing = true
		for _, j := range p.jobs {
			switch j.State {
			case StateRunning:
				j.kill = killShutdown
				if j.proc != nil {
					j.proc.Terminate()
				}
			case StateSubmitted, StateAwaitingApproval, StateQueued:
				if j.ticket != nil {
					j.ticket.Cancel()
				}
				p.finishLocked(j, StateInterrupted, "node shut down", nil)
			}
		}
		p.mu.Unlock()
		p.cancel()
		p.wg.Wait()
		if c, ok := p.inh.(interface{ Close() }); ok {
			c.Close()
		}
	})
}
