// Package browser lets agents on other devices drive a web browser on this
// one through Microsoft's Playwright MCP server, with the device owner
// approving each website.
//
// The provider supervises a Playwright MCP child process (started on the
// first call, stopped when idle), publishes a curated allowlist of its tools
// as class "browser", and puts messh's per-website approval in front of every
// call: it works out which website the call touches, asks (or applies
// allow_origins / deny_origins), and after any call that can change the page
// it checks where the browser ended up before returning a word of content.
//
// This is human approval, not a sandbox: a page the owner approved can still
// send whatever is on it anywhere, and the browser runs with the owner's
// network access.
package browser

import (
	"context"
	_ "embed"
	"encoding/json"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/provider"
	"messh/internal/state"
)

//go:embed tools.json
var toolsJSON []byte

// ToolStatus is the info-class tool that reports the browser's state.
const ToolStatus = "browser_status"

// Asker obtains a second approval while a call is running: the page ended up
// somewhere the first approval did not cover. The node implements it with its
// approval engine, so it prompts, applies saved rules, and is audited like any
// other gated request.
type Asker func(ctx context.Context, caller provider.Caller, tool string, ap provider.Approval) (allowed bool, reason string, err error)

// Options configures a Provider.
type Options struct {
	Paths state.Paths
	Log   *slog.Logger
	// Ask is used for approvals needed mid-call. Nil denies them.
	Ask Asker
	// ReloadEvery is how often browser.json is checked; default 2s.
	ReloadEvery time.Duration

	launch launcher // tests
}

// Provider implements provider.Provider, provider.Gated and provider.Notifier.
type Provider struct {
	paths state.Paths
	log   *slog.Logger
	ask   Asker
	every time.Duration
	up    *upstream

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	busy   chan struct{} // one browser action at a time

	mu       sync.RWMutex
	cfg      Config
	cfgErrs  []string
	stamp    fileStamp
	tools    []provider.Tool
	sig      string
	onChange func()
	closed   bool

	apprMu   sync.Mutex
	approved map[string][]approvedCall
}

type fileStamp struct {
	exists bool
	mod    time.Time
	size   int64
}

func statFile(path string) fileStamp {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{}
	}
	return fileStamp{exists: true, mod: fi.ModTime(), size: fi.Size()}
}

var (
	_ provider.Provider = (*Provider)(nil)
	_ provider.Gated    = (*Provider)(nil)
	_ provider.Notifier = (*Provider)(nil)
)

// New loads browser.json and starts watching it. No browser is started until
// the first call.
func New(ctx context.Context, opts Options) (*Provider, error) {
	log := opts.Log
	if log == nil {
		log = slog.Default()
	}
	ctx, cancel := context.WithCancel(ctx)
	p := &Provider{
		paths: opts.Paths, log: log, ask: opts.Ask, every: opts.ReloadEvery,
		ctx: ctx, cancel: cancel, busy: make(chan struct{}, 1),
		approved: map[string][]approvedCall{},
	}
	if p.every <= 0 {
		p.every = 2 * time.Second
	}
	p.up = newUpstream(ctx, opts.Paths, log, opts.launch)
	p.up.reapLeftover()
	if err := p.reload(true); err != nil {
		log.Warn("browser.json", "error", err)
	}
	p.wg.Add(1)
	go p.watch()
	return p, nil
}

// Name implements provider.Provider.
func (p *Provider) Name() string { return "browser" }

// SetOnChange implements provider.Notifier.
func (p *Provider) SetOnChange(fn func()) {
	p.mu.Lock()
	p.onChange = fn
	p.mu.Unlock()
}

// Close stops the browser and the config watcher.
func (p *Provider) Close() {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.cancel()
	p.wg.Wait()
	p.up.close()
}

// Stop ends the running browser now, for example so the owner can sign in to
// the agent profile. It reports whether one was running.
func (p *Provider) Stop() bool { return p.up.stop() }

