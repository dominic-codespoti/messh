package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	desktopID = "aaaa1111desktop"
	raspiID   = "bbbb2222raspi"
)

// longTool is too long for a "desktop-pc__" prefix, so full mode skips it.
var longTool = strings.Repeat("long_", 11) + "tool"

type backendCall struct {
	device, tool, args, agent string
}

type fakeBackend struct {
	mu       sync.Mutex
	nodes    []Node
	calls    []backendCall
	taskMode bool
}

func (f *fakeBackend) Nodes() []Node { f.mu.Lock(); defer f.mu.Unlock(); return slices.Clone(f.nodes) }
func (f *fakeBackend) Call(_ context.Context, deviceID, tool string, args json.RawMessage, agent string) (*mcp.CallToolResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, backendCall{device: deviceID, tool: tool, args: string(args), agent: agent})
	taskMode := f.taskMode
	f.mu.Unlock()
	if tool == "broken" {
		return nil, errors.New("connection refused")
	}
	if taskMode {
		switch tool {
		case "job_submit":
			return &mcp.CallToolResult{StructuredContent: map[string]any{"job_id": "job-task", "state": "awaiting_approval", "message": "approval pending"}}, nil
		case "job_status":
			return &mcp.CallToolResult{StructuredContent: map[string]any{"job_id": "job-task", "state": "running", "submitted": time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}}, nil
		case "job_cancel":
			return &mcp.CallToolResult{StructuredContent: map[string]any{"job_id": "job-task", "state": "running"}}, nil
		}
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ran " + tool + " on " + deviceID}}}, nil
}

func (f *fakeBackend) lastCall(t *testing.T) backendCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("backend was not called")
	}
	return f.calls[len(f.calls)-1]
}

func (f *fakeBackend) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func tool(name, desc string) *mcp.Tool {
	return &mcp.Tool{Name: name, Description: desc, InputSchema: json.RawMessage(`{"type":"object","properties":{"x":{"type":"string"}}}`)}
}

func testNodes() []Node {
	return []Node{
		{ID: desktopID, Name: "Desktop PC", Self: true, Online: true, LastSeen: time.Now(), Addr: "10.0.0.2:7519", Tools: []*mcp.Tool{
			tool("node_info", "Describe this device."),
			tool("job_submit", "Run a command as a background job."),
			tool("voicestudio__generate_speech", "Generate speech audio from text."),
			tool(longTool, "A tool whose prefixed name is too long."),
			{Name: "bad_schema", Description: "Not an object schema.", InputSchema: json.RawMessage(`{"type":"string"}`)},
		}},
		{ID: raspiID, Name: "raspi", Online: true, LastSeen: time.Now(), Tools: []*mcp.Tool{
			tool("node_info", "Describe this device."),
			tool("job_status", "Report a job's state."),
			tool("files_list", "List files in a workspace."),
		}},
	}
}

type harness struct {
	t   *testing.T
	be  *fakeBackend
	g   *Gateway
	url string

	mu    sync.Mutex
	modes map[string]string
}

func newHarness(t *testing.T) *harness {
	h := &harness{t: t, be: &fakeBackend{nodes: testNodes()}, modes: map[string]string{"wide": ModeFull}}
	h.g = New(h.be, "test", slog.New(slog.NewTextHandler(io.Discard, nil)))
	h.g.AddTool(&mcp.Tool{
		Name:        "mesh_copy",
		Description: "Copy between devices.",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "copied"}}}, nil
	})
	verify := func(_ context.Context, tok string, _ *http.Request) (*auth.TokenInfo, error) {
		if name, ok := strings.CutPrefix(tok, "tok-"); ok {
			return &auth.TokenInfo{UserID: name}, nil
		}
		return nil, auth.ErrInvalidToken
	}
	srv := httptest.NewServer(h.g.Handler(verify, h.mode))
	t.Cleanup(srv.Close)
	h.url = srv.URL
	return h
}

func (h *harness) mode(agent string) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.modes[agent]
}

func (h *harness) setMode(agent, mode string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.modes[agent] = mode
}

type authHeader string

func (a authHeader) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", string(a))
	return http.DefaultTransport.RoundTrip(r)
}

func (h *harness) session(agent string) *mcp.ClientSession {
	h.t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "1"}, nil)
	s, err := client.Connect(h.t.Context(), &mcp.StreamableClientTransport{
		Endpoint:             h.url,
		HTTPClient:           &http.Client{Transport: authHeader("Bearer tok-" + agent)},
		MaxRetries:           -1,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	h.t.Cleanup(func() { s.Close() })
	return s
}

func toolNames(t *testing.T, s *mcp.ClientSession) []string {
	t.Helper()
	var names []string
	for tl, err := range s.Tools(t.Context(), nil) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, tl.Name)
	}
	slices.Sort(names)
	return names
}

