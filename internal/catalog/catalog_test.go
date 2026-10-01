package catalog

import (
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/files"
	"messh/internal/provider"
	"messh/internal/state"
)

func decodeText(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, s)
	}
	return m
}

func toolByName(env *testEnv, name string) (provider.Tool, bool) {
	for _, tl := range env.p.Tools() {
		if tl.Def.Name == name {
			return tl, true
		}
	}
	return provider.Tool{}, false
}

func TestMCPProxyLegacySession(t *testing.T) {
	f := newFakeMCP(t)
	env := newEnv(t, Service{Name: "voice", Kind: KindMCP, URL: f.endpoint(), Description: "voice generation", Tags: []string{"tts"}})

	for _, name := range []string{"catalogue", "voice__check_health", "voice__generate_speech", "voice__transcribe"} {
		if !env.hasTool(name) {
			t.Fatalf("missing tool %s in %v", name, env.toolNames())
		}
	}
	if env.hasTool("service_call") || env.hasTool("openai_chat") {
		t.Fatalf("generic tools published without a matching service: %v", env.toolNames())
	}
	tl, _ := toolByName(env, "voice__check_health")
	if tl.Class != provider.ClassService {
		t.Errorf("proxy class = %s", tl.Class)
	}
	if !strings.HasPrefix(tl.Def.Description, "[service voice] ") {
		t.Errorf("description = %q", tl.Def.Description)
	}
	if tl.Def.Annotations == nil || !tl.Def.Annotations.ReadOnlyHint {
		t.Errorf("upstream readOnlyHint not carried over: %+v", tl.Def.Annotations)
	}
	gs, _ := toolByName(env, "voice__generate_speech")
	if gs.Def.Annotations != nil {
		t.Errorf("annotations invented for a tool without any: %+v", gs.Def.Annotations)
	}
	schema, _ := gs.Def.InputSchema.(map[string]any)
	if req, _ := schema["required"].([]any); len(req) != 1 || req[0] != "text" {
		t.Errorf("input schema not preserved: %v", gs.Def.InputSchema)
	}
	if cat, _ := toolByName(env, "catalogue"); cat.Class != provider.ClassInfo {
		t.Errorf("catalogue class = %s", cat.Class)
	}

	res, text := env.call("voice__check_health", map[string]any{})
	if res.IsError || !strings.Contains(text, "ok") {
		t.Fatalf("check_health: %v %s", res.IsError, text)
	}
	env.call("voice__generate_speech", map[string]any{"text": "hi"})
	if got := f.callLog(); !slices.Equal(got, []string{"check_health", "generate_speech"}) {
		t.Errorf("upstream calls = %v", got)
	}
	if n := f.inits.Load(); n != 1 {
		t.Errorf("session not pooled: %d initializations", n)
	}
	if string(f.args()["text"]) != `"hi"` {
		t.Errorf("arguments not forwarded: %v", f.args())
	}
}

func TestMCPReconnectAfterSessionLoss(t *testing.T) {
	f := newFakeMCP(t)
	// Slow probing so the call itself meets the dead session.
	env := newEnvWith(t, func(o *Options) { o.ProbeEvery, o.MaxBackoff = time.Hour, time.Hour }, Service{Name: "voice", Kind: KindMCP, URL: f.endpoint()})
	f.killSessions()
	res, text := env.call("voice__check_health", map[string]any{})
	if res.IsError || !strings.Contains(text, "ok") {
		t.Fatalf("call after session loss: %v %s", res.IsError, text)
	}
	if f.inits.Load() < 2 {
		t.Errorf("expected a fresh session, inits=%d", f.inits.Load())
	}
}

func TestMCPLostReplyIsNotReplayed(t *testing.T) {
	f := newFakeMCP(t)
	env := newEnv(t, Service{Name: "voice", Kind: KindMCP, URL: f.endpoint()})
	f.loseReply.Store(true)
	res, _ := env.call("voice__generate_speech", map[string]any{"text": "once"})
	if !res.IsError {
		t.Fatal("expected an error when the reply is lost")
	}
	if n := strings.Count(strings.Join(f.callLog(), ","), "generate_speech"); n != 1 {
		t.Errorf("a possibly-executed call was replayed: %d upstream calls", n)
	}
}

func TestMCPArtifactExtraction(t *testing.T) {
	f := newFakeMCP(t)
	env := newEnv(t, Service{Name: "voice", Kind: KindMCP, URL: f.endpoint()})

	check := func(tool string) {
		t.Helper()
		res, text := env.call(tool, map[string]any{})
		if res.IsError {
			t.Fatalf("%s: %s", tool, text)
		}
		if strings.Contains(text, "UklGR") {
			t.Fatalf("%s: base64 audio leaked into the reply: %.200s", tool, text)
		}
		if res.StructuredContent != nil {
			if b, _ := json.Marshal(res.StructuredContent); strings.Contains(string(b), "UklGR") {
				t.Fatalf("%s: base64 in structured content", tool)
			}
		}
		var made []Artifact
		for _, line := range strings.Split(text, "\n") {
			var m struct {
				Artifacts []Artifact `json:"artifacts"`
			}
			if json.Unmarshal([]byte(line), &m) == nil {
				made = append(made, m.Artifacts...)
			}
		}
		if len(made) != 1 {
			t.Fatalf("%s: want 1 artifact in reply, got %v\n%s", tool, made, text)
		}
		a := made[0]
		if !strings.HasPrefix(a.Ref, "artifacts/voice/") || !strings.HasSuffix(a.Ref, ".wav") || a.Mime != "audio/wav" {
			t.Errorf("%s: artifact = %+v", tool, a)
		}
		abs, err := files.Resolve(env.paths, files.Ref(a.Ref))
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(abs)
		if err != nil || len(got) != len(wavBytes(2000)) || a.Bytes != int64(len(got)) {
			t.Errorf("%s: artifact file %v (%d bytes), reply says %d", tool, err, len(got), a.Bytes)
		}
	}
	check("voice__make_blob")      // MCP audio content
	check("voice__make_json_blob") // base64 inside JSON text
}