// Tools implements provider.Provider.
func (p *Provider) Tools() []provider.Tool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return slices.Clone(p.tools)
}

func (p *Provider) config() Config {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cfg
}

func (p *Provider) watch() {
	defer p.wg.Done()
	t := time.NewTicker(p.every)
	defer t.Stop()
	for {
		select {
		case <-p.ctx.Done():
			return
		case <-t.C:
			if err := p.reload(false); err != nil {
				p.log.Warn("browser.json", "error", err)
			}
		}
	}
}

// reload applies browser.json when it changed. A file that cannot be read
// keeps the previous configuration, so a half-saved edit does not drop the
// deny list; the problem is reported by browser_status and the log.
func (p *Provider) reload(force bool) error {
	path := p.paths.BrowserFile()
	st := statFile(path)
	p.mu.RLock()
	same := !force && st == p.stamp
	p.mu.RUnlock()
	if same {
		return nil
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		p.mu.Lock()
		p.stamp = st
		p.cfgErrs = []string{err.Error()}
		p.mu.Unlock()
		return err
	}
	problems := cfg.Validate()
	if len(problems) > 0 {
		// Do not run on a configuration with errors: the lists might not mean
		// what the owner wrote.
		cfg.Enabled = false
	}
	cfg.Mode, cfg.Channel = cfg.Normalized().Mode, cfg.Normalized().Channel

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil
	}
	p.stamp, p.cfg, p.cfgErrs = st, cfg, problems
	p.mu.Unlock()
	for _, msg := range problems {
		p.log.Warn("ignoring browser.json", "problem", msg)
	}
	if !cfg.Enabled {
		p.up.stop()
	} else {
		p.up.setIdle(cfg.Idle())
	}
	p.rebuild()
	return nil
}

func (p *Provider) rebuild() {
	tools := buildTools(p.config())
	sig := toolsSignature(tools)
	p.mu.Lock()
	changed := sig != p.sig
	p.tools, p.sig = tools, sig
	fn := p.onChange
	p.mu.Unlock()
	if changed && fn != nil {
		go fn()
	}
}

func toolsSignature(tools []provider.Tool) string {
	var b strings.Builder
	for _, t := range tools {
		b.WriteString(t.Def.Name)
		b.WriteByte(',')
	}
	return b.String()
}

// --- published tools -----------------------------------------------------

