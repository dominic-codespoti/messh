package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/catalog"
	"messh/internal/provider"
	"messh/internal/state"
)

// fakeTab is one tab of the fake browser.
type fakeTab struct{ title, url string }

// fakeBrowser speaks Playwright MCP's real result format (captured from
// @playwright/mcp 0.0.83) over a real MCP server, with just enough browser
// behaviour to exercise the gate: tabs, navigation, redirects, link clicks,
// popups, screenshots and uploads.
type fakeBrowser struct {
	t      *testing.T
	outDir string

	mu              sync.Mutex
	tabs            []fakeTab
	cur             int
	titles          map[string]string // url -> title
	redirects       map[string]string // url -> where the server sends you
	links           map[string]string // click target -> url to navigate the current tab to
	popups          map[string]string // click target -> url opened in a new tab
	calls           []string          // "tool args" in order
	uploads         []string          // paths browser_file_upload received
	uploadTxt       []string          // their contents at call time
	pngSize         int               // bytes of the fake screenshot
	badTabs         string            // when set, browser_tabs list returns this text instead
	breakAfterClick string            // when set, a click makes browser_tabs list return this text
	rootsAdvertised bool              // a client advertised the roots capability
	srv             *httptest.Server
}

func newFakeBrowser(t *testing.T, outDir string) *fakeBrowser {
	f := &fakeBrowser{
		t: t, outDir: outDir,
		tabs:      []fakeTab{{"", "about:blank"}},
		titles:    map[string]string{},
		redirects: map[string]string{},
		links:     map[string]string{},
		popups:    map[string]string{},
		pngSize:   2000,
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "Playwright", Version: "1.64.0-fake"}, nil)
	for _, name := range append(allowlisted(), "browser_run_code_unsafe", "browser_install", "browser_cookie_list") {
		name := name
		s.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				if p := req.Session.InitializeParams(); p != nil && p.Capabilities != nil {
					f.mu.Lock()
					f.rootsAdvertised = f.rootsAdvertised || p.Capabilities.RootsV2 != nil
					f.mu.Unlock()
				}
				return f.handle(name, req.Params.Arguments), nil
			})
	}
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, nil)
	f.srv = httptest.NewServer(h)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeBrowser) addSite(url, title string) { f.titles[url] = title }

func (f *fakeBrowser) record(tool string, args json.RawMessage) {
	f.calls = append(f.calls, tool+" "+string(args))
}

// called reports how many recorded calls started with tool.
func (f *fakeBrowser) called(tool string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, tool+" ") {
			n++
		}
	}
	return n
}

func (f *fakeBrowser) current() fakeTab {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tabs[f.cur]
}

func (f *fakeBrowser) tabURLs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, t := range f.tabs {
		out = append(out, t.url)
	}
	return out
}

// open puts a URL in tab i, following the fake server's redirects.
func (f *fakeBrowser) load(i int, url string) {
	for n := 0; n < 5; n++ {
		next, ok := f.redirects[url]
		if !ok {
			break
		}
		url = next
	}
	f.tabs[i] = fakeTab{title: f.titles[url], url: url}
}

func text(s string) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: s}}}
}

func codeSection(code string) string { return "### Ran Playwright code\n```js\n" + code + "\n```\n" }

func (f *fakeBrowser) pageSection() string {
	t := f.tabs[f.cur]
	s := "### Page\n- Page URL: " + t.url + "\n"
	if t.title != "" {
		s += "- Page Title: " + t.title + "\n"
	}
	return s + "### Snapshot\n- [Snapshot](" + filepath.Join("out", "page-1.yml") + ")"
}

func (f *fakeBrowser) tabList() string {
	var b strings.Builder
	for i, t := range f.tabs {
		cur := ""
		if i == f.cur {
			cur = " (current)"
		}
		fmt.Fprintf(&b, "- %d:%s [%s](%s)\n", i, cur, t.title, t.url)
	}
	return strings.TrimRight(b.String(), "\n")
}

func (f *fakeBrowser) openTabsSection() string {
	if len(f.tabs) == 1 {
		return ""
	}
	return "### Open tabs\n" + f.tabList() + "\n"
}

