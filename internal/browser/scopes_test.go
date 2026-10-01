package browser

import (
	"context"
	"strings"
	"testing"

	"messh/internal/approval"
	"messh/internal/provider"
)

func approvalFor(t *testing.T, p *Provider, tool string, args any) provider.Approval {
	t.Helper()
	ap, err := p.Approval(context.Background(), tool, mustJSON(args), provider.Caller{DeviceID: "d", Agent: "omp"})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return ap
}

func scopeKeys(ap provider.Approval) []string {
	var out []string
	for _, s := range ap.Scopes {
		out = append(out, s.Key)
	}
	return out
}

func TestScopeLadder(t *testing.T) {
	n := newNode(t, baseConfig())
	n.env.fb.addSite("http://a.test/", "A")
	n.allowOnce()
	n.call("browser_navigate", map[string]any{"url": "http://a.test/"})
	p := n.env.p

	read := approvalFor(t, p, "browser_snapshot", map[string]any{})
	if got := strings.Join(scopeKeys(read), " "); got != "browser:http://a.test:read browser:http://a.test:interact browser:*" {
		t.Errorf("read scopes = %s", got)
	}
	if !read.Scopes[2].Broad || read.Scopes[0].Broad || read.Scopes[1].Broad {
		t.Errorf("only the any-site scope is broad: %+v", read.Scopes)
	}
	click := approvalFor(t, p, "browser_click", map[string]any{"target": "e1"})
	if got := strings.Join(scopeKeys(click), " "); got != "browser:http://a.test:interact browser:*" {
		t.Errorf("interact scopes = %s", got)
	}
	up := approvalFor(t, p, "browser_type", map[string]any{"target": "e1", "text": "x"})
	if strings.Join(scopeKeys(up), " ") != strings.Join(scopeKeys(click), " ") {
		t.Errorf("type scopes = %v", scopeKeys(up))
	}
	nav := approvalFor(t, p, "browser_navigate", map[string]any{"url": "https://b.test:8443/x"})
	if got := strings.Join(scopeKeys(nav), " "); got != "browser:https://b.test:8443:read browser:https://b.test:8443:interact browser:*" {
		t.Errorf("navigate scopes = %s", got)
	}
	if !strings.Contains(nav.Title, "wants to view b.test") && !strings.Contains(nav.Title, "https://b.test:8443") {
		t.Errorf("title = %q", nav.Title)
	}
	if !strings.Contains(nav.Title, "omp") || nav.Exact == "" || nav.Auto != "" {
		t.Errorf("approval = %+v", nav)
	}
}

// The saved-rule semantics come from the real approval engine: a rule matches
// a request whose exact hash or any of its scope keys equals the rule's key.
func TestSavedRulesAcrossLevels(t *testing.T) {
	n := newNode(t, baseConfig())
	fb := n.env.fb
	fb.addSite("http://a.test/", "A")
	fb.addSite("http://b.test/", "B")
	writeWS(t, n.env.paths.Root, "up.txt", "hello")

	// 1. Always allow "view and interact" on a.test (answered on a click)...
	n.alwaysScope("interact with http://a.test")
	n.call("browser_navigate", map[string]any{"url": "http://a.test/"}) // prompt 1: saves the interact rule via the navigate's scope ladder
	n.surf.reset()
	// ...then reads and interaction on a.test, and a navigate to it, run unprompted.
	n.denyAll()
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"browser_snapshot", map[string]any{}},
		{"browser_take_screenshot", map[string]any{}},
		{"browser_click", map[string]any{"element": "x", "target": "e1"}},
		{"browser_navigate", map[string]any{"url": "http://a.test/"}},
	} {
		if res := n.call(c.tool, c.args); isErr(res) {
			t.Errorf("%s should be covered by the saved interact rule: %s", c.tool, resultText(res))
		}
	}
	if len(n.surf.asked()) != 0 {
		t.Errorf("covered calls prompted: %v", n.surf.asked())
	}
	// ...but not another site, uploads, or scripts.
	if res := n.call("browser_navigate", map[string]any{"url": "http://b.test/"}); !isErr(res) {
		t.Error("an a.test rule covered b.test")
	}
	n.call("browser_navigate", map[string]any{"url": "http://a.test/"})
	n.surf.reset()
	if res := n.call("browser_file_upload", map[string]any{"paths": []string{"ws/up.txt"}}); !isErr(res) {
		t.Errorf("an interact rule covered an upload: %s", resultText(res))
	}
	if len(n.surf.asked()) != 1 {
		t.Errorf("upload should have prompted once: %d", len(n.surf.asked()))
	}
}