type toolDef struct {
	Name        string          `json:"name"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	ReadOnly    bool            `json:"readOnly"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

var upstreamDefs = func() map[string]toolDef {
	var list []toolDef
	if err := json.Unmarshal(toolsJSON, &list); err != nil {
		panic("browser: tools.json: " + err.Error())
	}
	m := make(map[string]toolDef, len(list))
	for _, d := range list {
		m[d.Name] = d
	}
	return m
}()

const approvalNote = "Needs the device owner's approval for the website involved (view / interact). "

// descriptionOverrides replace upstream wording where messh changes behaviour.
var descriptionOverrides = map[string]string{
	"browser_tabs":            "List, open, close or switch browser tabs. `list` shows each tab's number and website only (no titles or paths). `new` with a url and `select` need approval for that website.",
	"browser_file_upload":     "Upload files into the file chooser that is open (click the page's file input first). Pass files as ws/... or artifacts/... references on this device, e.g. a file copied here with mesh_copy. Uploading needs its own approval.",
	"browser_take_screenshot": "Take a screenshot of the current page. Saved as an artifact (artifacts/browser/....png) and returned as that reference; small images are also returned inline. You can't perform actions based on the screenshot, use browser_snapshot for actions.",
	"browser_pdf_save":        "Save the current page as a PDF, returned as an artifacts/browser/....pdf reference.",
	"browser_evaluate":        "Evaluate JavaScript in the page or on an element. Runs with the page's own privileges, so it needs its own approval.",
	"browser_navigate":        "Navigate the current tab to a URL (http or https only).",
}

func buildTools(cfg Config) []provider.Tool {
	if !cfg.Active() {
		return nil
	}
	var out []provider.Tool
	for _, name := range allowlisted() {
		if name == "browser_evaluate" && !cfg.AllowScript {
			continue
		}
		d, ok := upstreamDefs[name]
		if !ok {
			continue
		}
		desc := d.Description
		if o, ok := descriptionOverrides[name]; ok {
			desc = o
		}
		schema := d.InputSchema
		if name == "browser_file_upload" {
			schema = json.RawMessage(uploadSchema)
		}
		out = append(out, provider.Tool{
			Def: &mcp.Tool{
				Name:        name,
				Title:       d.Title,
				Description: approvalNote + desc,
				InputSchema: schema,
				Annotations: &mcp.ToolAnnotations{Title: d.Title, ReadOnlyHint: d.ReadOnly, OpenWorldHint: new(true)},
			},
			Class: provider.ClassBrowser,
		})
	}
	out = append(out, provider.Tool{
		Def: &mcp.Tool{
			Name:        ToolStatus,
			Title:       "Browser status",
			Description: "Report whether the browser tools work on this device: mode (agent profile or the owner's main browser), channel, whether the browser is running, the websites of the open tabs, and configuration problems. Never starts the browser.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, Title: "Browser status"},
		},
		Class: provider.ClassInfo,
	})
	return out
}

const uploadSchema = `{"type":"object","properties":{"paths":{"description":"Files to upload, as ws/... or artifacts/... references on this device. If omitted, the file chooser is cancelled.","type":"array","items":{"type":"string"}}},"additionalProperties":false}`

// --- status ----------------------------------------------------------------

// Status is what browser_status and `messh browser status` report.
type Status struct {
	Enabled   bool     `json:"enabled"`
	Mode      string   `json:"mode,omitempty"`
	Channel   string   `json:"channel,omitempty"`
	Headless  bool     `json:"headless,omitempty"`
	Running   bool     `json:"running"`
	Starting  bool     `json:"starting,omitempty"` // the browser is being started (first start downloads Playwright MCP)
	Upstream  string   `json:"upstream,omitempty"` // Playwright version the running server reports
	Tabs      []string `json:"open_tabs,omitempty"`
	Script    bool     `json:"script_allowed"`
	Idle      string   `json:"idle_timeout,omitempty"`
	Problems  []string `json:"problems,omitempty"`
	LastError string   `json:"last_error,omitempty"`
}

// Status reports the current state without starting the browser.
func (p *Provider) Status(ctx context.Context) Status {
	p.mu.RLock()
	cfg, problems := p.cfg, slices.Clone(p.cfgErrs)
	p.mu.RUnlock()
	st := Status{Enabled: cfg.Active(), Problems: problems}
	if cfg.Enabled {
		n := cfg.Normalized()
		st.Mode, st.Channel, st.Headless, st.Script = n.Mode, n.Channel, n.Headless, n.AllowScript
		st.Idle = n.Idle().String()
		if n.Idle() == 0 {
			st.Idle = "never"
		}
	}
	if s := p.up.running(); s != nil && cfg.Active() {
		st.Running = true
		st.Upstream = s.version
		if tabs, err := p.listTabs(ctx); err == nil {
			for _, t := range tabs {
				site := tabSite(t)
				if t.Current {
					site += " (current)"
				}
				st.Tabs = append(st.Tabs, site)
			}
		}
	}
	p.up.mu.Lock()
	st.LastError = p.up.lastErr
	st.Starting = p.up.starting != nil
	p.up.mu.Unlock()
	return st
}

func (p *Provider) statusResult(ctx context.Context) (*mcp.CallToolResult, error) {
	return provider.JSONResult(p.Status(ctx))
}

// Describe renders the mode for prompts.
func modeDescription(mode string) string {
	if mode == ModeExtension {
		return "your main browser, with your logged-in sessions (Playwright extension)"
	}
	return "a separate agent-only browser profile (none of your main browser's sessions)"
}
