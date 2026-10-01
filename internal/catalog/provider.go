package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/provider"
	"messh/internal/state"
)

// Options configures a Provider. Zero durations select the defaults.
type Options struct {
	Paths state.Paths
	Log   *slog.Logger

	ReloadEvery  time.Duration // services file mtime poll (2s)
	ProbeEvery   time.Duration // health probe interval while up (15s)
	ProbeTimeout time.Duration // one probe (5s)
	MaxBackoff   time.Duration // longest wait between probes of a down service (2m)
	Debounce     time.Duration // quiet period before the change callback (250ms)
	// InitialWait bounds how long New waits for the first probes (3s).
	InitialWait time.Duration
}

func (o *Options) defaults() {
	set := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	set(&o.ReloadEvery, 2*time.Second)
	set(&o.ProbeEvery, 15*time.Second)
	set(&o.ProbeTimeout, 5*time.Second)
	set(&o.MaxBackoff, 2*time.Minute)
	set(&o.Debounce, 250*time.Millisecond)
	set(&o.InitialWait, 3*time.Second)
	if o.Log == nil {
		o.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
}

// Provider exposes the registered services as tools. It implements
// provider.Provider, provider.Gated and provider.Notifier.
type Provider struct {
	opts   Options
	paths  state.Paths
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.RWMutex
	entries  map[string]*entry
	cfgErrs  []string
	stamp    fileStamp
	tools    []provider.Tool
	proxyIdx map[string]string // local proxy tool name → service name
	sig      string
	onChange func()
	pending  bool
	timer    *time.Timer
	closed   bool
}

type fileStamp struct {
	exists bool
	mod    time.Time
	size   int64
}

var _ provider.Provider = (*Provider)(nil)
var _ provider.Gated = (*Provider)(nil)
var _ provider.Notifier = (*Provider)(nil)

// New starts the provider: it loads the services file, probes every enabled
// service (waiting briefly so the first Tools() is meaningful) and keeps
// reloading and probing until ctx is cancelled or Close is called.
func New(ctx context.Context, opts Options) (*Provider, error) {
	opts.defaults()
	ctx, cancel := context.WithCancel(ctx)
	p := &Provider{
		opts: opts, paths: opts.Paths, log: opts.Log.With("component", "catalog"),
		ctx: ctx, cancel: cancel,
		entries:  map[string]*entry{},
		proxyIdx: map[string]string{},
		tools:    []provider.Tool{{Def: catalogueTool(), Class: provider.ClassInfo}},
	}
	p.sig = signature(p.tools)
	if err := p.reload(true); err != nil {
		p.log.Warn("services file", "error", err)
	}
	p.mu.RLock()
	var firsts []chan struct{}
	for _, e := range p.entries {
		firsts = append(firsts, e.first)
	}
	p.mu.RUnlock()
	deadline := time.After(opts.InitialWait)
wait:
	for _, c := range firsts {
		select {
		case <-c:
		case <-deadline:
			break wait
		case <-ctx.Done():
			break wait
		}
	}
	p.mu.Lock()
	p.pending = false // nobody is listening yet; the node reads Tools() after registering
	p.mu.Unlock()
	p.wg.Add(1)
	go p.watch()
	return p, nil
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return "catalog" }

// SetOnChange implements provider.Notifier.
func (p *Provider) SetOnChange(fn func()) {
	p.mu.Lock()
	p.onChange = fn
	p.mu.Unlock()
}

// Close stops probing and closes upstream sessions.
func (p *Provider) Close() {
	p.mu.Lock()
	p.closed = true
	if p.timer != nil {
		p.timer.Stop()
	}
	p.mu.Unlock()
	p.cancel()
	p.wg.Wait()
	p.mu.Lock()
	entries := p.entries
	p.entries = map[string]*entry{}
	p.mu.Unlock()
	var wg sync.WaitGroup
	for _, e := range entries {
		wg.Add(1)
		go func() { defer wg.Done(); e.stop() }()
	}
	wg.Wait()
}

// Tools implements provider.Provider.
func (p *Provider) Tools() []provider.Tool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]provider.Tool(nil), p.tools...)
}

// watch polls the services file.
func (p *Provider) watch() {
	defer p.wg.Done()
	t := time.NewTicker(p.opts.ReloadEvery)
	defer t.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-t.C:
			if err := p.reload(false); err != nil {
				p.log.Warn("services file", "error", err)
			}
		}
	}
}

func statFile(path string) fileStamp {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{exists: true, mod: fi.ModTime(), size: fi.Size()}
}