func TestMCPPathArgRefs(t *testing.T) {
	f := newFakeMCP(t)
	env := newEnv(t, Service{Name: "voice", Kind: KindMCP, URL: f.endpoint()})
	abs := env.write("ws/job1/a.wav", wavBytes(10))

	tl, _ := toolByName(env, "voice__transcribe")
	if !strings.Contains(tl.Def.Description, "audio_path") || !strings.Contains(tl.Def.Description, "file ref") {
		t.Errorf("description does not document the ref translation: %q", tl.Def.Description)
	}

	res, text := env.call("voice__transcribe", map[string]any{"audio_path": "ws/job1/a.wav"})
	if res.IsError {
		t.Fatalf("transcribe: %s", text)
	}
	var sent string
	json.Unmarshal(f.args()["audio_path"], &sent)
	if sent != abs {
		t.Errorf("upstream got %q, want the resolved host path %q", sent, abs)
	}
	if ap, err := env.approval("voice__transcribe", map[string]any{"audio_path": "ws/job1/a.wav"}); err != nil {
		t.Fatal(err)
	} else if strings.Contains(detailsText(ap), abs) {
		t.Errorf("approval shows the host path instead of the ref: %s", detailsText(ap))
	}

	before := len(f.callLog())
	for _, bad := range []string{"ws/../../etc/passwd", "ws/job1/../../../x", `C:\Windows\win.ini`, "/etc/passwd", "~/x", "other:ws/job1/a.wav"} {
		res, text := env.call("voice__transcribe", map[string]any{"audio_path": bad})
		if !res.IsError {
			t.Errorf("audio_path %q was accepted: %s", bad, text)
		}
		if _, err := env.approval("voice__transcribe", map[string]any{"audio_path": bad}); err == nil {
			t.Errorf("approval built for %q", bad)
		}
	}
	if len(f.callLog()) != before {
		t.Errorf("refused calls reached the upstream: %v", f.callLog())
	}
}

func TestMCPAllowDeny(t *testing.T) {
	f := newFakeMCP(t)
	env := newEnv(t, Service{Name: "voice", Kind: KindMCP, URL: f.endpoint(), Deny: []string{"generate_*", "leak"}, Allow: []string{"check_health", "generate_speech", "transcribe", "make_*"}})
	if !env.hasTool("voice__check_health") || !env.hasTool("voice__transcribe") {
		t.Fatalf("allowed tools missing: %v", env.toolNames())
	}
	if env.hasTool("voice__generate_speech") || env.hasTool("voice__leak") {
		t.Fatalf("denied tools published: %v", env.toolNames())
	}
	res, _ := env.call("voice__generate_speech", map[string]any{"text": "x"})
	if !res.IsError {
		t.Error("call to a denied tool succeeded")
	}
}

func TestToolNameShortening(t *testing.T) {
	if got := proxyName("voice", "check_health"); got != "voice__check_health" {
		t.Errorf("plain name changed: %s", got)
	}
	long1 := "a_very_long_upstream_tool_name_that_goes_on_one"
	long2 := "a_very_long_upstream_tool_name_that_goes_on_two"
	a, b := proxyName("voice", long1), proxyName("voice", long2)
	if len(a) > maxToolName || len(b) > maxToolName || a == b {
		t.Errorf("long names: %q %q", a, b)
	}
	if proxyName("voice", long1) != a {
		t.Error("shortening is not deterministic")
	}
	longSvc := strings.Repeat("s", 24)
	if n := proxyName(longSvc, strings.Repeat("t", 80)); len(n) > maxToolName {
		t.Errorf("name too long: %d", len(n))
	}
	w1, w2 := proxyName("voice", "a.b"), proxyName("voice", "a_b")
	if w1 == w2 || strings.ContainsAny(w1, ". ") {
		t.Errorf("sanitised names collide or stay unsafe: %q %q", w1, w2)
	}

	mk := func(names ...string) []*mcp.Tool {
		var ts []*mcp.Tool
		for _, n := range names {
			ts = append(ts, &mcp.Tool{Name: n, InputSchema: map[string]any{"type": "object"}})
		}
		return ts
	}
	one := buildProxies(Service{Name: "voice"}, mk("a.b", "a_b", long1, long2, "x y", "x_y"))
	two := buildProxies(Service{Name: "voice"}, mk("x_y", "x y", long2, long1, "a_b", "a.b"))
	if len(one) != 6 || len(two) != 6 {
		t.Fatalf("lost tools: %d %d", len(one), len(two))
	}
	seen := map[string]bool{}
	for i := range one {
		if one[i].Name != two[i].Name || one[i].Upstream != two[i].Upstream {
			t.Errorf("result depends on input order at %d: %v vs %v", i, one[i].Name, two[i].Name)
		}
		if seen[one[i].Name] || len(one[i].Name) > maxToolName {
			t.Errorf("duplicate or long name %q", one[i].Name)
		}
		seen[one[i].Name] = true
	}
}

