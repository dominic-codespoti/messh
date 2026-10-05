package browser

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/approval"
	"messh/internal/provider"
)

func toolNames(tools []provider.Tool) []string {
	var out []string
	for _, t := range tools {
		out = append(out, t.Def.Name)
	}
	sort.Strings(out)
	return out
}

func hasTool(tools []provider.Tool, name string) bool {
	for _, t := range tools {
		if t.Def.Name == name {
			return true
		}
	}
	return false
}

func isErr(r *mcp.CallToolResult) bool { return r.IsError }

func TestPublishedToolAllowlist(t *testing.T) {
	// Nothing configured: no tools at all, not even the status tool.
	e := newEnv(t, Config{})
	if got := e.p.Tools(); len(got) != 0 {
		t.Fatalf("no config published %v", toolNames(got))
	}
	// enabled:false likewise.
	cfg := baseConfig()
	cfg.Enabled = false
	e = newEnv(t, cfg)
	if got := e.p.Tools(); len(got) != 0 {
		t.Fatalf("enabled:false published %v", toolNames(got))
	}

	e = newEnv(t, baseConfig())
	tools := e.p.Tools()
	for _, banned := range []string{
		"browser_run_code_unsafe", "browser_install", "browser_get_config", "browser_route", "browser_cookie_list",
		"browser_localstorage_get", "browser_storage_state", "browser_start_tracing", "browser_start_video", "browser_evaluate", "browser_drop",
	} {
		if hasTool(tools, banned) {
			t.Errorf("%s is published", banned)
		}
	}
	for _, want := range []string{"browser_navigate", "browser_snapshot", "browser_click", "browser_tabs", "browser_file_upload", "browser_take_screenshot", ToolStatus} {
		if !hasTool(tools, want) {
			t.Errorf("%s is not published", want)
		}
	}
	for _, tl := range tools {
		wantClass := provider.ClassBrowser
		if tl.Def.Name == ToolStatus {
			wantClass = provider.ClassInfo
		}
		if tl.Class != wantClass {
			t.Errorf("%s has class %s", tl.Def.Name, tl.Class)
		}
		if len(tl.Def.Name)+len("a-long-device-name__") > 64 {
			t.Errorf("%s is too long for <device>__<tool>", tl.Def.Name)
		}
		raw, _ := json.Marshal(tl.Def.InputSchema)
		if strings.Contains(string(raw), `"filename"`) {
			t.Errorf("%s still offers a filename parameter", tl.Def.Name)
		}
		if !strings.Contains(tl.Def.Description, "approval") && tl.Def.Name != ToolStatus {
			t.Errorf("%s description lacks the approval note", tl.Def.Name)
		}
	}
	// Every allowlisted tool has a schema and a policy.
	for _, n := range allowlisted() {
		if _, ok := upstreamDefs[n]; !ok {
			t.Errorf("%s has no embedded definition", n)
		}
	}

	// browser_evaluate appears only when the owner allows scripts.
	cfg = baseConfig()
	cfg.AllowScript = true
	e = newEnv(t, cfg)
	if !hasTool(e.p.Tools(), "browser_evaluate") {
		t.Error("allow_script did not publish browser_evaluate")
	}
}

func TestUnpublishedToolsAreNotCallable(t *testing.T) {
	n := newNode(t, baseConfig())
	n.allowOnce()
	for _, tool := range []string{"browser_run_code_unsafe", "browser_install", "browser_cookie_list", "browser_evaluate", "nonsense"} {
		res := n.call(tool, map[string]any{"code": "async (page) => 1"})
		if !isErr(res) {
			t.Errorf("%s ran: %s", tool, resultText(res))
		}
	}
	if n.env.fb.called("browser_run_code_unsafe") != 0 || n.env.fb.called("browser_evaluate") != 0 {
		t.Error("an unpublished tool reached the browser")
	}
	if len(n.surf.asked()) != 0 {
		t.Errorf("refused calls must not prompt: %+v", n.surf.asked())
	}
}

