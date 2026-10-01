package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	specRefresh = 5 * time.Minute
	maxSpecSize = 32 << 20
)

// health is the last observed state of a service.
type health struct {
	Up        bool
	Since     time.Time // when Up last changed
	Checked   time.Time
	Latency   time.Duration
	LastError string
	Fails     int
}

// entry is a registered service plus everything learned about it at runtime.
type entry struct {
	cfg    Service
	p      *Provider
	ctx    context.Context
	cancel context.CancelFunc
	http   *http.Client
	mcp    *mcpConn
	kick   chan struct{} // request an immediate re-probe
	first  chan struct{} // closed after the first probe finishes
	secret secretSource

	mu       sync.Mutex
	h        health
	upstream upstreamInfo
	upSig    string      // signature of the upstream tool list
	proxies  []proxyTool // published MCP tools
	upTools  []*mcp.Tool // raw upstream tools (for annotations)
	doc      *apiDoc     // OpenAPI document, when fetched
	docAt    time.Time
}

func newEntry(p *Provider, cfg Service) *entry {
	ctx, cancel := context.WithCancel(p.ctx)
	e := &entry{
		cfg: cfg, p: p, ctx: ctx, cancel: cancel,
		http:   newHTTPClient(cfg, p.paths.Root),
		kick:   make(chan struct{}, 1),
		first:  make(chan struct{}),
		secret: secretSource{auth: cfg.Auth, rootDir: p.paths.Root},
	}
	if cfg.Kind == KindMCP {
		e.mcp = newMCPConn(p.ctx, cfg.URL, e.http)
	}
	return e
}

// run probes until the entry is stopped: every ProbeEvery while up, backing
// off exponentially (capped) while down.
func (e *entry) run() {
	defer e.p.wg.Done()
	timer := time.NewTimer(0)
	defer timer.Stop()
	firstDone := false
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-timer.C:
		case <-e.kick:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		}
		e.probe()
		if !firstDone {
			firstDone = true
			close(e.first)
		}
		timer.Reset(e.nextDelay())
	}
}

func (e *entry) nextDelay() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	d := e.p.opts.ProbeEvery
	if !e.h.Up {
		for i := 0; i < min(e.h.Fails, 6) && d < e.p.opts.MaxBackoff; i++ {
			d *= 2
		}
		d = min(d, e.p.opts.MaxBackoff)
	}
	return d
}

// requestProbe asks for a prompt re-check (e.g. after a failed call).
func (e *entry) requestProbe() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

func (e *entry) stop() {
	e.cancel()
	if e.mcp != nil {
		e.mcp.close()
	}
	closeIdle(e.http)
}

func (e *entry) probe() {
	ctx, cancel := context.WithTimeout(e.ctx, e.p.opts.ProbeTimeout)
	defer cancel()
	start := time.Now()
	var err error
	if e.cfg.Kind == KindMCP {
		err = e.probeMCP(ctx)
	} else {
		err = e.probeHTTP(ctx)
	}
	if e.ctx.Err() != nil {
		return
	}
	e.record(err, time.Since(start))
}

// record stores a probe outcome and tells the provider when anything visible changed.
func (e *entry) record(err error, took time.Duration) {
	msg := ""
	if err != nil {
		msg = redactString(describeErr(err), e.secret.secrets())
	}
	e.mu.Lock()
	changed := e.h.Up != (err == nil) || e.h.Checked.IsZero()
	if changed {
		e.h.Since = time.Now()
	}
	e.h.Up = err == nil
	e.h.Checked = time.Now()
	e.h.Latency = took
	e.h.LastError = msg
	if err == nil {
		e.h.Fails = 0
	} else {
		e.h.Fails++
	}
	e.mu.Unlock()
	if err != nil && e.cfg.Kind == KindMCP {
		e.mcp.mu.Lock()
		sess := e.mcp.sess
		e.mcp.mu.Unlock()
		if sess != nil {
			e.mcp.drop(sess)
		}
	}
	e.p.changed()
}