func (f *fakeBrowser) handle(tool string, raw json.RawMessage) *mcp.CallToolResult {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.record(tool, raw)
	var a map[string]any
	json.Unmarshal(raw, &a)
	str := func(k string) string { s, _ := a[k].(string); return s }
	switch tool {
	case "browser_tabs":
		switch str("action") {
		case "list":
			if f.badTabs != "" {
				return text(f.badTabs)
			}
			return text("### Result\n" + f.tabList())
		case "new":
			f.tabs = append(f.tabs, fakeTab{url: "about:blank"})
			f.cur = len(f.tabs) - 1
			if u := str("url"); u != "" {
				f.load(f.cur, u)
			}
			return text("### Result\n" + f.tabList() + "\n" + codeSection("await page.goto()") + f.openTabsSection() + f.pageSection())
		case "select":
			i := int(a["index"].(float64))
			f.cur = i
			return text("### Result\n" + f.tabList())
		case "close":
			i := f.cur
			if v, ok := a["index"].(float64); ok {
				i = int(v)
			}
			f.tabs = append(f.tabs[:i], f.tabs[i+1:]...)
			if f.cur >= len(f.tabs) {
				f.cur = len(f.tabs) - 1
			}
			if len(f.tabs) == 0 {
				return text("### Result\nNo open tabs. Navigate to a URL to create one.")
			}
			return text("### Result\n" + f.tabList())
		}
	case "browser_navigate":
		u := str("url")
		if strings.HasPrefix(u, "file:") {
			r := text("### Error\nError: Access to \"file:\" protocol is blocked. Attempted URL: \"" + u + "\"")
			r.IsError = true
			return r
		}
		f.load(f.cur, u)
		return text(codeSection("await page.goto('"+u+"');") + f.openTabsSection() + f.pageSection())
	case "browser_snapshot":
		t := f.tabs[f.cur]
		return text("### Page\n- Page URL: " + t.url + "\n- Page Title: " + t.title + "\n### Snapshot\n```yaml\n- generic [ref=e1]: content of " + t.url + "\n```")
	case "browser_click":
		target := str("target")
		if u, ok := f.links[target]; ok {
			f.load(f.cur, u)
		}
		if u, ok := f.popups[target]; ok {
			f.tabs = append(f.tabs, fakeTab{})
			f.load(len(f.tabs)-1, u)
		}
		out := text(codeSection("await page.click()") + f.openTabsSection() + f.pageSection())
		if f.breakAfterClick != "" {
			f.badTabs = f.breakAfterClick
		}
		return out
	case "browser_type":
		return text(codeSection("await page.fill()"))
	case "browser_take_screenshot":
		os.MkdirAll(f.outDir, 0o700)
		name := filepath.Join("out", "page-1.png")
		png := make([]byte, f.pngSize)
		copy(png, "\x89PNG\r\n\x1a\n")
		os.WriteFile(filepath.Join(filepath.Dir(f.outDir), name), png, 0o600)
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.TextContent{Text: "### Result\n- [Screenshot of viewport](" + name + ")\n" + codeSection("await page.screenshot()")},
			&mcp.ImageContent{Data: png, MIMEType: "image/png"},
		}}
	case "browser_pdf_save":
		os.MkdirAll(f.outDir, 0o700)
		name := filepath.Join("out", "page-1.pdf")
		os.WriteFile(filepath.Join(filepath.Dir(f.outDir), name), []byte("%PDF-1.4 fake"), 0o600)
		return text("### Result\n- [Page as pdf](" + name + ")\n" + codeSection("await page.pdf()"))
	case "browser_file_upload":
		for _, p := range anyStrings(a["paths"]) {
			f.uploads = append(f.uploads, p)
			b, _ := os.ReadFile(p)
			f.uploadTxt = append(f.uploadTxt, string(b))
		}
		return text(codeSection("await fileChooser.setFiles()") + f.pageSection())
	case "browser_evaluate":
		return text("### Result\n\"ran\"\n" + codeSection("await page.evaluate()"))
	case "browser_close":
		f.tabs, f.cur = nil, 0
		return text("### Result\nNo open tabs. Navigate to a URL to create one.")
	}
	return text("### Result\nok")
}

func anyStrings(v any) []string {
	arr, _ := v.([]any)
	var out []string
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// launcher returns a launcher that "starts" this fake and counts starts.
type fakeLaunch struct {
	f      *fakeBrowser
	mu     sync.Mutex
	starts int
	stops  int
}

func (l *fakeLaunch) launch(ctx context.Context, cfg Config) (*session, error) {
	l.mu.Lock()
	l.starts++
	l.mu.Unlock()
	exited := make(chan struct{})
	var once sync.Once
	conn := catalog.NewConn(ctx, l.f.srv.URL, nil)
	if _, err := conn.Session(ctx); err != nil {
		return nil, err
	}
	tools := map[string]bool{}
	for _, n := range append(allowlisted(), "browser_run_code_unsafe") {
		tools[n] = true
	}
	return &session{
		conn: conn, exited: exited, started: time.Now(), version: "1.64.0-fake", tools: tools,
		stop: func() {
			once.Do(func() {
				l.mu.Lock()
				l.stops++
				l.mu.Unlock()
				close(exited)
			})
		},
	}, nil
}

func (l *fakeLaunch) counts() (starts, stops int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.starts, l.stops
}

// testEnv is a provider on a temp state dir with a fake browser behind it.
type testEnv struct {
	t      *testing.T
	paths  state.Paths
	fb     *fakeBrowser
	launch *fakeLaunch
	p      *Provider
	asked  []string // sites follow-up approvals were requested for
	askMu  sync.Mutex
	answer func(site string) (bool, string) // default: deny
}

func baseConfig() Config {
	return Config{Enabled: true, Executable: os.Args[0]}
}

func newEnv(t *testing.T, cfg Config) *testEnv {
	t.Helper()
	paths := state.Paths{Root: t.TempDir()}
	e := &testEnv{t: t, paths: paths}
	if err := SaveConfig(paths.BrowserFile(), cfg); err != nil {
		t.Fatal(err)
	}
	e.fb = newFakeBrowser(t, filepath.Join(paths.BrowserDir(), "work", "out"))
	e.launch = &fakeLaunch{f: e.fb}
	ctx, cancel := context.WithCancel(context.Background())
	p, err := New(ctx, Options{Paths: paths, launch: e.launch.launch, ReloadEvery: 50 * time.Millisecond,
		Ask: func(ctx context.Context, caller provider.Caller, tool string, ap provider.Approval) (bool, string, error) {
			site := ""
			for _, d := range ap.Details {
				if d.Label == "Moved to" || d.Label == "Site" {
					site = d.Value
				}
			}
			e.askMu.Lock()
			e.asked = append(e.asked, site)
			e.askMu.Unlock()
			if e.answer != nil {
				ok, why := e.answer(site)
				return ok, why, nil
			}
			return false, "denied by test", nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	e.p = p
	t.Cleanup(func() { p.Close(); cancel() })
	return e
}

func (e *testEnv) askedSites() []string {
	e.askMu.Lock()
	defer e.askMu.Unlock()
	return append([]string(nil), e.asked...)
}

// text returns the text of a result.
func resultText(r *mcp.CallToolResult) string { return textOf(r) }

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