func TestNavigateAndReadFlow(t *testing.T) {
	n := newNode(t, baseConfig())
	n.env.fb.addSite("http://a.test/start", "Site A")
	n.allowOnce()

	res := n.call("browser_navigate", map[string]any{"url": "http://a.test/start?token=secret"})
	if isErr(res) {
		t.Fatalf("navigate: %s", resultText(res))
	}
	txt := resultText(res)
	if !strings.Contains(txt, "Page URL: http://a.test/start") || strings.Contains(txt, "Ran Playwright code") || strings.Contains(txt, "out"+string(filepath.Separator)+"page") {
		t.Errorf("navigate result not cleaned:\n%s", txt)
	}
	if !strings.Contains(txt, "browser_snapshot") {
		t.Errorf("snapshot file link should point the agent at browser_snapshot:\n%s", txt)
	}
	prompts := n.surf.asked()
	if len(prompts) != 1 {
		t.Fatalf("want 1 prompt, got %d", len(prompts))
	}
	p := prompts[0]
	if p.Title != "omp on raspi wants to view a.test in your browser" && !strings.Contains(p.Title, "wants to view") {
		t.Errorf("title = %q", p.Title)
	}
	// The query (a token, maybe) must not be shown.
	for _, d := range p.Details {
		if strings.Contains(d.Value, "secret") {
			t.Errorf("detail %s leaks the query: %s", d.Label, d.Value)
		}
	}

	snap := n.call("browser_snapshot", map[string]any{})
	if isErr(snap) || !strings.Contains(resultText(snap), "content of http://a.test/start") {
		t.Errorf("snapshot: %s", resultText(snap))
	}
}

func TestSchemesRefusedWithoutPrompting(t *testing.T) {
	n := newNode(t, baseConfig())
	n.allowOnce()
	for _, u := range []string{"file:///C:/Windows/win.ini", "chrome://settings", "javascript:alert(1)", "data:text/html,hi", "view-source:http://a.test/", "devtools://x", "chrome-extension://x/y", "ftp://a.test/"} {
		for _, tool := range []struct {
			name string
			args map[string]any
		}{{"browser_navigate", map[string]any{"url": u}}, {"browser_tabs", map[string]any{"action": "new", "url": u}}} {
			if res := n.call(tool.name, tool.args); !isErr(res) {
				t.Errorf("%s %s was allowed: %s", tool.name, u, resultText(res))
			}
		}
	}
	if len(n.surf.asked()) != 0 || n.env.fb.called("browser_navigate") != 0 {
		t.Errorf("refused URLs reached the prompt or the browser: %d prompts", len(n.surf.asked()))
	}
}

func TestDenyAndAllowOrigins(t *testing.T) {
	cfg := baseConfig()
	cfg.DenyOrigins = []string{"bank.test", "*.secure.test"}
	cfg.AllowOrigins = []string{"docs.test", "*.wiki.test"}
	n := newNode(t, cfg)
	fb := n.env.fb
	fb.addSite("https://docs.test/", "Docs")
	fb.addSite("https://x.wiki.test/", "Wiki")
	fb.addSite("https://other.test/", "Other")
	n.denyAll() // anything that reaches a prompt is refused

	// Denied: refused with no prompt and nothing sent to the browser.
	for _, u := range []string{"https://bank.test/login", "http://bank.test:8080/", "https://a.secure.test/"} {
		if res := n.call("browser_navigate", map[string]any{"url": u}); !isErr(res) || !strings.Contains(resultText(res), "deny_origins") {
			t.Errorf("%s: %s", u, resultText(res))
		}
	}
	if len(n.surf.asked()) != 0 || fb.called("browser_navigate") != 0 {
		t.Fatal("deny_origins must refuse before any prompt or browser call")
	}

	// Allowed for reading: runs without a prompt.
	for _, u := range []string{"https://docs.test/", "https://x.wiki.test/"} {
		if res := n.call("browser_navigate", map[string]any{"url": u}); isErr(res) {
			t.Errorf("%s: %s", u, resultText(res))
		}
	}
	if len(n.surf.asked()) != 0 {
		t.Errorf("allow_origins read prompted: %+v", n.surf.asked())
	}
	ap, err := n.env.p.Approval(context.Background(), "browser_navigate", mustJSON(map[string]any{"url": "https://docs.test/"}), n.caller)
	if err != nil || ap.Auto == "" {
		t.Errorf("allow_origins read should be Auto: %+v %v", ap, err)
	}

	// Interacting with an allow-listed site still asks.
	ap, err = n.env.p.Approval(context.Background(), "browser_click", mustJSON(map[string]any{"target": "e1", "element": "x"}), n.caller)
	if err != nil || ap.Auto != "" {
		t.Errorf("allow_origins must not cover interaction: %+v %v", ap, err)
	}
	// A site that is not listed asks.
	ap, err = n.env.p.Approval(context.Background(), "browser_navigate", mustJSON(map[string]any{"url": "https://other.test/"}), n.caller)
	if err != nil || ap.Auto != "" {
		t.Errorf("unlisted site must ask: %+v %v", ap, err)
	}
	// Wildcards do not cover the bare domain.
	ap, err = n.env.p.Approval(context.Background(), "browser_navigate", mustJSON(map[string]any{"url": "https://wiki.test/"}), n.caller)
	if err != nil || ap.Auto != "" {
		t.Errorf("*.wiki.test must not cover wiki.test: %+v %v", ap, err)
	}
}

