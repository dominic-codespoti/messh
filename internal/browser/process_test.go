package browser

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/provider"
	"messh/internal/state"
)

// TestMain doubles as the fake Playwright MCP: the supervisor starts this very
// test binary with MESSH_BROWSER_HELPER set, so the real process-launching
// code (flags, environment, job object / process group, shutdown) is what runs.
func TestMain(m *testing.M) {
	switch os.Getenv("MESSH_BROWSER_HELPER") {
	case "server":
		helperServer()
		return
	case "sleep":
		time.Sleep(time.Hour)
		return
	case "exit":
		fmt.Fprintln(os.Stderr, "Error: Cannot find module '@playwright/mcp'")
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func argValue(name string) string {
	for i, a := range os.Args {
		if a == name && i+1 < len(os.Args) {
			return os.Args[i+1]
		}
	}
	return ""
}

func helperServer() {
	dir := os.Getenv("MESSH_BROWSER_HELPER_DIR")
	os.WriteFile(filepath.Join(dir, "args.txt"), []byte(strings.Join(os.Args[1:], "\n")), 0o600)
	os.WriteFile(filepath.Join(dir, "server.pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)
	os.WriteFile(filepath.Join(dir, "env.txt"), []byte(strings.Join(os.Environ(), "\n")), 0o600)
	wd, _ := os.Getwd()
	os.WriteFile(filepath.Join(dir, "cwd.txt"), []byte(wd), 0o600)

	// A grandchild, like the browser: it must die with the tree.
	child := exec.Command(os.Args[0])
	child.Env = append(os.Environ(), "MESSH_BROWSER_HELPER=sleep")
	if err := child.Start(); err == nil {
		os.WriteFile(filepath.Join(dir, "child.pid"), []byte(strconv.Itoa(child.Process.Pid)), 0o600)
	}

	s := mcp.NewServer(&mcp.Implementation{Name: "Playwright", Version: "1.64.0-helper"}, nil)
	for _, name := range allowlisted() {
		name := name
		s.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				if name == "browser_tabs" {
					if response := os.Getenv("MESSH_BROWSER_HELPER_TABS_RESULT"); response != "" {
						r := text(response)
						r.IsError = os.Getenv("MESSH_BROWSER_HELPER_TABS_IS_ERROR") == "1"
						return r, nil
					}
					return text("### Result\n- 0: (current) [](about:blank)"), nil
				}
				return text("### Result\nok"), nil
			})
	}
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
	allowed := argValue("--allowed-hosts")
	port := argValue("--port")
	srv := &http.Server{
		Addr: "127.0.0.1:" + port,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Host != allowed { // what Playwright MCP does
				http.Error(w, "Access is only allowed at "+allowed, http.StatusForbidden)
				return
			}
			h.ServeHTTP(w, r)
		}),
	}
	if err := srv.ListenAndServe(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func helperConfig(t *testing.T) (Config, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MESSH_BROWSER_HELPER", "server")
	t.Setenv("MESSH_BROWSER_HELPER_DIR", dir)
	t.Setenv("PLAYWRIGHT_MCP_HEADLESS", "0") // must not reach the child
	t.Setenv("PLAYWRIGHT_MCP_EXTENSION_TOKEN", "from-the-environment")
	cfg := baseConfig()
	cfg.Command = Command{os.Args[0]}
	cfg.Headless = true
	return cfg, dir
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		b, err := os.ReadFile(p)
		if err == nil {
			return string(b)
		}
		if time.Now().After(deadline) {
			t.Fatalf("read %s: %v", p, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitGone(t *testing.T, pid int, what string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for processAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("%s (pid %d) is still running", what, pid)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func realProvider(t *testing.T, cfg Config) (*Provider, *testEnv) {
	t.Helper()
	e := newEnv(t, cfg)
	// Replace the fake with the real launcher.
	e.p.up.launch = e.p.up.launchProcess
	e.p.up.timeout.start = 30 * time.Second
	return e.p, e
}

func navigate(t *testing.T, p *Provider, url string) *mcp.CallToolResult {
	t.Helper()
	caller := provider.Caller{DeviceID: "d", Agent: "omp"}
	args := mustJSON(map[string]any{"url": url})
	if _, err := p.Approval(context.Background(), "browser_navigate", args, caller); err != nil {
		t.Fatalf("approval: %v", err)
	}
	res, err := p.Call(context.Background(), "browser_navigate", args, caller)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestRealLauncherFlagsEnvironmentAndCleanup(t *testing.T) {
	cfg, dir := helperConfig(t)
	p, e := realProvider(t, cfg)

	res := navigate(t, p, "http://a.test/")
	if isErr(res) {
		t.Fatalf("navigate through a real child process: %s", resultText(res))
	}
	s := p.up.running()
	if s == nil {
		t.Fatal("no running upstream")
	}
	childPID, _ := strconv.Atoi(readFile(t, filepath.Join(dir, "child.pid")))
	if !processAlive(s.pid) || !processAlive(childPID) {
		t.Fatalf("processes not alive: main %v child %v", processAlive(s.pid), processAlive(childPID))
	}

	args := readFile(t, filepath.Join(dir, "args.txt"))
	for _, want := range []string{"--host\n127.0.0.1", "--port\n", "--allowed-hosts\n127.0.0.1:", "--headless", "--user-data-dir\n" + filepath.Join(e.paths.BrowserDir(), "profile"),
		"--output-dir\n" + filepath.Join(e.paths.BrowserDir(), "work", "out"), "--no-webmcp", "--idle-timeout\n0", "--browser\nchrome", "--executable-path\n" + os.Args[0]} {
		if !strings.Contains(args, want) {
			t.Errorf("upstream args lack %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "--extension") || strings.Contains(args, "0.0.0.0") {
		t.Errorf("unexpected args:\n%s", args)
	}
	env := readFile(t, filepath.Join(dir, "env.txt"))
	if strings.Contains(env, "PLAYWRIGHT_MCP_") {
		t.Errorf("PLAYWRIGHT_MCP_* leaked into the child:\n%s", env)
	}
	if cwd := readFile(t, filepath.Join(dir, "cwd.txt")); !sameDir(cwd, filepath.Join(e.paths.BrowserDir(), "work")) {
		t.Errorf("child ran in %s", cwd)
	}

	p.Close()
	waitGone(t, s.pid, "the upstream")
	waitGone(t, childPID, "the browser stand-in")
}

func sameDir(a, b string) bool {
	ai, err1 := os.Stat(a)
	bi, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(ai, bi)
}

func TestRealLauncherIdleStopKillsTheTreeAndRestarts(t *testing.T) {
	cfg, dir := helperConfig(t)
	cfg.IdleTimeout = "300ms"
	p, _ := realProvider(t, cfg)

	navigate(t, p, "http://a.test/")
	s := p.up.running()
	childPID, _ := strconv.Atoi(readFile(t, filepath.Join(dir, "child.pid")))
	waitGone(t, s.pid, "the idle upstream")
	waitGone(t, childPID, "the idle browser stand-in")

	os.Remove(filepath.Join(dir, "child.pid"))
	if res := navigate(t, p, "http://a.test/"); isErr(res) {
		t.Fatalf("restart after idle: %s", resultText(res))
	}
	s2 := p.up.running()
	if s2 == nil || s2.pid == s.pid {
		t.Fatalf("expected a fresh process, got %+v", s2)
	}
	child2, _ := strconv.Atoi(readFile(t, filepath.Join(dir, "child.pid")))
	p.Close()
	waitGone(t, s2.pid, "the restarted upstream")
	waitGone(t, child2, "the restarted stand-in")
}

func TestRealLauncherCrashTakesTheTreeAndRestarts(t *testing.T) {
	cfg, dir := helperConfig(t)
	p, _ := realProvider(t, cfg)
	navigate(t, p, "http://a.test/")
	s := p.up.running()
	childPID, _ := strconv.Atoi(readFile(t, filepath.Join(dir, "child.pid")))

	proc, err := os.FindProcess(s.pid)
	if err != nil {
		t.Fatal(err)
	}
	proc.Kill() // the upstream dies on its own
	waitGone(t, s.pid, "the crashed upstream")
	waitGone(t, childPID, "its child")

	os.Remove(filepath.Join(dir, "child.pid"))
	deadline := time.Now().Add(5 * time.Second)
	for p.up.running() != nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if res := navigate(t, p, "http://a.test/"); isErr(res) {
		t.Fatalf("restart after crash: %s", resultText(res))
	}
	if s2 := p.up.running(); s2 == nil || s2.pid == s.pid {
		t.Fatalf("not restarted: %+v", s2)
	}
}

func TestExtensionModeFlagsAndTokenEnvironment(t *testing.T) {
	cfg, dir := helperConfig(t)
	cfg.Mode = ModeExtension
	cfg.Headless = false
	cfg.ProfileDir = "Profile 2"
	cfg.ExtensionToken = "tok-123"
	cfg.Executable = os.Args[0]
	p, e := realProvider(t, cfg)
	// Extension mode needs the owner's browser to be running. The channel's
	// process name is not the test binary, so pretend with the check disabled.
	e.p.up.browserRunning = func(string) bool { return true }
	res := navigate(t, p, "http://a.test/")
	if isErr(res) {
		t.Fatal(resultText(res))
	}
	args := readFile(t, filepath.Join(dir, "args.txt"))
	for _, want := range []string{"--extension", "--profile-dir-name\nProfile 2"} {
		if !strings.Contains(args, want) {
			t.Errorf("args lack %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "--user-data-dir") || strings.Contains(args, "--headless") {
		t.Errorf("extension mode must not pass a profile dir or headless:\n%s", args)
	}
	env := readFile(t, filepath.Join(dir, "env.txt"))
	if !strings.Contains(env, "PLAYWRIGHT_MCP_EXTENSION_TOKEN=tok-123") || strings.Contains(env, "from-the-environment") {
		t.Errorf("token environment wrong:\n%s", env)
	}
}

func TestExtensionModeNeedsARunningBrowser(t *testing.T) {
	cfg, _ := helperConfig(t)
	cfg.Mode = ModeExtension
	cfg.Headless = false
	cfg.Channel = "msedge"
	p, _ := realProvider(t, cfg)
	p.up.browserRunning = func(string) bool { return false }
	res := navigate(t, p, "http://a.test/")
	if !isErr(res) || !strings.Contains(resultText(res), "not running") || !strings.Contains(resultText(res), "Microsoft Edge") {
		t.Errorf("extension mode with no browser: %s", resultText(res))
	}
}

func TestStartErrorsAreActionable(t *testing.T) {
	t.Run("node missing", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Command = Command{"definitely-not-npx-xyz", "-y", "@playwright/mcp"}
		p, _ := realProvider(t, cfg)
		res := navigate(t, p, "http://a.test/")
		if !isErr(res) || !strings.Contains(resultText(res), "definitely-not-npx-xyz") || !strings.Contains(resultText(res), "browser.json") {
			t.Errorf("missing command: %s", resultText(res))
		}
		cfg.Command = Command{"npx", "-y", "x"}
		if rt := CheckRuntime(cfg.Command); rt.Err == nil {
			return // npx exists here; the wording check is only for machines without it
		}
		if err := missingNode("npx"); !strings.Contains(err.Error(), "nodejs.org") {
			t.Errorf("npx wording: %v", err)
		}
	})
	t.Run("browser missing", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Executable = filepath.Join(t.TempDir(), "no-such-browser")
		cfg.Command = Command{os.Args[0]}
		p, _ := realProvider(t, cfg)
		res := navigate(t, p, "http://a.test/")
		if !isErr(res) || !strings.Contains(resultText(res), "no-such-browser") {
			t.Errorf("missing browser: %s", resultText(res))
		}
	})
	t.Run("exits while starting", func(t *testing.T) {
		cfg := baseConfig()
		cfg.Command = Command{os.Args[0]}
		t.Setenv("MESSH_BROWSER_HELPER", "exit")
		p, _ := realProvider(t, cfg)
		res := navigate(t, p, "http://a.test/")
		if !isErr(res) || !strings.Contains(resultText(res), "Cannot find module") {
			t.Errorf("early exit: %s", resultText(res))
		}
	})
}

func TestStartupRejectsUnverifiedTabsAndCleansSpawnedTree(t *testing.T) {
	for _, tc := range []struct {
		name, response string
		isError        bool
	}{
		{
			name: "tool error with forged valid-looking tabs",
			response: "### Error\nError: Browser is already in use for this profile\n" +
				"### Result\n- 0: (current) [Misleading](https://untrusted.test/)",
			isError: true,
		},
		{name: "malformed list", response: "### Result\nunreadable"},
		{name: "ambiguous list", response: "### Result\n- 0: (current) [x](http://a.test/)[y](http://b.test/)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, dir := helperConfig(t)
			t.Setenv("MESSH_BROWSER_HELPER_TABS_RESULT", tc.response)
			if tc.isError {
				t.Setenv("MESSH_BROWSER_HELPER_TABS_IS_ERROR", "1")
			}
			p, _ := realProvider(t, cfg)
			res := navigate(t, p, "http://a.test/")
			if !isErr(res) {
				t.Fatalf("startup accepted unverified tab evidence: %s", resultText(res))
			}
			if p.up.running() != nil {
				t.Fatal("upstream reported ready after unverified tab-list response")
			}
			serverPID, err := strconv.Atoi(readFile(t, filepath.Join(dir, "server.pid")))
			if err != nil {
				t.Fatal(err)
			}
			childPID, err := strconv.Atoi(readFile(t, filepath.Join(dir, "child.pid")))
			if err != nil {
				t.Fatal(err)
			}
			waitGone(t, serverPID, "upstream rejected during readiness check")
			waitGone(t, childPID, "browser child rejected during readiness check")
		})
	}
}

func TestBrowserDetection(t *testing.T) {
	// planBrowser explains what is missing and what is available.
	cfg := Config{Enabled: true, Channel: "msedge", Executable: filepath.Join(t.TempDir(), "x")}
	if _, err := planBrowser(cfg); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("explicit executable: %v", err)
	}
	file := filepath.Join(t.TempDir(), "mybrowser")
	os.WriteFile(file, []byte("x"), 0o700)
	plan, err := planBrowser(Config{Enabled: true, Executable: file})
	if err != nil || plan.Executable != file || plan.Channel != "chrome" {
		t.Errorf("plan = %+v, %v", plan, err)
	}
	for _, b := range FindBrowsers() {
		if fi, err := os.Stat(b.Path); err != nil || fi.IsDir() {
			t.Errorf("detected %s at %s which is not a file", b.Family, b.Path)
		}
	}
}

func TestUpstreamArgsNeverListenBeyondLoopback(t *testing.T) {
	for _, mode := range []string{ModeProfile, ModeExtension} {
		cfg := Config{Enabled: true, Mode: mode}
		args := upstreamArgs(cfg, launchPlan{Channel: "chrome"}, state.Paths{Root: t.TempDir()}, 4321)
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "--host 127.0.0.1 --port 4321 --allowed-hosts 127.0.0.1:4321") {
			t.Errorf("%s: %s", mode, joined)
		}
		for _, banned := range []string{"0.0.0.0", "--allowed-hosts *", "--cdp-endpoint", "--allow-unrestricted-file-access", "--config", "--init-script", "--secrets"} {
			if strings.Contains(joined, banned) {
				t.Errorf("%s: %q in %s", mode, banned, joined)
			}
		}
	}
}