func TestRESTDescribeAndCall(t *testing.T) {
	rest := newFakeREST(t, "")
	env := newEnv(t, Service{Name: "api", Kind: KindOpenAPI, URL: rest.URL, Description: "voice REST API"})
	if !env.hasTool("service_describe") || !env.hasTool("service_call") {
		t.Fatalf("tools: %v", env.toolNames())
	}
	if env.hasTool("openai_chat") {
		t.Error("openai tools published without an openai service")
	}

	// Paging and search.
	_, text := env.call("service_describe", map[string]any{"service": "api", "limit": 3})
	m := decodeText(t, text)
	if m["total_operations"].(float64) != 10 || m["matched"].(float64) != 10 || m["next_offset"].(float64) != 3 {
		t.Errorf("paging: %v", m)
	}
	if ops := m["operations"].([]any); len(ops) != 3 {
		t.Errorf("limit ignored: %d", len(ops))
	}
	if _, ok := m["tags"]; !ok {
		t.Error("tag overview missing for an unfiltered listing")
	}
	_, text = env.call("service_describe", map[string]any{"service": "api", "query": "profile"})
	m = decodeText(t, text)
	ops := m["operations"].([]any)
	if len(ops) != 4 {
		t.Fatalf("query profile matched %d ops: %s", len(ops), text)
	}
	if !strings.Contains(text, "profile_id") || !strings.Contains(text, `"name?"`) && !strings.Contains(text, `"name"`) {
		t.Errorf("detail missing parameters/body shape: %s", text)
	}
	if !strings.Contains(text, "string (en|de)") {
		t.Errorf("schema $ref not resolved into the body shape: %s", text)
	}
	_, text = env.call("service_describe", map[string]any{"service": "api", "tag": "tts"})
	if m = decodeText(t, text); m["matched"].(float64) != 2 {
		t.Errorf("tag filter: %s", text)
	}
	_, text = env.call("service_describe", map[string]any{"service": "api", "query": "generate"})
	if !strings.Contains(text, "binary file") {
		t.Errorf("multipart file field not shown: %s", text)
	}

	// Plain JSON call with query.
	res, text := env.call("service_call", map[string]any{"service": "api", "method": "GET", "path": "/profiles", "query": map[string]any{"q": "ada lovelace"}})
	m = decodeText(t, text)
	if res.IsError || m["status"].(float64) != 200 || !strings.Contains(text, `"q": "ada lovelace"`) {
		t.Errorf("GET /profiles: %v %s", res.IsError, text)
	}
	// JSON body.
	res, text = env.call("service_call", map[string]any{"service": "api", "method": "POST", "path": "/profiles", "body": map[string]any{"name": "Zed"}})
	if res.IsError || !strings.Contains(text, `"created": true`) || !strings.Contains(text, `"Zed"`) {
		t.Errorf("POST /profiles: %s", text)
	}
	// HTTP errors are errors but carry the body.
	res, text = env.call("service_call", map[string]any{"service": "api", "method": "GET", "path": "/profiles/x/missing"})
	if !res.IsError || !strings.Contains(text, "no such profile") {
		t.Errorf("404: %v %s", res.IsError, text)
	}

	// Multipart upload and binary reply -> artifact.
	env.write("ws/job1/ref.wav", wavBytes(100))
	res, text = env.call("service_call", map[string]any{
		"service": "api", "method": "POST", "path": "/generate",
		"form": map[string]any{"text": "hello", "speed": 1.2}, "files": map[string]any{"ref_audio": "ws/job1/ref.wav"},
	})
	m = decodeText(t, text)
	if res.IsError {
		t.Fatalf("generate: %s", text)
	}
	art, _ := m["artifact"].(map[string]any)
	ref, _ := art["ref"].(string)
	if !strings.HasPrefix(ref, "artifacts/api/") || !strings.HasSuffix(ref, ".wav") || art["mime"] != "audio/wav" {
		t.Fatalf("binary reply not captured: %s", text)
	}
	abs, _ := files.Resolve(env.paths, files.Ref(ref))
	if got, err := os.ReadFile(abs); err != nil || len(got) != len(wavBytes(3000)) {
		t.Errorf("artifact content: %v %d", err, len(got))
	}
	rest.mu.Lock()
	uploaded := len(rest.got)
	rest.mu.Unlock()
	if uploaded != len(wavBytes(100)) {
		t.Errorf("multipart file part had %d bytes", uploaded)
	}
	if strings.Contains(text, "RIFF") {
		t.Error("binary content inline")
	}

	// Large JSON is truncated with a marker and size.
	_, text = env.call("service_call", map[string]any{"service": "api", "method": "GET", "path": "/big"})
	m = decodeText(t, text)
	if m["truncated"] != true || m["bytes"].(float64) < 300000 || len(m["text"].(string)) > maxInlineBytes {
		t.Errorf("big JSON: truncated=%v bytes=%v", m["truncated"], m["bytes"])
	}
	// base64 audio inside JSON becomes a ref.
	_, text = env.call("service_call", map[string]any{"service": "api", "method": "GET", "path": "/json-audio"})
	if strings.Contains(text, "UklGR") || !strings.Contains(text, "artifacts/api/") {
		t.Errorf("json audio: %.300s", text)
	}
}

func TestRESTRefusals(t *testing.T) {
	rest := newFakeREST(t, "")
	env := newEnv(t, Service{Name: "api", Kind: KindOpenAPI, URL: rest.URL,
		Deny: []string{"DELETE /profiles/{id}", "* /admin/**"}, Allow: []string{"GET /**", "POST /profiles", "DELETE /profiles/*"}})
	before := len(rest.hitLog())
	cases := []map[string]any{
		{"method": "GET", "path": "/../etc/passwd"},
		{"method": "GET", "path": "/profiles/%2e%2e/x"},
		{"method": "GET", "path": "http://evil.example/x"},
		{"method": "GET", "path": "//evil.example/x"},
		{"method": "GET", "path": "/x?y=1"},
		{"method": "GET", "path": "profiles"},
		{"method": "TRACE", "path": "/health"},
		{"method": "DELETE", "path": "/profiles/a1"},     // denied
		{"method": "DELETE", "path": "/profiles/%61%31"}, // denied after decoding
		{"method": "POST", "path": "/generate"},          // not in allow
		{"method": "GET", "path": "/admin/users"},        // denied by pattern
		{"method": "GET", "path": "/health", "headers": map[string]any{"Authorization": "x"}},
		{"method": "GET", "path": "/health", "headers": map[string]any{"Host": "evil"}},
		{"method": "GET", "path": "/health", "body": map[string]any{"a": 1}},
		{"method": "POST", "path": "/profiles", "body": map[string]any{"a": 1}, "files": map[string]any{"f": "ws/x/y"}},
		{"method": "POST", "path": "/profiles", "files": map[string]any{"f": "ws/../../x"}},
		{"method": "POST", "path": "/profiles", "files": map[string]any{"f": "/etc/passwd"}},
	}
	for _, c := range cases {
		c["service"] = "api"
		res, text := env.call("service_call", c)
		if !res.IsError {
			t.Errorf("accepted %v: %s", c, text)
		}
		if _, err := env.approval("service_call", c); err == nil {
			t.Errorf("approval built for %v", c)
		}
	}
	if len(rest.hitLog()) != before {
		t.Errorf("refused requests reached the service: %v", rest.hitLog()[before:])
	}
	// Allowed ones still work.
	if res, text := env.call("service_call", map[string]any{"service": "api", "method": "GET", "path": "/health"}); res.IsError {
		t.Errorf("GET /health: %s", text)
	}
}