// A redirect or link that lands on a denied site is refused without a prompt,
// and the tab is reset.
func TestRedirectIntoDeniedOrigin(t *testing.T) {
	cfg := baseConfig()
	cfg.DenyOrigins = []string{"bank.test"}
	cfg.AllowOrigins = []string{"hop.test"}
	n := newNode(t, cfg)
	fb := n.env.fb
	fb.addSite("https://bank.test/in", "Bank")
	fb.redirects["https://hop.test/r"] = "https://bank.test/in"
	res := n.call("browser_navigate", map[string]any{"url": "https://hop.test/r"})
	if !isErr(res) || strings.Contains(resultText(res), "Bank") || !strings.Contains(resultText(res), "deny_origins") {
		t.Fatalf("result: %s", resultText(res))
	}
	if got := fb.current().url; got != "about:blank" {
		t.Errorf("tab not reset: %s", got)
	}
	if len(n.env.askedSites()) != 0 {
		t.Errorf("a denied site must not be asked about: %v", n.env.askedSites())
	}
}

func TestPostNavigationHoldBackAndReset(t *testing.T) {
	n := newNode(t, baseConfig())
	fb := n.env.fb
	fb.addSite("http://a.test/", "Site A")
	fb.addSite("http://b.test/inbox", "Secret Inbox")
	fb.links["e3"] = "http://b.test/inbox"
	// Prompts for the first site and for clicking on it are fine; the follow-up
	// for the site the click lands on is refused.
	n.surf.answer = func(p approval.Prompt) approval.Answer {
		return approval.Answer{Allow: !strings.Contains(p.Title, "went to")}
	}
	if res := n.call("browser_navigate", map[string]any{"url": "http://a.test/"}); isErr(res) {
		t.Fatal(resultText(res))
	}
	res := n.call("browser_click", map[string]any{"element": "go to B", "target": "e3"})
	txt := resultText(res)
	if !isErr(res) {
		t.Fatalf("click into an unapproved site returned content: %s", txt)
	}
	if strings.Contains(txt, "Secret Inbox") || strings.Contains(txt, "Page Title") {
		t.Errorf("content of the unapproved page leaked: %s", txt)
	}
	if !strings.Contains(txt, "b.test") || !strings.Contains(txt, "not approved") {
		t.Errorf("refusal should say where it went: %s", txt)
	}
	if got := fb.current().url; got != "about:blank" {
		t.Errorf("tab still shows %s", got)
	}
	if sites := n.env.askedSites(); len(sites) != 0 {
		_ = sites // the follow-up goes through the engine, not the test asker
	}
	var follow *approval.Prompt
	for _, p := range n.surf.asked() {
		if strings.Contains(p.Title, "went to") {
			p := p
			follow = &p
		}
	}
	if follow == nil {
		t.Fatal("no follow-up approval was requested for b.test")
	}
	if len(follow.Scopes) < 3 { // exact + read + interact + any
		t.Errorf("follow-up scopes: %+v", follow.Scopes)
	}

	// With approval the result is released.
	fb.mu.Lock()
	fb.load(fb.cur, "http://a.test/")
	fb.mu.Unlock()
	n.allowOnce()
	res = n.call("browser_click", map[string]any{"element": "go to B", "target": "e3"})
	if isErr(res) || !strings.Contains(resultText(res), "Page Title: Secret Inbox") {
		t.Errorf("approved follow-up: %s", resultText(res))
	}
}

