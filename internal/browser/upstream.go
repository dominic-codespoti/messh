package browser

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/catalog"
	"messh/internal/state"
)

// Timing defaults; Options overrides them in tests.
const (
	defaultStartTimeout     = 3 * time.Minute // first start downloads Playwright MCP through npx
	defaultCallTimeout      = 2 * time.Minute
	defaultStartWait        = 25 * time.Second // how long a call waits for a start; agents' MCP clients give up at 30-60 s
	defaultExtensionTimeout = 90 * time.Second
	maxBackoff              = 30 * time.Second
)

// session is one running upstream: a Playwright MCP process plus our MCP
// client connection to it.
type session struct {
	conn    *catalog.Conn
	stop    func()        // ends the process tree; nil for injected upstreams
	exited  chan struct{} // closed when the process is gone
	out     *tailBuffer
	pid     int
	started time.Time
	version string // Playwright version the server reported
	tools   map[string]bool
	// connected becomes true after the first successful browser tool call;
	// until then (extension mode) the owner may still be approving the connection.
	connected bool
}

func (s *session) alive() bool {
	select {
	case <-s.exited:
		return false
	default:
		return true
	}
}

// launcher starts one upstream. The default launches Playwright MCP as a
// child process; tests inject servers.
type launcher func(ctx context.Context, cfg Config) (*session, error)

// upstream supervises the Playwright MCP child: it starts lazily on the first
// call, restarts after a crash with backoff, and stops when idle.
type upstream struct {
	paths  state.Paths
	log    *slog.Logger
	ctx    context.Context
	launch launcher
	// browserRunning reports whether the owner's browser for a channel is
	// running (extension mode attaches to it); replaceable in tests.
	browserRunning func(channel string) bool
	timeout        struct{ start, call, extension, startWait time.Duration }

	mu       sync.Mutex
	cur      *session
	cfg      Config // configuration cur was started with
	failures int
	nextTry  time.Time
	lastErr  string
	inflight int
	idle     *time.Timer
	idleFor  time.Duration
	closed   bool
	starting *startOp // non-nil while a start is in progress
}

func newUpstream(ctx context.Context, paths state.Paths, log *slog.Logger, launch launcher) *upstream {
	u := &upstream{paths: paths, log: log, ctx: ctx, launch: launch, browserRunning: BrowserRunning}
	u.timeout.start, u.timeout.call, u.timeout.extension, u.timeout.startWait = defaultStartTimeout, defaultCallTimeout, defaultExtensionTimeout, defaultStartWait
	if launch == nil {
		u.launch = u.launchProcess
	}
	return u
}

// running returns the live session, if any, without starting one.
func (u *upstream) running() *session {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.cur != nil && u.cur.alive() {
		return u.cur
	}
	return nil
}

// ensure returns a live session, starting the upstream when needed.
func (u *upstream) ensure(ctx context.Context, cfg Config) (*session, error) {
	for {
		u.mu.Lock()
		if u.closed {
			u.mu.Unlock()
			return nil, errors.New("the browser provider is shutting down")
		}
		if u.cur != nil && u.cur.alive() {
			if sameLaunch(u.cfg, cfg) {
				s := u.cur
				u.mu.Unlock()
				return s, nil
			}
			// browser.json changed in a way that needs a new process.
			s := u.cur
			u.cur = nil
			u.mu.Unlock()
			u.log.Info("browser configuration changed; restarting the browser")
			stopSession(s)
			continue
		}
		if op := u.starting; op != nil {
			u.mu.Unlock()
			if err := u.waitStart(ctx, op); err != nil {
				return nil, err
			}
			continue
		}
		if d := time.Until(u.nextTry); d > 0 {
			msg := fmt.Sprintf("the browser failed to start %d times in a row (%s); next attempt in %s", u.failures, u.lastErr, d.Round(time.Second))
			u.mu.Unlock()
			return nil, errors.New(msg)
		}
		op := &startOp{done: make(chan struct{})}
		u.starting = op
		u.mu.Unlock()

		// The start runs on the provider's context, not the caller's: an agent
		// that gives up waiting must not abort a start (or an npx download)
		// that the next call will want.
		go u.runStart(cfg, op)
		if err := u.waitStart(ctx, op); err != nil {
			return nil, err
		}
	}
}