func (e *entry) probeMCP(ctx context.Context) error {
	tools, err := e.mcp.listTools(ctx)
	if err != nil {
		return err
	}
	sig := toolSig(tools)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.upstream = e.mcp.identity()
	if sig != e.upSig {
		e.upSig = sig
		e.upTools = tools
		e.proxies = buildProxies(e.cfg, tools)
	}
	return nil
}

func toolSig(tools []*mcp.Tool) string {
	h := sha256.New()
	sorted := append([]*mcp.Tool(nil), tools...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	for _, t := range sorted {
		b, _ := json.Marshal(t)
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// apiBase is the base URL for OpenAI-style paths: a trailing /v1 is dropped
// because the endpoints below add it themselves.
func (s *Service) apiBase() string {
	b := strings.TrimRight(s.URL, "/")
	return strings.TrimSuffix(b, "/v1")
}

func (e *entry) specURL() string {
	if e.cfg.OpenAPI != "" {
		u, _ := e.cfg.resolve(e.cfg.OpenAPI)
		return u
	}
	u, _ := e.cfg.resolve("/openapi.json")
	if bu, err := url.Parse(e.cfg.URL); err == nil && bu.Path != "" && bu.Path != "/" {
		return strings.TrimRight(e.cfg.URL, "/") + "/openapi.json"
	}
	return u
}

func (e *entry) probeHTTP(ctx context.Context) error {
	cfg := &e.cfg
	// The spec is the contract for kind openapi: fetch it first (it doubles
	// as the health check while fresh enough to skip).
	if cfg.Kind == KindOpenAPI {
		e.mu.Lock()
		stale := e.doc == nil || time.Since(e.docAt) > specRefresh
		e.mu.Unlock()
		if stale {
			body, err := e.get(ctx, e.specURL(), maxSpecSize)
			if err != nil {
				return fmt.Errorf("openapi document: %w", err)
			}
			doc, err := parseOpenAPI(body)
			if err != nil {
				return fmt.Errorf("openapi document: %w", err)
			}
			e.mu.Lock()
			e.doc, e.docAt = doc, time.Now()
			e.mu.Unlock()
			if cfg.Health == "" {
				return nil
			}
		}
	}
	switch {
	case cfg.Health != "":
		u, _ := cfg.resolve(cfg.Health)
		_, err := e.get(ctx, u, 64<<10)
		return err
	case cfg.Kind == KindOpenAI:
		_, err := e.get(ctx, cfg.apiBase()+"/v1/models", 64<<10)
		return err
	}
	// No health URL: any answer other than a server or auth failure shows the service is there.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.URL, nil)
	if err != nil {
		return err
	}
	resp, err := noRedirect(e.http).Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	return statusErr(resp.StatusCode, true)
}

// get fetches a URL for probing and returns up to limit bytes of a 2xx body.
func (e *entry) get(ctx context.Context, u string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json, */*;q=0.5")
	resp, err := e.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if err := statusErr(resp.StatusCode, false); err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errors.New("response too large")
	}
	return body, nil
}

// statusErr maps a probe status to an error. Lenient accepts anything below
// 500 except authentication failures.
func statusErr(code int, lenient bool) error {
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return fmt.Errorf("HTTP %d (check the service's auth)", code)
	case code >= 500:
		return fmt.Errorf("HTTP %d", code)
	case !lenient && code >= 400:
		return fmt.Errorf("HTTP %d", code)
	}
	return nil
}

// snap is a consistent copy of an entry's runtime state.
type snap struct {
	cfg      Service
	h        health
	upstream upstreamInfo
	proxies  []proxyTool
	upTools  []*mcp.Tool
	doc      *apiDoc
}

func (e *entry) snapshot() snap {
	e.mu.Lock()
	defer e.mu.Unlock()
	return snap{cfg: e.cfg, h: e.h, upstream: e.upstream, proxies: e.proxies, upTools: e.upTools, doc: e.doc}
}