func TestPopupAndNewTabAreVetted(t *testing.T) {
	n := newNode(t, baseConfig())
	fb := n.env.fb
	fb.addSite("http://a.test/", "Site A")
	fb.addSite("http://ads.test/pop", "Popup")
	fb.popups["e9"] = "http://ads.test/pop"
	n.surf.answer = func(p approval.Prompt) approval.Answer {
		return approval.Answer{Allow: !strings.Contains(p.Title, "went to")}
	}
	n.call("browser_navigate", map[string]any{"url": "http://a.test/"})
	res := n.call("browser_click", map[string]any{"element": "ad", "target": "e9"})
	if isErr(res) {
		t.Fatalf("the approved page itself stays available: %s", resultText(res))
	}
	if strings.Contains(resultText(res), "Popup") || strings.Contains(resultText(res), "ads.test/pop") {
		t.Errorf("popup details leaked in %s", resultText(res))
	}
	if urls := fb.tabURLs(); len(urls) != 1 || urls[0] != "http://a.test/" {
		t.Errorf("denied popup tab was not closed: %v", urls)
	}
	// The open-tabs section the browser adds for a popup lists sites only.
	fb.popups["e9"] = "http://a.test/other" // same site: no prompt, allowed
	fb.addSite("http://a.test/other", "Other Page Title")
	res = n.call("browser_click", map[string]any{"element": "ad", "target": "e9"})
	if isErr(res) {
		t.Fatalf("%s", resultText(res))
	}
	if strings.Contains(resultText(res), "Other Page Title") && strings.Contains(resultText(res), "### Open tabs") {
		sec, _ := section(resultText(res), "Open tabs")
		if strings.Contains(sec, "Other Page Title") {
			t.Errorf("open tabs section shows titles: %s", sec)
		}
	}
}

func TestOpenTabsListShowsSitesOnly(t *testing.T) {
	n := newNode(t, baseConfig())
	fb := n.env.fb
	fb.addSite("http://a.test/p", "Private Title A")
	n.allowOnce()
	n.call("browser_navigate", map[string]any{"url": "http://a.test/p?s=1"})
	res := n.call("browser_tabs", map[string]any{"action": "list"})
	txt := resultText(res)
	if isErr(res) || strings.Contains(txt, "Private Title") || strings.Contains(txt, "/p") || !strings.Contains(txt, "http://a.test") {
		t.Errorf("tab list: %s", txt)
	}
	if !strings.Contains(txt, "(current)") {
		t.Errorf("tab list lost the current marker: %s", txt)
	}
}

func TestFailClosedWhenOriginUnknown(t *testing.T) {
	n := newNode(t, baseConfig())
	fb := n.env.fb
	fb.addSite("http://a.test/", "Site A")
	n.allowOnce()
	n.call("browser_navigate", map[string]any{"url": "http://a.test/"})
	n.surf.reset()

	// The tab list cannot be read: page-bound calls are refused before any prompt.
	fb.mu.Lock()
	fb.badTabs = "### Result\nthis is not a tab list"
	fb.mu.Unlock()
	res := n.call("browser_snapshot", map[string]any{})
	if !isErr(res) || strings.Contains(resultText(res), "content of") {
		t.Errorf("snapshot with unknown origin: %s", resultText(res))
	}
	if len(n.surf.asked()) != 0 {
		t.Error("an unknown origin must not reach the prompt")
	}
	// A title that forges the list boundary: ambiguous, refused.
	fb.mu.Lock()
	fb.badTabs = "### Result\n- 0: (current) [x](http://a.test/)[y](http://evil.test/)"
	fb.mu.Unlock()
	if res := n.call("browser_snapshot", map[string]any{}); !isErr(res) {
		t.Errorf("ambiguous tab accepted: %s", resultText(res))
	}
	// Failing after the action: the content is withheld and the tab reset.
	fb.mu.Lock()
	fb.badTabs = ""
	fb.breakAfterClick = "### Result\nunreadable" // the list breaks as the click lands
	fb.mu.Unlock()
	fb.links["e1"] = "http://c.test/"
	fb.addSite("http://c.test/", "C")
	n.allowOnce()
	res = n.call("browser_click", map[string]any{"element": "x", "target": "e1"})
	if !isErr(res) || strings.Contains(resultText(res), "Page Title") {
		t.Errorf("click with unverifiable result returned content: %s", resultText(res))
	}
	if got := fb.current().url; got != "about:blank" {
		t.Errorf("tab not reset after an unverifiable result: %s", got)
	}
}