func TestOpenAI(t *testing.T) {
	oa := newFakeOpenAI(t, "k-123")
	env := newEnv(t, Service{Name: "llm", Kind: KindOpenAI, URL: oa.URL + "/v1/", Auth: &Auth{Header: "Authorization", Value: "Bearer k-123"}})
	if !env.hasTool("openai_models") || !env.hasTool("openai_chat") {
		t.Fatalf("tools: %v", env.toolNames())
	}
	_, text := env.call("openai_models", map[string]any{"service": "llm"})
	if !strings.Contains(text, "llama3") || !strings.Contains(text, "qwen") {
		t.Errorf("models: %s", text)
	}
	res, text := env.call("openai_chat", map[string]any{"service": "llm", "model": "llama3", "max_tokens": 50, "temperature": 0.2,
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	m := decodeText(t, text)
	if res.IsError || m["text"] != "hello there" || m["usage"].(map[string]any)["total_tokens"].(float64) != 7 {
		t.Errorf("chat: %s", text)
	}
	oa.mu.Lock()
	req := oa.lastReq
	oa.mu.Unlock()
	if req["stream"] != false || req["model"] != "llama3" || req["max_tokens"].(float64) != 50 {
		t.Errorf("upstream request: %v", req)
	}
	res, text = env.call("openai_chat", map[string]any{"service": "llm", "model": "boom", "messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if !res.IsError || !strings.Contains(text, "model not found") {
		t.Errorf("upstream error: %v %s", res.IsError, text)
	}
	res, _ = env.call("openai_chat", map[string]any{"service": "llm", "model": "x", "messages": []any{map[string]any{"role": "tool", "content": "hi"}}})
	if !res.IsError {
		t.Error("bad role accepted")
	}
	// Generic tools refuse the wrong kind.
	if res, _ := env.call("service_call", map[string]any{"service": "llm", "method": "GET", "path": "/"}); !res.IsError {
		t.Error("service_call accepted an openai service")
	}
}

func detailsText(ap provider.Approval) string {
	var sb strings.Builder
	sb.WriteString(ap.Title + "\n")
	for _, d := range ap.Details {
		sb.WriteString(d.Label + ": " + d.Value + "\n")
	}
	return sb.String()
}

func TestApprovalMCP(t *testing.T) {
	f := newFakeMCP(t)
	env := newEnv(t, Service{Name: "voice", Kind: KindMCP, URL: f.endpoint()})
	env.write("ws/j/a.wav", wavBytes(10))

	ap, err := env.approval("voice__generate_speech", map[string]any{"text": "hello", "speed": 1.0})
	if err != nil {
		t.Fatal(err)
	}
	if ap.Title != "Call voice: generate_speech" || ap.Deferred || ap.Auto != "" {
		t.Errorf("approval: %+v", ap)
	}
	if len(ap.Scopes) != 2 || ap.Scopes[0].Key != "service:voice:generate_speech" || ap.Scopes[0].Broad ||
		ap.Scopes[1].Key != "service:voice:*" || !ap.Scopes[1].Broad {
		t.Errorf("scopes: %+v", ap.Scopes)
	}
	text := detailsText(ap)
	if !strings.Contains(text, "hello") || !strings.Contains(text, "voice") || !strings.Contains(text, "omp on pi") {
		t.Errorf("details: %s", text)
	}
	same, _ := env.approval("voice__generate_speech", map[string]any{"speed": 1.0, "text": "hello"}) // key order differs
	if same.Exact != ap.Exact || len(ap.Exact) != 64 {
		t.Errorf("Exact unstable: %s vs %s", same.Exact, ap.Exact)
	}
	for name, args := range map[string]map[string]any{
		"text":  {"text": "hellp", "speed": 1.0},
		"speed": {"text": "hello", "speed": 1.5},
		"extra": {"text": "hello", "speed": 1.0, "x": 1},
	} {
		other, _ := env.approval("voice__generate_speech", args)
		if other.Exact == ap.Exact {
			t.Errorf("Exact ignores %s", name)
		}
	}
	other, _ := env.approval("voice__check_health", map[string]any{})
	if other.Exact == ap.Exact {
		t.Error("Exact ignores the tool")
	}

	// A file argument contributes its content hash.
	a1, _ := env.approval("voice__transcribe", map[string]any{"audio_path": "ws/j/a.wav"})
	env.write("ws/j/a.wav", wavBytes(11))
	a2, _ := env.approval("voice__transcribe", map[string]any{"audio_path": "ws/j/a.wav"})
	if a1.Exact == a2.Exact {
		t.Error("Exact does not cover file content")
	}
	// A path the service may create is allowed to be absent, and its identity changes once it exists.
	absent, err := env.approval("voice__transcribe", map[string]any{"audio_path": "ws/j/new.wav"})
	if err != nil {
		t.Fatalf("approval for an absent output path: %v", err)
	}
	env.write("ws/j/new.wav", wavBytes(5))
	if present, _ := env.approval("voice__transcribe", map[string]any{"audio_path": "ws/j/new.wav"}); present.Exact == absent.Exact {
		t.Error("absent and present files share an identity")
	}
	// Credential-looking and base64 arguments are never shown.
	ap, _ = env.approval("voice__transcribe", map[string]any{"audio_base64": strings.Repeat("QUJD", 500), "api_key": "hunter2-secret"})
	if text := detailsText(ap); strings.Contains(text, "hunter2") || strings.Contains(text, "QUJDQUJD") {
		t.Errorf("sensitive argument shown: %s", text)
	}
}

func TestAutoOnlyWhenConfigured(t *testing.T) {
	f := newFakeMCP(t)
	rest := newFakeREST(t, "")
	mk := func(auto Auto) *testEnv {
		return newEnv(t,
			Service{Name: "voice", Kind: KindMCP, URL: f.endpoint(), Auto: auto},
			Service{Name: "api", Kind: KindOpenAPI, URL: rest.URL, Auto: auto})
	}
	autoOf := func(env *testEnv, tool string, args any) string {
		ap, err := env.approval(tool, args)
		if err != nil {
			t.Fatalf("%s: %v", tool, err)
		}
		return ap.Auto
	}
	get := map[string]any{"service": "api", "method": "GET", "path": "/profiles/a1"}
	post := map[string]any{"service": "api", "method": "POST", "path": "/generate"}
	del := map[string]any{"service": "api", "method": "DELETE", "path": "/profiles/a1"}

	none := mk(Auto{})
	for tool, args := range map[string]any{"voice__check_health": map[string]any{}, "voice__generate_speech": map[string]any{"text": "x"}, "service_call": get, "service_describe": map[string]any{"service": "api"}} {
		if a := autoOf(none, tool, args); a != "" {
			t.Errorf("default config auto-approved %s: %s", tool, a)
		}
	}

	read := mk(Auto{Read: true})
	if a := autoOf(read, "voice__check_health", map[string]any{}); !strings.Contains(a, "voice auto.read") {
		t.Errorf("readOnly MCP tool: %q", a)
	}
	if a := autoOf(read, "service_call", get); !strings.Contains(a, "api auto.read") {
		t.Errorf("GET: %q", a)
	}
	if a := autoOf(read, "service_describe", map[string]any{"service": "api"}); a == "" {
		t.Error("describe is read-only")
	}
	for tool, args := range map[string]any{"voice__generate_speech": map[string]any{"text": "x"}, "service_call#post": post, "service_call#del": del} {
		tool, _, _ = strings.Cut(tool, "#")
		if a := autoOf(read, tool, args); a != "" {
			t.Errorf("auto.read approved mutating %s: %q", tool, a)
		}
	}

	listed := mk(Auto{Tools: []string{"generate_speech", "POST /generate", "DELETE /profiles/{id}"}})
	if a := autoOf(listed, "voice__generate_speech", map[string]any{"text": "x"}); !strings.Contains(a, `auto.tools "generate_speech"`) {
		t.Errorf("listed MCP tool: %q", a)
	}
	if a := autoOf(listed, "service_call", post); !strings.Contains(a, `"POST /generate"`) {
		t.Errorf("listed POST: %q", a)
	}
	if a := autoOf(listed, "service_call", del); a == "" {
		t.Error("template entry did not match a concrete path")
	}
	if a := autoOf(listed, "voice__check_health", map[string]any{}); a != "" {
		t.Errorf("unlisted tool auto-approved: %q", a)
	}
	if a := autoOf(listed, "service_call", map[string]any{"service": "api", "method": "POST", "path": "/profiles", "body": map[string]any{"name": "x"}}); a != "" {
		t.Errorf("unlisted POST auto-approved: %q", a)
	}
}

func TestApprovalREST(t *testing.T) {
	rest := newFakeREST(t, "")
	env := newEnv(t, Service{Name: "api", Kind: KindOpenAPI, URL: rest.URL})
	env.write("ws/j/in.wav", wavBytes(10))
	call := func(m map[string]any) provider.Approval {
		t.Helper()
		m["service"] = "api"
		ap, err := env.approval("service_call", m)
		if err != nil {
			t.Fatal(err)
		}
		return ap
	}
	base := call(map[string]any{"method": "POST", "path": "/profiles", "body": map[string]any{"name": "a", "tags": []any{"x"}}})
	if base.Title != "Call api: POST /profiles" || base.Scopes[0].Key != "service:api:POST /profiles" || base.Scopes[1].Key != "service:api:*" {
		t.Errorf("approval: %+v", base)
	}
	if again := call(map[string]any{"method": "POST", "path": "/profiles", "body": map[string]any{"tags": []any{"x"}, "name": "a"}}); again.Exact != base.Exact {
		t.Error("Exact depends on key order")
	}
	diffs := map[string]map[string]any{
		"body":    {"method": "POST", "path": "/profiles", "body": map[string]any{"name": "b", "tags": []any{"x"}}},
		"query":   {"method": "POST", "path": "/profiles", "query": map[string]any{"a": 1}, "body": map[string]any{"name": "a", "tags": []any{"x"}}},
		"method":  {"method": "PUT", "path": "/profiles", "body": map[string]any{"name": "a", "tags": []any{"x"}}},
		"path":    {"method": "POST", "path": "/profiles/x", "body": map[string]any{"name": "a", "tags": []any{"x"}}},
		"headers": {"method": "POST", "path": "/profiles", "headers": map[string]any{"X-A": "1"}, "body": map[string]any{"name": "a", "tags": []any{"x"}}},
	}
	for name, a := range diffs {
		if call(a).Exact == base.Exact {
			t.Errorf("Exact ignores %s", name)
		}
	}
	// Path templates in scopes come from the OpenAPI document.
	ap := call(map[string]any{"method": "GET", "path": "/profiles/abc"})
	if ap.Scopes[0].Key != "service:api:GET /profiles/{profile_id}" {
		t.Errorf("scope = %q", ap.Scopes[0].Key)
	}
	// Uploaded content is covered.
	up := map[string]any{"method": "POST", "path": "/generate", "files": map[string]any{"ref_audio": "ws/j/in.wav"}, "form": map[string]any{"text": "t"}}
	u1 := call(up)
	env.write("ws/j/in.wav", wavBytes(12))
	if call(up).Exact == u1.Exact {
		t.Error("Exact does not cover uploaded file content")
	}
	if !strings.Contains(detailsText(u1), "ws/j/in.wav") {
		t.Errorf("file ref not shown: %s", detailsText(u1))
	}
}

func TestSecretsNeverLeave(t *testing.T) {
	f := newFakeMCP(t)
	rest := newFakeREST(t, testSecret)
	oa := newFakeOpenAI(t, testSecret)
	bearer := &Auth{Header: "Authorization", Value: "Bearer " + testSecret}
	// MCP service reached with a credential, plus a value_file credential for the OpenAI one.
	env := newEnv(t,
		Service{Name: "voice", Kind: KindMCP, URL: f.endpoint(), Auth: bearer},
		Service{Name: "api", Kind: KindOpenAPI, URL: rest.URL, Auth: bearer},
		Service{Name: "llm", Kind: KindOpenAI, URL: oa.URL, Auth: &Auth{Header: "Authorization", ValueFile: "key.txt"}},
	)
	// The value_file service is down until the file exists.
	env.write("key.txt", []byte("Bearer "+testSecret+"\n"))
	waitFor(t, "llm up", func() bool { return env.hasTool("openai_chat") })

	var outputs []string
	add := func(s string) { outputs = append(outputs, s) }
	add(mustJSONText(env.p.Tools()))
	_, text := env.call("catalogue", map[string]any{})
	add(text)
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"voice__leak", map[string]any{}},
		{"voice__check_health", map[string]any{}},
		{"service_call", map[string]any{"service": "api", "method": "GET", "path": "/whoami"}},
		{"service_call", map[string]any{"service": "api", "method": "GET", "path": "/health"}},
		{"service_describe", map[string]any{"service": "api"}},
		{"openai_models", map[string]any{"service": "llm"}},
		{"openai_chat", map[string]any{"service": "llm", "model": "boom", "messages": []any{map[string]any{"role": "user", "content": "x"}}}},
	} {
		_, text := env.call(c.tool, c.args)
		add(text)
		if ap, err := env.approval(c.tool, c.args); err == nil {
			add(detailsText(ap) + mustJSONText(ap))
		}
	}
	for i, out := range outputs {
		if strings.Contains(out, testSecret) {
			t.Errorf("output %d contains the secret:\n%s", i, out)
		}
	}
	if logs := env.logs.String(); strings.Contains(logs, testSecret) {
		t.Errorf("log contains the secret:\n%s", logs)
	}
	// The credential really was sent: whoami proves redaction happened rather than omission.
	_, text = env.call("service_call", map[string]any{"service": "api", "method": "GET", "path": "/whoami"})
	if !strings.Contains(text, "[redacted]") {
		t.Errorf("expected the echoed credential to be redacted: %s", text)
	}
	if _, text := env.call("voice__leak", map[string]any{}); !strings.Contains(text, "[redacted]") {
		t.Errorf("MCP echo not redacted: %s", text)
	}
}

func mustJSONText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestSecretInProbeErrors(t *testing.T) {
	rest := newFakeREST(t, "right-token-123456")
	env := newEnv(t, Service{Name: "api", Kind: KindOpenAPI, URL: rest.URL, Auth: &Auth{Header: "Authorization", Value: "Bearer " + testSecret}})
	_, text := env.call("catalogue", map[string]any{})
	m := decodeText(t, text)
	svc := m["services"].([]any)[0].(map[string]any)
	if svc["up"] != false || !strings.Contains(svc["last_error"].(string), "401") {
		t.Errorf("a service rejecting our credential should read as down with its HTTP error: %v", svc)
	}
	if strings.Contains(text, testSecret) || strings.Contains(env.logs.String(), testSecret) {
		t.Error("secret leaked through probe errors")
	}
}

func TestCatalogueContent(t *testing.T) {
	f := newFakeMCP(t)
	rest := newFakeREST(t, "")
	oa := newFakeOpenAI(t, "")
	env := newEnv(t,
		Service{Name: "voice", Kind: KindMCP, URL: f.endpoint(), Description: "voice generation", Tags: []string{"tts", "gpu"}},
		Service{Name: "api", Kind: KindOpenAPI, URL: rest.URL, Tags: []string{"tts"}},
		Service{Name: "llm", Kind: KindOpenAI, URL: oa.URL},
		Service{Name: "off", Kind: KindHTTP, URL: rest.URL, Enabled: new(false)},
	)
	_, text := env.call("catalogue", map[string]any{})
	m := decodeText(t, text)
	svcs := m["services"].([]any)
	if len(svcs) != 3 {
		t.Fatalf("services: %d\n%s", len(svcs), text)
	}
	if strings.Contains(text, "127.0.0.1") || strings.Contains(text, "http://") {
		t.Errorf("catalogue exposes upstream URLs: %s", text)
	}
	by := map[string]map[string]any{}
	for _, s := range svcs {
		sm := s.(map[string]any)
		by[sm["name"].(string)] = sm
	}
	v := by["voice"]
	if v["up"] != true || v["description"] != "voice generation" || v["facts"].(map[string]any)["upstream"] != "VoiceStudio 1.29.0" {
		t.Errorf("voice: %v", v)
	}
	if tools := v["use"].(map[string]any)["tools"].([]any); !slices.Contains(tools, any("voice__check_health")) {
		t.Errorf("voice use: %v", v["use"])
	}
	if by["api"]["facts"].(map[string]any)["operations"].(float64) != 10 || !strings.Contains(by["api"]["facts"].(map[string]any)["api"].(string), "Fake Voice API") {
		t.Errorf("api facts: %v", by["api"]["facts"])
	}
	if via := by["api"]["use"].(map[string]any)["via"].([]any); len(via) != 2 {
		t.Errorf("api use: %v", by["api"]["use"])
	}
	if via := by["llm"]["use"].(map[string]any)["via"].([]any); !slices.Contains(via, any("openai_chat")) {
		t.Errorf("llm use: %v", by["llm"]["use"])
	}
	_, text = env.call("catalogue", map[string]any{"tag": "tts"})
	if len(decodeText(t, text)["services"].([]any)) != 2 {
		t.Errorf("tag filter: %s", text)
	}
	_, text = env.call("catalogue", map[string]any{"kind": "openai"})
	if len(decodeText(t, text)["services"].([]any)) != 1 {
		t.Errorf("kind filter: %s", text)
	}
}

func TestConfigReloadAndNotifier(t *testing.T) {
	f := newFakeMCP(t)
	rest := newFakeREST(t, "")
	env := newEnv(t) // no services file yet
	if got := env.toolNames(); len(got) != 1 || got[0] != "catalogue" {
		t.Fatalf("empty config tools: %v", got)
	}
	_, text := env.call("catalogue", map[string]any{})
	if !strings.Contains(text, "no services are registered") {
		t.Errorf("empty catalogue: %s", text)
	}

	// Appears via mtime reload.
	base := env.fired.Load()
	writeServices(t, env.paths, Service{Name: "voice", Kind: KindMCP, URL: f.endpoint()})
	waitFor(t, "mcp tools to appear", func() bool { return env.hasTool("voice__check_health") })
	waitFor(t, "notifier after add", func() bool { return env.fired.Load() > base })

	// Edits apply: description and a second service.
	writeServices(t, env.paths,
		Service{Name: "voice", Kind: KindMCP, URL: f.endpoint(), Description: "edited"},
		Service{Name: "api", Kind: KindOpenAPI, URL: rest.URL})
	waitFor(t, "service_call to appear", func() bool { return env.hasTool("service_call") })
	_, text = env.call("catalogue", map[string]any{})
	if !strings.Contains(text, "edited") {
		t.Errorf("description edit not applied: %s", text)
	}

	// A broken file keeps the last good config and says so.
	if err := os.WriteFile(env.paths.ServicesFile(), []byte(`{"services": [`), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "config error to show", func() bool {
		_, text := env.call("catalogue", map[string]any{})
		return strings.Contains(text, "config_errors")
	})
	if !env.hasTool("voice__check_health") || !env.hasTool("service_call") {
		t.Error("a half-written file dropped working services")
	}

	// Invalid entries are skipped, valid ones kept.
	writeServices(t, env.paths,
		Service{Name: "Bad Name", Kind: KindMCP, URL: f.endpoint()},
		Service{Name: "api", Kind: KindOpenAPI, URL: rest.URL})
	waitFor(t, "invalid entry reported", func() bool {
		_, text := env.call("catalogue", map[string]any{})
		return strings.Contains(text, "invalid service name") && !env.hasTool("voice__check_health")
	})
	if !env.hasTool("service_call") {
		t.Error("valid entry lost next to an invalid one")
	}

	// Disable and delete.
	base = env.fired.Load()
	writeServices(t, env.paths, Service{Name: "api", Kind: KindOpenAPI, URL: rest.URL, Enabled: new(false)})
	waitFor(t, "service_call to vanish", func() bool { return !env.hasTool("service_call") })
	waitFor(t, "notifier after disable", func() bool { return env.fired.Load() > base })
	if err := os.Remove(env.paths.ServicesFile()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "empty after delete", func() bool { return len(env.toolNames()) == 1 })
}

func TestHealthTransitionsFireNotifier(t *testing.T) {
	f := newFakeMCP(t)
	rest := newFakeREST(t, "")
	env := newEnv(t,
		Service{Name: "voice", Kind: KindMCP, URL: f.endpoint()},
		Service{Name: "api", Kind: KindOpenAPI, URL: rest.URL})
	if !env.hasTool("voice__check_health") || !env.hasTool("service_call") {
		t.Fatalf("not up at start: %v", env.toolNames())
	}

	base := env.fired.Load()
	f.down.Store(true)
	waitFor(t, "mcp tools to disappear", func() bool { return !env.hasTool("voice__check_health") })
	waitFor(t, "notifier on mcp down", func() bool { return env.fired.Load() > base })
	_, text := env.call("catalogue", map[string]any{})
	for _, s := range decodeText(t, text)["services"].([]any) {
		sm := s.(map[string]any)
		if sm["name"] == "voice" && (sm["up"] != false || sm["last_error"] == "" || sm["last_error"] == nil) {
			t.Errorf("down service reported as %v", sm)
		}
	}

	base = env.fired.Load()
	f.down.Store(false)
	waitFor(t, "mcp tools to return", func() bool { return env.hasTool("voice__check_health") })
	waitFor(t, "notifier on mcp up", func() bool { return env.fired.Load() > base })

	base = env.fired.Load()
	rest.down.Store(true)
	waitFor(t, "service_call to disappear", func() bool { return !env.hasTool("service_call") })
	waitFor(t, "notifier on rest down", func() bool { return env.fired.Load() > base })
	rest.down.Store(false)
	waitFor(t, "service_call to return", func() bool { return env.hasTool("service_call") })

	// Upstream tool list change is picked up.
	base = env.fired.Load()
	f.mu.Lock()
	f.tools = append(f.tools, map[string]any{"name": "new_tool", "description": "added later", "inputSchema": map[string]any{"type": "object"}})
	f.mu.Unlock()
	waitFor(t, "new upstream tool", func() bool { return env.hasTool("voice__new_tool") })
	waitFor(t, "notifier on tool change", func() bool { return env.fired.Load() > base })
}

func TestNotifierIsDebounced(t *testing.T) {
	f := newFakeMCP(t)
	env := newEnvWith(t, func(o *Options) { o.Debounce = 300 * time.Millisecond }, Service{Name: "voice", Kind: KindMCP, URL: f.endpoint()})
	base := env.fired.Load()
	for range 5 {
		env.p.mu.Lock()
		env.p.sig = "stale"
		env.p.mu.Unlock()
		env.p.rebuild()
	}
	waitFor(t, "one callback", func() bool { return env.fired.Load() > base })
	if got := env.fired.Load() - base; got != 1 {
		t.Errorf("burst produced %d callbacks", got)
	}
}

func TestValidate(t *testing.T) {
	ok := Service{Name: "voice", Kind: KindMCP, URL: "http://127.0.0.1:3900/mcp/"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	mut := func(f func(*Service)) Service { s := ok; f(&s); return s }
	bad := map[string]Service{
		"uppercase name":    mut(func(s *Service) { s.Name = "Voice" }),
		"long name":         mut(func(s *Service) { s.Name = strings.Repeat("a", 25) }),
		"double underscore": mut(func(s *Service) { s.Name = "a__b" }),
		"kind":              mut(func(s *Service) { s.Kind = "grpc" }),
		"scheme":            mut(func(s *Service) { s.URL = "ftp://127.0.0.1/" }),
		"credentials":       mut(func(s *Service) { s.URL = "http://u:p@127.0.0.1/" }),
		"query":             mut(func(s *Service) { s.URL = "http://127.0.0.1/?key=1" }),
		"lan without opt":   mut(func(s *Service) { s.URL = "http://192.168.1.20:3900" }),
		"host without opt":  mut(func(s *Service) { s.URL = "http://desktop.local:3900" }),
		"public with opt":   mut(func(s *Service) { s.URL = "http://8.8.8.8/"; s.AllowLAN = true }),
		"auth no value":     mut(func(s *Service) { s.Auth = &Auth{Header: "X-Key"} }),
		"auth both":         mut(func(s *Service) { s.Auth = &Auth{Header: "X-Key", Value: "a", ValueFile: "b"} }),
		"auth reserved":     mut(func(s *Service) { s.Auth = &Auth{Header: "Host", Value: "a"} }),
		"auth newline":      mut(func(s *Service) { s.Auth = &Auth{Header: "X-Key", Value: "a\r\nX: b"} }),
		"health offsite":    mut(func(s *Service) { s.Health = "http://127.0.0.1:1/h" }),
		"health relative":   mut(func(s *Service) { s.Health = "health" }),
		"tag":               mut(func(s *Service) { s.Tags = []string{"bad tag"} }),
		"openapi on mcp":    mut(func(s *Service) { s.OpenAPI = "/x" }),
	}
	for name, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	good := []Service{
		mut(func(s *Service) { s.URL = "http://192.168.1.20:3900"; s.AllowLAN = true }),
		mut(func(s *Service) { s.URL = "http://desktop.local:3900"; s.AllowLAN = true }),
		mut(func(s *Service) { s.URL = "http://localhost:8080" }),
		mut(func(s *Service) { s.URL = "http://[::1]:8080" }),
		mut(func(s *Service) { s.Health = "/health"; s.Auth = &Auth{Header: "X-Api-Key", ValueFile: "k.txt"} }),
	}
	for i, s := range good {
		if err := s.Validate(); err != nil {
			t.Errorf("good[%d]: %v", i, err)
		}
	}
}

func TestDialPolicy(t *testing.T) {
	for _, c := range []struct {
		addr string
		lan  bool
		ok   bool
	}{
		{"127.0.0.1:80", false, true},
		{"[::1]:80", false, true},
		{"10.0.0.5:80", false, false},
		{"10.0.0.5:80", true, true},
		{"192.168.1.9:80", true, true},
		{"169.254.1.1:80", true, true},
		{"8.8.8.8:80", true, false},
		{"[2001:4860::1]:80", true, false},
	} {
		if err := ipAllowedAtDial(c.addr, c.lan); (err == nil) != c.ok {
			t.Errorf("%s lan=%v: err=%v", c.addr, c.lan, err)
		}
	}
}

func TestFileRoundTripAndPerms(t *testing.T) {
	paths := state.Paths{Root: t.TempDir()}
	writeServices(t, paths, Service{Name: "a", Kind: KindHTTP, URL: "http://127.0.0.1:1", Auth: &Auth{Header: "X-K", Value: "v"}})
	f, err := LoadFile(paths.ServicesFile())
	if err != nil || len(f.Services) != 1 || f.Services[0].Auth.Value != "v" {
		t.Fatalf("round trip: %v %+v", err, f)
	}
	if fi, err := os.Stat(paths.ServicesFile()); err != nil || fi.Mode().Perm()&0o077 != 0 && fi.Mode().Perm() != 0o666 {
		// Windows reports 0666 regardless; elsewhere group/other must have nothing.
		t.Errorf("services file mode %v", fi.Mode())
	}
	if _, err := parseFile([]byte(`{"services":[{"name":"a","kind":"http","url":"http://127.0.0.1","bogus":1}]}`)); err == nil {
		t.Error("unknown field accepted (typos would silently disable settings)")
	}
	if g, err := LoadFile(paths.ServicesFile() + ".none"); err != nil || len(g.Services) != 0 {
		t.Errorf("missing file: %v %v", err, g)
	}
}

func TestBlobDetection(t *testing.T) {
	sk := &sink{paths: state.Paths{Root: t.TempDir()}, source: "svc"}
	// Plain text that merely looks long is left alone.
	long := strings.Repeat("hello world ", 500)
	if out, changed, err := sk.scrubText(`{"x":"` + long + `"}`); err != nil || changed || !strings.Contains(out, "hello world") {
		t.Errorf("text mangled: %v %v", changed, err)
	}
	// data: URIs and bare base64 with a media signature are captured.
	b64 := encodeStd(wavBytes(2000))
	out, changed, err := sk.scrubText(`{"a":"data:audio/wav;base64,` + b64 + `","b":["` + b64 + `"],"n":12345678901234567890}`)
	if err != nil || !changed || len(sk.made) != 2 || strings.Contains(out, "UklGR") || !strings.Contains(out, "12345678901234567890") {
		t.Errorf("scrub: changed=%v err=%v made=%v out=%.200s", changed, err, sk.made, out)
	}
	if got := limitText(strings.Repeat("é", maxInlineText)); !strings.Contains(got, "[truncated") || !strings.HasPrefix(got, "é") {
		t.Error("limitText")
	}
}