func TestReadRuleDoesNotCoverInteraction(t *testing.T) {
	n := newNode(t, baseConfig())
	fb := n.env.fb
	fb.addSite("http://a.test/", "A")
	n.alwaysScope("view http://a.test only")
	n.call("browser_navigate", map[string]any{"url": "http://a.test/"})
	n.surf.reset()
	n.denyAll()
	if res := n.call("browser_snapshot", map[string]any{}); isErr(res) {
		t.Errorf("read rule should cover reads: %s", resultText(res))
	}
	if res := n.call("browser_click", map[string]any{"element": "x", "target": "e1"}); !isErr(res) {
		t.Error("a read-only rule covered a click")
	}
	if len(n.surf.asked()) != 1 {
		t.Errorf("click should have prompted once, got %d", len(n.surf.asked()))
	}
}

func TestAnySiteRuleAndCallerBinding(t *testing.T) {
	cfg := baseConfig()
	cfg.AllowScript = true
	n := newNode(t, cfg)
	fb := n.env.fb
	fb.addSite("http://a.test/", "A")
	fb.addSite("http://b.test/", "B")
	writeWS(t, n.env.paths.Root, "up.txt", "hello")
	n.alwaysScope("ANY website")
	n.call("browser_navigate", map[string]any{"url": "http://a.test/"})
	n.surf.reset()
	n.denyAll()
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"browser_navigate", map[string]any{"url": "http://b.test/"}},
		{"browser_click", map[string]any{"element": "x", "target": "e1"}},
		{"browser_evaluate", map[string]any{"function": "() => 1"}},
		{"browser_file_upload", map[string]any{"paths": []string{"ws/up.txt"}}},
	} {
		if res := n.call(c.tool, c.args); isErr(res) {
			t.Errorf("%s not covered by browser:*: %s", c.tool, resultText(res))
		}
	}
	if len(n.surf.asked()) != 0 {
		t.Errorf("prompted despite a browser:* rule: %d", len(n.surf.asked()))
	}
	// The rule belongs to this agent on this device.
	other := n.caller
	other.Agent = "pi"
	ap, err := n.env.p.Approval(context.Background(), "browser_navigate", mustJSON(map[string]any{"url": "http://b.test/"}), other)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := n.eng.Decide(context.Background(), approval.Request{Caller: other, Tool: "browser_navigate", Class: provider.ClassBrowser, Approval: ap})
	if err != nil || dec.Allowed {
		t.Errorf("another agent inherited the rule: %+v %v", dec, err)
	}
}

func TestScriptAndUploadHaveTheirOwnScopes(t *testing.T) {
	cfg := baseConfig()
	cfg.AllowScript = true
	n := newNode(t, cfg)
	n.env.fb.addSite("http://a.test/", "A")
	writeWS(t, n.env.paths.Root, "up.txt", "hello")
	n.allowOnce()
	n.call("browser_navigate", map[string]any{"url": "http://a.test/"})
	ev := approvalFor(t, n.env.p, "browser_evaluate", map[string]any{"function": "() => document.cookie"})
	if got := strings.Join(scopeKeys(ev), " "); got != "browser:http://a.test:script browser:*" {
		t.Errorf("evaluate scopes = %s", got)
	}
	hasScript := false
	for _, d := range ev.Details {
		if d.Label == "Script" && strings.Contains(d.Value, "document.cookie") {
			hasScript = true
		}
	}
	if !hasScript || !strings.Contains(ev.Title, "JavaScript") {
		t.Errorf("evaluate prompt does not show the script: %+v", ev)
	}
	up := approvalFor(t, n.env.p, "browser_file_upload", map[string]any{"paths": []string{"ws/up.txt"}})
	if got := strings.Join(scopeKeys(up), " "); got != "browser:http://a.test:upload browser:*" {
		t.Errorf("upload scopes = %s", got)
	}
	var file bool
	for _, d := range up.Details {
		if d.Label == "File" && strings.Contains(d.Value, "ws/up.txt") && strings.Contains(d.Value, "5 bytes") {
			file = true
		}
	}
	if !file {
		t.Errorf("upload prompt does not list the file: %+v", up.Details)
	}
}