func TestTabListToolErrorsFailClosed(t *testing.T) {
	n := newNode(t, baseConfig())
	fb := n.env.fb
	fb.addSite("http://a.test/", "Private A")
	fb.addSite("http://b.test/secret", "Private B")
	n.allowOnce()
	if res := n.call("browser_navigate", map[string]any{"url": "http://a.test/"}); isErr(res) {
		t.Fatal(resultText(res))
	}
	n.surf.reset()

	// Even a valid-looking Result cannot be used when the upstream marked it as
	// an error; the page-bound action must not reach the browser or approval UI.
	fb.mu.Lock()
	fb.badTabs = "### Result\n- 0: (current) [Private B](http://b.test/secret)"
	fb.badTabsIsError = true
	fb.mu.Unlock()
	before := fb.called("browser_snapshot")
	res := n.call("browser_snapshot", map[string]any{})
	if !isErr(res) || strings.Contains(resultText(res), "Private B") {
		t.Fatalf("tool-error tab list authenticated an origin: %s", resultText(res))
	}
	if fb.called("browser_snapshot") != before || len(n.surf.asked()) != 0 {
		t.Fatal("page-bound action or approval was reached with an error tab list")
	}

	// After a page-changing action, the same error must withhold the action's
	// page content and reset the active tab.
	fb.mu.Lock()
	fb.badTabs = ""
	fb.badTabsIsError = false
	fb.links["e1"] = "http://b.test/secret"
	fb.breakAfterClick = "### Result\n- 0: (current) [Private B](http://b.test/secret)"
	fb.clickTabsError = true
	fb.mu.Unlock()
	n.allowOnce()
	res = n.call("browser_click", map[string]any{"element": "open private page", "target": "e1"})
	txt := resultText(res)
	if !isErr(res) || strings.Contains(txt, "Private B") || strings.Contains(txt, "Page URL:") {
		t.Fatalf("tool-error tab list leaked post-action content: %s", txt)
	}
	if got := fb.current().url; got != "about:blank" {
		t.Errorf("active tab not reset after tab-list tool error: %s", got)
	}
}

func TestFailedRemediationWithholdsAndStops(t *testing.T) {
	t.Run("blank navigation", func(t *testing.T) {
		cfg := baseConfig()
		cfg.DenyOrigins = []string{"b.test"}
		n := newNode(t, cfg)
		fb := n.env.fb
		fb.addSite("http://a.test/", "Approved")
		fb.addSite("http://b.test/private", "Private Page")
		fb.links["e1"] = "http://b.test/private"
		n.allowOnce()
		if res := n.call("browser_navigate", map[string]any{"url": "http://a.test/"}); isErr(res) {
			t.Fatal(resultText(res))
		}
		fb.mu.Lock()
		fb.blankResetError = true
		fb.mu.Unlock()

		res := n.call("browser_click", map[string]any{"element": "open private page", "target": "e1"})
		txt := resultText(res)
		if !isErr(res) || strings.Contains(txt, "Private Page") || strings.Contains(txt, "Page URL:") {
			t.Fatalf("failed blank remediation did not withhold the result: %s", txt)
		}
		if n.env.p.up.running() != nil {
			t.Fatal("browser automation remained active after failed blank remediation")
		}
	})

	t.Run("popup close", func(t *testing.T) {
		cfg := baseConfig()
		cfg.DenyOrigins = []string{"ads.test"}
		n := newNode(t, cfg)
		fb := n.env.fb
		fb.addSite("http://a.test/", "Approved")
		fb.addSite("http://ads.test/private", "Private Popup")
		fb.popups["e9"] = "http://ads.test/private"
		n.allowOnce()
		if res := n.call("browser_navigate", map[string]any{"url": "http://a.test/"}); isErr(res) {
			t.Fatal(resultText(res))
		}
		fb.mu.Lock()
		fb.closeTabError = true
		fb.mu.Unlock()

		res := n.call("browser_click", map[string]any{"element": "open popup", "target": "e9"})
		txt := resultText(res)
		if !isErr(res) || strings.Contains(txt, "Private Popup") || strings.Contains(txt, "ads.test/private") || strings.Contains(txt, "Page URL:") {
			t.Fatalf("failed popup-close remediation did not withhold the result: %s", txt)
		}
		if n.env.p.up.running() != nil {
			t.Fatal("browser automation remained active after failed popup close")
		}
	})
}
func TestNoPageYet(t *testing.T) {
	n := newNode(t, baseConfig())
	n.allowOnce()
	res := n.call("browser_snapshot", map[string]any{})
	if !isErr(res) || !strings.Contains(resultText(res), "browser_navigate") {
		t.Errorf("snapshot without a page: %s", resultText(res))
	}
	if s, _ := n.env.launch.counts(); s != 0 {
		t.Error("a page-bound call must not start the browser just to find out there is no page")
	}
}