// reload applies the services file when it changed. A file that cannot be
// parsed keeps the previous configuration so a half-saved edit does not
// drop every service; the problem is reported by catalogue.
func (p *Provider) reload(force bool) error {
	path := p.paths.ServicesFile()
	st := statFile(path)
	p.mu.RLock()
	same := !force && st == p.stamp
	p.mu.RUnlock()
	if same {
		return nil
	}
	f, err := LoadFile(path)
	if err != nil {
		p.mu.Lock()
		p.stamp = st
		p.cfgErrs = []string{err.Error()}
		p.mu.Unlock()
		return err
	}
	var errs []string
	want := map[string]Service{}
	for _, s := range f.Services {
		if err := s.Validate(); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if _, dup := want[s.Name]; dup {
			errs = append(errs, fmt.Sprintf("service %s is listed twice; the first entry is used", s.Name))
			continue
		}
		if s.Active() {
			want[s.Name] = s
		}
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.stamp = st
	p.cfgErrs = errs
	var retired []*entry
	for name, e := range p.entries {
		if w, ok := want[name]; !ok || !sameService(w, e.cfg) {
			retired = append(retired, e)
			delete(p.entries, name)
		}
	}
	for name, s := range want {
		if _, ok := p.entries[name]; ok {
			continue
		}
		e := newEntry(p, s)
		p.entries[name] = e
		p.wg.Add(1)
		go e.run()
	}
	p.mu.Unlock()
	for _, e := range retired {
		go e.stop()
	}
	for _, msg := range errs {
		p.log.Warn("ignoring service entry", "error", msg)
	}
	p.rebuild()
	return nil
}

func sameService(a, b Service) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// changed is called by entries after a probe; it refreshes the tool set.
func (p *Provider) changed() { p.rebuild() }

// rebuild recomputes the published tools from the entries' current state and
// schedules the change callback when the set differs from the last one.
func (p *Provider) rebuild() {
	p.mu.RLock()
	names := make([]string, 0, len(p.entries))
	for n := range p.entries {
		names = append(names, n)
	}
	sort.Strings(names)
	snaps := make([]snap, 0, len(names))
	for _, n := range names {
		snaps = append(snaps, p.entries[n].snapshot())
	}
	p.mu.RUnlock()

	var rest, restSpec, openai []string
	var proxies []provider.Tool
	idx := map[string]string{}
	for _, s := range snaps {
		if !s.h.Up {
			continue
		}
		switch s.cfg.Kind {
		case KindMCP:
			for _, pt := range s.proxies {
				proxies = append(proxies, provider.Tool{Def: pt.Def, Class: provider.ClassService})
				idx[pt.Name] = s.cfg.Name
			}
		case KindOpenAPI:
			rest = append(rest, s.cfg.Name)
			restSpec = append(restSpec, s.cfg.Name)
		case KindHTTP:
			rest = append(rest, s.cfg.Name)
		case KindOpenAI:
			openai = append(openai, s.cfg.Name)
		}
	}
	tools := []provider.Tool{{Def: catalogueTool(), Class: provider.ClassInfo}}
	for _, d := range genericTools(rest, restSpec, openai) {
		class := provider.ClassService
		tools = append(tools, provider.Tool{Def: d, Class: class})
	}
	tools = append(tools, proxies...)
	sig := signature(tools)

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.tools, p.proxyIdx = tools, idx
	if sig == p.sig {
		return
	}
	p.sig = sig
	p.pending = true
	if p.timer == nil {
		p.timer = time.AfterFunc(p.opts.Debounce, p.fire)
	} else {
		p.timer.Reset(p.opts.Debounce)
	}
}

func (p *Provider) fire() {
	p.mu.Lock()
	fn := p.onChange
	run := p.pending && fn != nil && !p.closed
	p.pending = false
	p.mu.Unlock()
	if run {
		fn()
	}
}

func signature(tools []provider.Tool) string {
	h := sha256.New()
	for _, t := range tools {
		b, _ := json.Marshal(t.Def)
		h.Write(b)
		h.Write([]byte(t.Class))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// lookup returns the named, enabled service.
func (p *Provider) lookup(name string) (*entry, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	e, ok := p.entries[name]
	if !ok {
		return nil, fmt.Errorf("no service named %q (see the catalogue tool)", name)
	}
	return e, nil
}

func (p *Provider) proxyOwner(tool string) (*entry, proxyTool, bool) {
	p.mu.RLock()
	svc, ok := p.proxyIdx[tool]
	e := p.entries[svc]
	p.mu.RUnlock()
	if !ok || e == nil {
		return nil, proxyTool{}, false
	}
	for _, pt := range e.snapshot().proxies {
		if pt.Name == tool {
			return e, pt, true
		}
	}
	return nil, proxyTool{}, false
}

// Call implements provider.Provider.
func (p *Provider) Call(ctx context.Context, tool string, args json.RawMessage, caller provider.Caller) (*mcp.CallToolResult, error) {
	if tool == toolCatalogue {
		return p.catalogue(args)
	}
	pl, err := p.plan(tool, args)
	if err != nil {
		return provider.ErrorResult("%v", err), nil
	}
	return pl.run(ctx)
}

// Approval implements provider.Gated.
func (p *Provider) Approval(ctx context.Context, tool string, args json.RawMessage, caller provider.Caller) (provider.Approval, error) {
	if tool == toolCatalogue {
		return provider.Approval{}, errors.New("catalogue needs no approval")
	}
	pl, err := p.plan(tool, args)
	if err != nil {
		return provider.Approval{}, err
	}
	return pl.approval(caller)
}