// startOp is one upstream start in progress.
type startOp struct {
	done chan struct{}
	err  error // set before done closes
}

func (u *upstream) runStart(cfg Config, op *startOp) {
	s, err := u.start(u.ctx, cfg)
	u.mu.Lock()
	u.starting = nil
	if err == nil && u.closed {
		err = errors.New("the browser provider is shutting down")
	}
	if err != nil {
		u.failures++
		u.lastErr = firstLine(err.Error())
		u.nextTry = time.Now().Add(backoff(u.failures))
	} else {
		u.failures, u.lastErr = 0, ""
		u.cur, u.cfg = s, cfg
		u.idleFor = cfg.Idle()
		u.armIdleLocked()
	}
	u.mu.Unlock()
	if err != nil {
		stopSession(s)
	} else {
		go u.watch(s)
	}
	op.err = err
	close(op.done)
}

// waitStart waits for a start, but only as long as an agent's MCP client
// plausibly will: after startWait the call returns an explanation and the
// start carries on in the background.
func (u *upstream) waitStart(ctx context.Context, op *startOp) error {
	t := time.NewTimer(u.timeout.startWait)
	defer t.Stop()
	select {
	case <-op.done:
		return op.err
	case <-t.C:
		return fmt.Errorf("the browser is still starting after %s (the first start downloads Playwright MCP through npx, then launches the browser). It keeps starting in the background: retry this call in a few seconds, or check browser_status", u.timeout.startWait.Round(time.Second))
	case <-ctx.Done():
		return ctx.Err()
	}
}

func backoff(failures int) time.Duration {
	if failures <= 1 {
		return 0
	}
	d := time.Second << min(failures-2, 5)
	return min(d, maxBackoff)
}

func (u *upstream) start(ctx context.Context, cfg Config) (*session, error) {
	sctx, cancel := context.WithTimeout(ctx, u.timeout.start)
	defer cancel()
	return u.launch(sctx, cfg)
}

// watch notes an unexpected exit; the next call starts a fresh process.
func (u *upstream) watch(s *session) {
	<-s.exited
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.cur == s {
		u.cur = nil
		if !u.closed {
			tail := ""
			if s.out != nil {
				tail = firstLine(lastLine(s.out.String()))
			}
			u.log.Warn("browser upstream exited", "output", tail)
			u.lastErr = "exited: " + tail
		}
	}
}

// sameLaunch reports whether two configurations start the same process.
func sameLaunch(a, b Config) bool {
	a, b = a.Normalized(), b.Normalized()
	if a.Mode != b.Mode || a.Channel != b.Channel || a.Executable != b.Executable || a.ProfileDir != b.ProfileDir ||
		a.Headless != b.Headless || a.ExtensionToken != b.ExtensionToken || len(a.Command) != len(b.Command) {
		return false
	}
	for i := range a.Command {
		if a.Command[i] != b.Command[i] {
			return false
		}
	}
	return true
}

// begin marks a call in flight (the idle timer waits for it) and returns its end.
func (u *upstream) begin() func() {
	u.mu.Lock()
	u.inflight++
	u.mu.Unlock()
	return func() {
		u.mu.Lock()
		u.inflight--
		u.armIdleLocked()
		u.mu.Unlock()
	}
}