func TestBlankAndNonWebPages(t *testing.T) {
	n := newNode(t, baseConfig())
	fb := n.env.fb
	n.allowOnce()
	n.call("browser_navigate", map[string]any{"url": "about:blank"}) // allowed, no site involved
	if len(n.surf.asked()) != 0 {
		t.Error("about:blank asked for approval")
	}
	res := n.call("browser_snapshot", map[string]any{})
	if isErr(res) {
		t.Errorf("reading a blank page: %s", resultText(res))
	}
	if res := n.call("browser_click", map[string]any{"element": "x", "target": "e1"}); !isErr(res) {
		t.Errorf("clicking on a blank page: %s", resultText(res))
	}
	fb.mu.Lock()
	fb.tabs[0] = fakeTab{title: "Settings", url: "chrome://settings/"}
	fb.mu.Unlock()
	if res := n.call("browser_snapshot", map[string]any{}); !isErr(res) || !strings.Contains(resultText(res), "chrome:") {
		t.Errorf("a chrome:// page must be refused: %s", resultText(res))
	}
}

func TestIdleStopAndRestart(t *testing.T) {
	cfg := baseConfig()
	cfg.IdleTimeout = "150ms"
	n := newNode(t, cfg)
	n.env.fb.addSite("http://a.test/", "A")
	n.allowOnce()
	n.call("browser_navigate", map[string]any{"url": "http://a.test/"})
	starts, _ := n.env.launch.counts()
	if starts != 1 {
		t.Fatalf("starts = %d", starts)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, stops := n.env.launch.counts(); stops == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the browser was not stopped after the idle timeout")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Idle-stopped: there is no page, and the next navigation starts it again.
	if res := n.call("browser_snapshot", map[string]any{}); !isErr(res) {
		t.Error("page-bound call after idle stop should say there is no page")
	}
	if res := n.call("browser_navigate", map[string]any{"url": "http://a.test/"}); isErr(res) {
		t.Fatalf("restart: %s", resultText(res))
	}
	if starts, _ := n.env.launch.counts(); starts != 2 {
		t.Errorf("starts = %d, want 2", starts)
	}
}

func TestUpstreamCrashRestarts(t *testing.T) {
	n := newNode(t, baseConfig())
	n.env.fb.addSite("http://a.test/", "A")
	n.allowOnce()
	n.call("browser_navigate", map[string]any{"url": "http://a.test/"})
	// Simulate the process dying.
	s := n.env.p.up.running()
	if s == nil {
		t.Fatal("not running")
	}
	s.stop()
	deadline := time.Now().Add(2 * time.Second)
	for n.env.p.up.running() != nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if res := n.call("browser_navigate", map[string]any{"url": "http://a.test/"}); isErr(res) {
		t.Fatalf("after a crash: %s", resultText(res))
	}
	if starts, _ := n.env.launch.counts(); starts != 2 {
		t.Errorf("starts = %d", starts)
	}
}

func TestStartFailureBacksOff(t *testing.T) {
	n := newNode(t, baseConfig())
	n.env.p.up.launch = func(ctx context.Context, cfg Config) (*session, error) {
		return nil, os.ErrPermission
	}
	n.allowOnce()
	r1 := n.call("browser_navigate", map[string]any{"url": "http://a.test/"})
	r2 := n.call("browser_navigate", map[string]any{"url": "http://a.test/"})
	r3 := n.call("browser_navigate", map[string]any{"url": "http://a.test/"})
	if !isErr(r1) || !isErr(r2) || !isErr(r3) {
		t.Fatal("start failures must be reported")
	}
	if !strings.Contains(resultText(r3), "next attempt in") {
		t.Errorf("repeated failures should back off: %s", resultText(r3))
	}
}

func TestConfigReload(t *testing.T) {
	e := newEnv(t, Config{})
	changed := make(chan struct{}, 8)
	e.p.SetOnChange(func() { changed <- struct{}{} })
	wait := func(what string) {
		t.Helper()
		select {
		case <-changed:
		case <-time.After(3 * time.Second):
			t.Fatalf("no change notification: %s", what)
		}
	}
	if err := SaveConfig(e.paths.BrowserFile(), baseConfig()); err != nil {
		t.Fatal(err)
	}
	wait("enable")
	if !hasTool(e.p.Tools(), "browser_navigate") {
		t.Fatal("tools not published after enabling")
	}
	// A half-saved file keeps the previous configuration.
	if err := os.WriteFile(e.paths.BrowserFile(), []byte(`{"enabled": tru`), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if !hasTool(e.p.Tools(), "browser_navigate") {
		t.Fatal("a corrupt file dropped the configuration")
	}
	if st := e.p.Status(context.Background()); len(st.Problems) == 0 {
		t.Error("status does not report the corrupt file")
	}
	// An invalid list means the provider does not run on it.
	bad := baseConfig()
	bad.DenyOrigins = []string{"https://bank.test/with/path"}
	if err := SaveConfig(e.paths.BrowserFile(), bad); err != nil {
		t.Fatal(err)
	}
	wait("invalid config disables")
	if len(e.p.Tools()) != 0 {
		t.Error("tools published on an invalid configuration")
	}
	st := e.p.Status(context.Background())
	if st.Enabled || len(st.Problems) == 0 || !strings.Contains(st.Problems[0], "deny_origins") {
		t.Errorf("status = %+v", st)
	}
	// allow_script turns browser_evaluate on without a restart.
	cfg := baseConfig()
	cfg.AllowScript = true
	if err := SaveConfig(e.paths.BrowserFile(), cfg); err != nil {
		t.Fatal(err)
	}
	wait("allow_script")
	if !hasTool(e.p.Tools(), "browser_evaluate") {
		t.Error("allow_script not applied")
	}
	// Unknown keys are typos, not silently ignored settings.
	if err := os.WriteFile(e.paths.BrowserFile(), []byte(`{"enabled": true, "deny_origin": ["x.test"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if st := e.p.Status(context.Background()); len(st.Problems) == 0 {
		t.Error("an unknown key was accepted")
	}
}

func TestStatusNeverStartsBrowserAndHasNoSecrets(t *testing.T) {
	cfg := baseConfig()
	cfg.ExtensionToken = "SECRET-TOKEN-123"
	cfg.Command = Command{"npx", "--token", "SECRET-TOKEN-123"}
	n := newNode(t, cfg)
	n.env.fb.addSite("http://a.test/", "Hidden Title")
	res, err := n.env.p.Call(context.Background(), ToolStatus, nil, n.caller)
	if err != nil || isErr(res) {
		t.Fatalf("status: %v %s", err, resultText(res))
	}
	if s, _ := n.env.launch.counts(); s != 0 {
		t.Error("browser_status started the browser")
	}
	n.allowOnce()
	n.call("browser_navigate", map[string]any{"url": "http://a.test/"})
	res, _ = n.env.p.Call(context.Background(), ToolStatus, nil, n.caller)
	txt := resultText(res)
	if strings.Contains(txt, "SECRET") || strings.Contains(txt, "Hidden Title") {
		t.Errorf("status leaks: %s", txt)
	}
	var st Status
	if err := json.Unmarshal([]byte(txt), &st); err != nil || !st.Running || len(st.Tabs) != 1 || st.Tabs[0] != "http://a.test (current)" {
		t.Errorf("status = %+v (%v)", st, err)
	}
	if st.Mode != ModeProfile || st.Channel != "chrome" {
		t.Errorf("status defaults: %+v", st)
	}
}