func TestExactIsStableAndSensitive(t *testing.T) {
	n := newNode(t, baseConfig())
	n.env.fb.addSite("http://a.test/", "A")
	n.env.fb.addSite("http://b.test/", "B")
	n.allowOnce()
	n.call("browser_navigate", map[string]any{"url": "http://a.test/"})
	p := n.env.p
	a1 := approvalFor(t, p, "browser_click", map[string]any{"element": "go", "target": "e3"}).Exact
	raw := mustJSON(map[string]any{"target": "e3", "element": "go"}) // different key order
	ap, err := p.Approval(context.Background(), "browser_click", raw, provider.Caller{DeviceID: "d", Agent: "omp"})
	if err != nil || ap.Exact != a1 {
		t.Errorf("key order changed Exact: %s vs %s (%v)", ap.Exact, a1, err)
	}
	spaced := []byte(`{ "element" : "go",
	  "target":"e3" }`)
	if ap, _ := p.Approval(context.Background(), "browser_click", spaced, provider.Caller{DeviceID: "d", Agent: "omp"}); ap.Exact != a1 {
		t.Error("whitespace changed Exact")
	}
	if b := approvalFor(t, p, "browser_click", map[string]any{"element": "go", "target": "e4"}).Exact; b == a1 {
		t.Error("a different target kept the same Exact")
	}
	if b := approvalFor(t, p, "browser_click", map[string]any{"element": "go", "target": "e3", "doubleClick": true}).Exact; b == a1 {
		t.Error("an extra argument kept the same Exact")
	}
	if b := approvalFor(t, p, "browser_hover", map[string]any{"element": "go", "target": "e3"}).Exact; b == a1 {
		t.Error("a different tool kept the same Exact")
	}
	// The same arguments on another site are another request.
	n.call("browser_navigate", map[string]any{"url": "http://b.test/"})
	if b := approvalFor(t, p, "browser_click", map[string]any{"element": "go", "target": "e3"}).Exact; b == a1 {
		t.Error("the same click on another site kept the same Exact")
	}
}

// If the page moved between the gate and the call, the old approval does not
// cover the new page: Call asks again before doing anything.
func TestApprovalDoesNotSurviveAPageChange(t *testing.T) {
	n := newNode(t, baseConfig())
	fb := n.env.fb
	fb.addSite("http://a.test/", "A")
	fb.addSite("http://b.test/", "B")
	n.allowOnce()
	n.call("browser_navigate", map[string]any{"url": "http://a.test/"})

	args := mustJSON(map[string]any{"element": "x", "target": "e1"})
	ap, err := n.env.p.Approval(context.Background(), "browser_click", args, n.caller)
	if err != nil {
		t.Fatal(err)
	}
	_ = ap // approved for a.test
	fb.mu.Lock()
	fb.load(fb.cur, "http://b.test/") // the page moves on its own
	fb.mu.Unlock()
	n.denyAll()
	res, err := n.env.p.Call(context.Background(), "browser_click", args, n.caller)
	if err != nil {
		t.Fatal(err)
	}
	if !isErr(res) || !strings.Contains(resultText(res), "not approved") {
		t.Errorf("click ran on a page the gate never saw: %s", resultText(res))
	}
	if fb.called("browser_click") != 0 {
		t.Error("the click reached the browser")
	}
}