func (u *upstream) armIdleLocked() {
	if u.idle != nil {
		u.idle.Stop()
		u.idle = nil
	}
	if u.cur == nil || u.idleFor <= 0 || u.inflight > 0 {
		return
	}
	s := u.cur
	u.idle = time.AfterFunc(u.idleFor, func() {
		u.mu.Lock()
		if u.cur != s || u.inflight > 0 {
			u.mu.Unlock()
			return
		}
		u.cur = nil
		u.mu.Unlock()
		u.log.Info("browser idle; stopping it", "after", u.idleFor)
		stopSession(s)
	})
}

// setIdle changes the idle timeout for the running upstream (config reload).
func (u *upstream) setIdle(d time.Duration) {
	u.mu.Lock()
	u.idleFor = d
	u.armIdleLocked()
	u.mu.Unlock()
}

// call forwards one tool call to the browser and refreshes the idle timer.
// Failures of the upstream itself (not tool errors) are returned as errors.
func (u *upstream) call(ctx context.Context, cfg Config, name string, args json.RawMessage) (*mcp.CallToolResult, error) {
	end := u.begin()
	defer end()
	s, err := u.ensure(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if !s.tools[name] && len(s.tools) > 0 {
		return nil, fmt.Errorf("this Playwright MCP (%s) has no tool %s; messh was written for %s", s.version, name, PinnedVersion)
	}
	timeout := u.timeout.call
	waitingForUser := cfg.Normalized().Mode == ModeExtension && !s.connected
	if waitingForUser {
		timeout = u.timeout.extension
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	res, err := s.conn.Call(cctx, name, args)
	if err != nil {
		if waitingForUser && errors.Is(cctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			u.dropIf(s)
			return nil, errors.New(extensionHelp(cfg))
		}
		if errors.Is(cctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil {
			// A hung browser would block every later call; start over.
			u.dropIf(s)
			return nil, fmt.Errorf("the browser did not answer %s within %s and was stopped; the next call starts it again", name, timeout)
		}
		return nil, fmt.Errorf("%s", catalog.DescribeErr(err))
	}
	u.mu.Lock()
	s.connected = true
	u.mu.Unlock()
	return res, nil
}

// dropIf stops s if it is still the current session.
func (u *upstream) dropIf(s *session) {
	u.mu.Lock()
	if u.cur == s {
		u.cur = nil
	}
	u.mu.Unlock()
	stopSession(s)
}

// stop ends the running upstream, if any.
func (u *upstream) stop() bool {
	u.mu.Lock()
	s := u.cur
	u.cur = nil
	if u.idle != nil {
		u.idle.Stop()
		u.idle = nil
	}
	u.mu.Unlock()
	if s == nil {
		return false
	}
	stopSession(s)
	return true
}

// close stops the upstream for good.
func (u *upstream) close() {
	u.mu.Lock()
	u.closed = true
	u.mu.Unlock()
	u.stop()
	u.removePid()
}

func stopSession(s *session) {
	if s == nil {
		return
	}
	if s.conn != nil {
		s.conn.Close()
	}
	if s.stop != nil {
		s.stop()
	}
}

func extensionHelp(cfg Config) string {
	cfg = cfg.Normalized()
	b := channelLabel(cfg.Channel)
	msg := "the Playwright browser extension did not connect in time. In " + b + ": install the \"Playwright Extension\" (https://chromewebstore.google.com/detail/playwright-extension/mmlmfjhmonkocbjadbfplnigmagldckm), " +
		"then approve the connection page messh opened and pick the tab to share"
	if cfg.ExtensionToken == "" {
		msg += ". To skip that page every session, copy the extension's token with: messh browser setup --mode extension --token -"
	} else {
		msg += "; the extension_token in browser.json must be the one shown on the extension's status page for this browser profile"
	}
	return msg + ". The next call tries again."
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return clip(strings.TrimSpace(s), 300)
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// --- default launcher: Playwright MCP as a supervised child process -------

func (u *upstream) workDir() string { return filepath.Join(u.paths.BrowserDir(), "work") }
func (u *upstream) outDir() string  { return filepath.Join(u.workDir(), "out") }
func (u *upstream) pidFile() string { return filepath.Join(u.paths.BrowserDir(), "upstream.json") }

type pidRecord struct {
	PID   int    `json:"pid"`
	Token string `json:"token"`
}

func (u *upstream) savePid(t tree) {
	data, _ := json.Marshal(pidRecord{PID: t.PID(), Token: t.Token()})
	state.WriteFileAtomic(u.pidFile(), data, 0o600)
}

func (u *upstream) removePid() { os.Remove(u.pidFile()) }

// reapLeftover ends a tree a previous node left behind after a crash.
func (u *upstream) reapLeftover() {
	data, err := os.ReadFile(u.pidFile())
	if err != nil {
		return
	}
	var r pidRecord
	if json.Unmarshal(data, &r) == nil {
		killLeftover(r.PID, r.Token)
	}
	u.removePid()
}

// upstreamArgs builds Playwright MCP's flags (after the configured command).
// The server listens on 127.0.0.1 only and answers only requests whose Host
// header is exactly that address, which blocks DNS-rebinding from web pages.
func upstreamArgs(cfg Config, plan launchPlan, paths state.Paths, port int) []string {
	cfg = cfg.Normalized()
	args := []string{
		"--host", "127.0.0.1",
		"--port", strconv.Itoa(port),
		"--allowed-hosts", "127.0.0.1:" + strconv.Itoa(port),
		"--browser", plan.Channel,
		"--caps", "pdf,vision",
		"--output-dir", filepath.Join(paths.BrowserDir(), "work", "out"),
		"--idle-timeout", "0", // messh decides when the browser stops
		"--no-webmcp", // page-registered tools are not ours to expose
		"--image-responses", "allow",
	}
	if plan.Executable != "" {
		args = append(args, "--executable-path", plan.Executable)
	}
	if cfg.Mode == ModeExtension {
		args = append(args, "--extension")
		if cfg.ProfileDir != "" {
			args = append(args, "--profile-dir-name", cfg.ProfileDir)
		}
		return args
	}
	args = append(args, "--user-data-dir", AgentProfileDir(paths, cfg))
	if cfg.Headless {
		args = append(args, "--headless")
	}
	return args
}

// upstreamEnv is the child's environment: the node's own, minus any
// PLAYWRIGHT_MCP_* setting that could silently change what the server does.
func upstreamEnv(cfg Config) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(strings.ToUpper(kv), "PLAYWRIGHT_MCP_") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "NPM_CONFIG_UPDATE_NOTIFIER=false", "NPM_CONFIG_FUND=false", "NPM_CONFIG_AUDIT=false")
	if cfg.Normalized().Mode == ModeExtension && cfg.ExtensionToken != "" {
		env = append(env, "PLAYWRIGHT_MCP_EXTENSION_TOKEN="+cfg.ExtensionToken)
	}
	return env
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (u *upstream) launchProcess(ctx context.Context, cfg Config) (*session, error) {
	cfg = cfg.Normalized()
	if err := os.MkdirAll(u.paths.BrowserDir(), 0o700); err != nil {
		return nil, err
	}
	rt := CheckRuntime(cfg.Command)
	if rt.Err != nil {
		return nil, rt.Err
	}
	plan, err := planBrowser(cfg)
	if err != nil {
		return nil, err
	}
	if cfg.Mode == ModeExtension {
		if !u.browserRunning(cfg.Channel) {
			return nil, fmt.Errorf("extension mode attaches to the %s you already have open, and it is not running: start it (signed in, with the Playwright extension installed), then retry", channelLabel(cfg.Channel))
		}
	} else if err := os.MkdirAll(AgentProfileDir(u.paths, cfg), 0o700); err != nil {
		return nil, err
	}
	// Fresh scratch space: outputs and staged uploads from earlier runs are gone.
	os.RemoveAll(u.workDir())
	if err := os.MkdirAll(u.outDir(), 0o700); err != nil {
		return nil, err
	}
	u.reapLeftover()

	port, err := freePort()
	if err != nil {
		return nil, err
	}
	out := newTail(16 << 10)
	argv := append(append([]string(nil), cfg.Command[1:]...), upstreamArgs(cfg, plan, u.paths, port)...)
	t, err := startTree(treeSpec{Path: rt.Command, Args: argv, Dir: u.workDir(), Env: upstreamEnv(cfg), Output: out})
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", cfg.Command[0], err)
	}
	u.savePid(t)
	exited := make(chan struct{})
	go func() { t.Wait(); close(exited) }()
	s := &session{stop: func() { t.Kill(); <-exited }, exited: exited, out: out, pid: t.PID(), started: time.Now()}
	fail := func(err error) (*session, error) {
		stopSession(s)
		return nil, err
	}

	// Wait for the port, noticing an early exit (missing package, bad flag).
	addr := "127.0.0.1:" + strconv.Itoa(port)
	for {
		c, derr := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if derr == nil {
			c.Close()
			break
		}
		select {
		case <-exited:
			return nil, fmt.Errorf("Playwright MCP exited while starting: %s", describeOutput(out.String()))
		case <-ctx.Done():
			return fail(fmt.Errorf("Playwright MCP did not start listening within %s (first start downloads it with npx): %s", u.timeout.start, describeOutput(out.String())))
		case <-time.After(250 * time.Millisecond):
		}
	}
	listening := time.Since(s.started)
	endpoint := "http://" + addr + "/m/" + randomHex(16)
	if err := u.connect(ctx, s, endpoint); err != nil {
		return fail(err)
	}
	// Launch the browser now, as part of the start, so that the agent's first
	// real call is not also the browser launch. (Extension mode connects to the
	// owner's browser on the first call and has its own wait.)
	if cfg.Mode != ModeExtension {
		res, err := s.conn.Call(ctx, "browser_tabs", json.RawMessage(`{"action":"list"}`))
		if err != nil {
			return fail(fmt.Errorf("the browser did not launch: %s (%s)", catalog.DescribeErr(err), describeOutput(out.String())))
		}
		if res.IsError {
			return fail(fmt.Errorf("the browser did not launch: browser_tabs returned an error (%s; %s)", clip(describeOutput(textOf(res)), 1024), describeOutput(out.String())))
		}
		if _, err := ParseTabs(textOf(res)); err != nil {
			return fail(fmt.Errorf("the browser did not launch: cannot verify browser tabs (%v; %s)", err, describeOutput(out.String())))
		}
		s.connected = true
	}
	u.log.Info("browser upstream started", "pid", s.pid, "mode", cfg.Mode, "listening_after", listening.Round(time.Millisecond), "ready_after", time.Since(s.started).Round(time.Millisecond))
	return s, nil
}

// connect opens the MCP session on endpoint and records what the server offers.
func (u *upstream) connect(ctx context.Context, s *session, endpoint string) error {
	conn := catalog.NewConn(u.ctx, endpoint, &http.Client{Transport: &http.Transport{Proxy: nil}})
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if _, err := conn.Session(cctx); err != nil {
		return fmt.Errorf("connect to Playwright MCP: %s", catalog.DescribeErr(err))
	}
	tools, err := conn.ListTools(cctx)
	if err != nil {
		conn.Close()
		return fmt.Errorf("list Playwright MCP tools: %s", catalog.DescribeErr(err))
	}
	s.conn = conn
	_, s.version, _ = conn.Identity()
	s.tools = map[string]bool{}
	for _, t := range tools {
		s.tools[t.Name] = true
	}
	return nil
}

// describeOutput trims child output to the part that explains a failure.
func describeOutput(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return "no output"
	}
	lines := strings.Split(out, "\n")
	if len(lines) > 6 {
		lines = lines[len(lines)-6:]
	}
	return strings.Join(lines, " | ")
}