func call(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return res
}

func text(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func searchTools(t *testing.T, s *mcp.ClientSession, args map[string]any) toolsResult {
	t.Helper()
	res := call(t, s, "mesh_tools", args)
	if res.IsError {
		t.Fatalf("mesh_tools %v: %s", args, text(res))
	}
	var out toolsResult
	if err := json.Unmarshal([]byte(text(res)), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func qualified(r toolsResult) []string {
	var out []string
	for _, ti := range r.Tools {
		out = append(out, ti.Device+"/"+ti.Name)
	}
	return out
}

func TestCompactListsOnlyMeshTools(t *testing.T) {
	h := newHarness(t)
	got := toolNames(t, h.session("omp"))
	want := []string{"mesh_call", "mesh_copy", "mesh_nodes", "mesh_tools"}
	if !slices.Equal(got, want) {
		t.Fatalf("compact tools = %v, want %v", got, want)
	}
}

func TestFullListsDeviceAndMeshTools(t *testing.T) {
	h := newHarness(t)
	got := toolNames(t, h.session("wide"))
	want := []string{
		"desktop-pc__job_submit", "desktop-pc__node_info", "desktop-pc__voicestudio__generate_speech",
		"mesh_copy", "mesh_nodes",
		"raspi__files_list", "raspi__job_status", "raspi__node_info",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("full tools = %v, want %v", got, want)
	}
	res := call(t, h.session("wide"), "raspi__job_status", map[string]any{"x": "1"})
	if c := h.be.lastCall(t); res.IsError || c.device != raspiID || c.tool != "job_status" || c.agent != "wide" {
		t.Fatalf("direct call: %s, backend saw %+v", text(res), c)
	}
}

func TestModeChangeAppliesToNextRequest(t *testing.T) {
	h := newHarness(t)
	s := h.session("omp")
	if names := toolNames(t, s); slices.Contains(names, "raspi__node_info") {
		t.Fatalf("compact agent saw device tools: %v", names)
	}
	h.setMode("omp", ModeFull)
	if names := toolNames(t, s); !slices.Contains(names, "raspi__node_info") || slices.Contains(names, "mesh_tools") {
		t.Fatalf("after switching to full the same session lists %v", names)
	}
	h.setMode("omp", ModeCompact)
	if names := toolNames(t, s); slices.Contains(names, "raspi__node_info") {
		t.Fatalf("after switching back to compact the session lists %v", names)
	}
}

func TestMeshToolsSearch(t *testing.T) {
	h := newHarness(t)
	s := h.session("omp")

	all := searchTools(t, s, nil)
	wantAll := []string{
		"desktop-pc/job_submit", "desktop-pc/" + longTool, "desktop-pc/node_info", "desktop-pc/voicestudio__generate_speech",
		"raspi/files_list", "raspi/job_status", "raspi/node_info",
	}
	if all.Matched != 7 || all.Returned != 7 || !slices.Equal(qualified(all), wantAll) || all.Note != "" {
		t.Fatalf("all tools = %+v (%v), want %v", all, qualified(all), wantAll)
	}
	if all.Tools[3].Description != "Generate speech audio from text." || all.Tools[3].InputSchema == nil {
		t.Fatalf("tool definition incomplete: %+v", all.Tools[3])
	}

	speech := searchTools(t, s, map[string]any{"query": "SPEECH"})
	if !slices.Equal(qualified(speech), []string{"desktop-pc/voicestudio__generate_speech"}) || speech.Matched != 1 {
		t.Fatalf("query SPEECH = %v", qualified(speech))
	}
	if got := qualified(searchTools(t, s, map[string]any{"query": "node_info"})); !slices.Equal(got, []string{"desktop-pc/node_info", "raspi/node_info"}) {
		t.Fatalf("query node_info = %v", got)
	}
	if got := qualified(searchTools(t, s, map[string]any{"query": "run job"})); !slices.Equal(got, []string{"desktop-pc/job_submit"}) {
		t.Fatalf("query \"run job\" = %v", got)
	}
	if none := searchTools(t, s, map[string]any{"query": "teleport"}); none.Matched != 0 || len(none.Tools) != 0 || none.Note == "" {
		t.Fatalf("query teleport = %+v", none)
	}

	for _, dev := range []string{"raspi", "RASPI", "bbbb2222"} {
		r := searchTools(t, s, map[string]any{"device": dev})
		if !slices.Equal(qualified(r), []string{"raspi/files_list", "raspi/job_status", "raspi/node_info"}) || r.Matched != 3 {
			t.Fatalf("device %q = %v", dev, qualified(r))
		}
	}
	if r := searchTools(t, s, map[string]any{"device": "Desktop PC", "query": "job"}); !slices.Equal(qualified(r), []string{"desktop-pc/job_submit"}) {
		t.Fatalf("device+query = %v", qualified(r))
	}

	limited := searchTools(t, s, map[string]any{"limit": 2})
	if limited.Matched != 7 || limited.Returned != 2 || !slices.Equal(qualified(limited), wantAll[:2]) || limited.Note == "" {
		t.Fatalf("limit 2 = %+v", limited)
	}
	if big := searchTools(t, s, map[string]any{"limit": 1000}); big.Returned != 7 {
		t.Fatalf("limit 1000 returned %d", big.Returned)
	}

	if res := call(t, s, "mesh_tools", map[string]any{"device": "toaster"}); !res.IsError || !strings.Contains(text(res), "toaster") {
		t.Fatalf("unknown device: %s", text(res))
	}
	if res := call(t, s, "mesh_tools", map[string]any{"limit": -1}); !res.IsError {
		t.Fatalf("negative limit accepted: %s", text(res))
	}
}

func TestMeshToolsRanksNameMatchesFirst(t *testing.T) {
	h := newHarness(t)
	h.be.mu.Lock()
	h.be.nodes[0].Tools = []*mcp.Tool{
		tool("files_delete", "Delete working files for jobs."),
		tool("job_submit", "Run a command as a background job."),
		tool("job_status", "Report the state of a job started by job_submit."),
		tool("node_info", "Describe this device."),
	}
	h.be.mu.Unlock()
	h.g.Sync()
	s := h.session("omp")
	search := func(args map[string]any) toolsResult {
		args["device"] = "desktop-pc"
		return searchTools(t, s, args)
	}

	page := search(map[string]any{"query": "job", "limit": 2})
	if page.Matched != 3 || page.Returned != 2 || page.Note == "" ||
		!slices.Equal(qualified(page), []string{"desktop-pc/job_status", "desktop-pc/job_submit"}) {
		t.Fatalf("query job limit 2 = %+v (%v)", page, qualified(page))
	}
	if got := qualified(search(map[string]any{"query": "job"})); !slices.Equal(got,
		[]string{"desktop-pc/job_status", "desktop-pc/job_submit", "desktop-pc/files_delete"}) {
		t.Fatalf("query job = %v", got)
	}
	if got := qualified(search(map[string]any{"query": "files"})); len(got) == 0 || got[0] != "desktop-pc/files_delete" {
		t.Fatalf("query files = %v", got)
	}
	if got := qualified(search(map[string]any{"query": "JOB_SUBMIT"})); !slices.Equal(got,
		[]string{"desktop-pc/job_submit", "desktop-pc/job_status"}) {
		t.Fatalf("exact-name query = %v", got)
	}
	if got := qualified(search(map[string]any{"query": "job job_submit"})); !slices.Equal(got,
		[]string{"desktop-pc/job_submit", "desktop-pc/job_status"}) {
		t.Fatalf("name equal to one word = %v", got)
	}
}

func TestDefaultLimit(t *testing.T) {
	h := newHarness(t)
	var many []*mcp.Tool
	for i := range 30 {
		many = append(many, tool("t"+strings.Repeat("x", i), "filler"))
	}
	h.be.mu.Lock()
	h.be.nodes[1].Tools = many
	h.be.mu.Unlock()
	h.g.Sync()
	r := searchTools(t, h.session("omp"), map[string]any{"device": "raspi"})
	if r.Matched != 30 || r.Returned != 20 {
		t.Fatalf("default limit: matched %d returned %d", r.Matched, r.Returned)
	}
}

func TestMeshCallRoutes(t *testing.T) {
	h := newHarness(t)
	s := h.session("omp")

	res := call(t, s, "mesh_call", map[string]any{"device": "raspi", "tool": "job_status", "arguments": map[string]any{"job_id": "j1"}})
	c := h.be.lastCall(t)
	if res.IsError || text(res) != "ran job_status on "+raspiID {
		t.Fatalf("result = %s", text(res))
	}
	if c.device != raspiID || c.tool != "job_status" || c.agent != "omp" || c.args != `{"job_id":"j1"}` {
		t.Fatalf("backend saw %+v", c)
	}

	res = call(t, s, "mesh_call", map[string]any{"device": "Desktop PC", "tool": "voicestudio__generate_speech"})
	c = h.be.lastCall(t)
	if res.IsError || c.device != desktopID || c.tool != "voicestudio__generate_speech" || c.args != "{}" {
		t.Fatalf("catalogue proxy call: %s, backend saw %+v", text(res), c)
	}

	// Compact mode reaches tools whose prefixed name full mode cannot expose.
	res = call(t, s, "mesh_call", map[string]any{"device": "aaaa1111", "tool": longTool, "arguments": map[string]any{}})
	if c = h.be.lastCall(t); res.IsError || c.tool != longTool || c.device != desktopID {
		t.Fatalf("long tool: %s, backend saw %+v", text(res), c)
	}
}

func TestMeshCallErrors(t *testing.T) {
	h := newHarness(t)
	s := h.session("omp")
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"unknown device", map[string]any{"device": "toaster", "tool": "node_info"}, "no device"},
		{"unknown tool", map[string]any{"device": "raspi", "tool": "job_submit"}, "mesh_tools"},
		{"prefixed tool", map[string]any{"device": "raspi", "tool": "raspi__node_info"}, "no tool"},
		{"missing tool", map[string]any{"device": "raspi"}, "required"},
		{"array arguments", map[string]any{"device": "raspi", "tool": "node_info", "arguments": []int{1}}, "object"},
		{"string arguments", map[string]any{"device": "raspi", "tool": "node_info", "arguments": "x=1"}, "object"},
	}
	for _, tc := range cases {
		res := call(t, s, "mesh_call", tc.args)
		if !res.IsError || !strings.Contains(text(res), tc.want) {
			t.Fatalf("%s: IsError=%v %q, want error containing %q", tc.name, res.IsError, text(res), tc.want)
		}
	}
	if n := h.be.callCount(); n != 0 {
		t.Fatalf("refused calls reached the backend %d times", n)
	}

	h.be.mu.Lock()
	h.be.nodes[1].Tools = append(h.be.nodes[1].Tools, tool("broken", "Always fails."))
	h.be.mu.Unlock()
	h.g.Sync()
	res := call(t, s, "mesh_call", map[string]any{"device": "raspi", "tool": "broken"})
	if !res.IsError || !strings.Contains(text(res), "raspi") || !strings.Contains(text(res), "connection refused") {
		t.Fatalf("backend error: IsError=%v %q", res.IsError, text(res))
	}
}

func TestMeshNodesByMode(t *testing.T) {
	h := newHarness(t)

	res := call(t, h.session("omp"), "mesh_nodes", nil)
	var compact []map[string]any
	if err := json.Unmarshal([]byte(text(res)), &compact); err != nil {
		t.Fatal(err)
	}
	if len(compact) != 2 || compact[1]["handle"] != "raspi" || compact[1]["id"] != raspiID || compact[1]["online"] != true {
		t.Fatalf("compact mesh_nodes = %v", compact)
	}
	if _, ok := compact[0]["addr"]; ok {
		t.Fatalf("compact mesh_nodes carries addresses: %v", compact[0])
	}
	tools, _ := compact[1]["tools"].([]any)
	if len(tools) != 3 || tools[0] != "files_list" {
		t.Fatalf("compact tool names = %v", compact[1]["tools"])
	}

	res = call(t, h.session("wide"), "mesh_nodes", nil)
	if !strings.Contains(text(res), `"raspi__job_status"`) || !strings.Contains(text(res), "Report a job's state.") {
		t.Fatalf("full mesh_nodes lost names or descriptions: %s", text(res))
	}
}

func TestSyncUpdatesBothModes(t *testing.T) {
	h := newHarness(t)
	h.be.mu.Lock()
	h.be.nodes[1].Tools = []*mcp.Tool{tool("node_info", "Describe this device."), tool("gpu_run", "Run on the GPU.")}
	h.be.mu.Unlock()
	h.g.Sync()

	full := toolNames(t, h.session("wide"))
	if !slices.Contains(full, "raspi__gpu_run") || slices.Contains(full, "raspi__files_list") {
		t.Fatalf("full tools after Sync = %v", full)
	}

	s := h.session("omp")
	if got := qualified(searchTools(t, s, map[string]any{"device": "raspi"})); !slices.Equal(got, []string{"raspi/gpu_run", "raspi/node_info"}) {
		t.Fatalf("compact catalogue after Sync = %v", got)
	}
	if res := call(t, s, "mesh_call", map[string]any{"device": "raspi", "tool": "gpu_run"}); res.IsError {
		t.Fatalf("new tool not callable: %s", text(res))
	}
	if res := call(t, s, "mesh_call", map[string]any{"device": "raspi", "tool": "files_list"}); !res.IsError {
		t.Fatalf("removed tool still callable: %s", text(res))
	}
	if res := call(t, s, "mesh_nodes", nil); !strings.Contains(text(res), `"gpu_run"`) || strings.Contains(text(res), "files_list") {
		t.Fatalf("compact mesh_nodes after Sync: %s", text(res))
	}
}
