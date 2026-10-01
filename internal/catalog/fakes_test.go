package catalog

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/provider"
	"messh/internal/state"
)

const testSecret = "sk-test-SECRET-1234567890"

// wavBytes is a tiny but signature-valid RIFF/WAVE payload.
func wavBytes(n int) []byte {
	b := append([]byte("RIFF\x24\x00\x00\x00WAVEfmt "), make([]byte, n)...)
	return b
}

// fakeMCP imitates VoiceStudio's legacy streamable-HTTP flow: the session id
// comes back in a header on initialize, every reply is SSE-framed, there is
// no standalone GET stream.
type fakeMCP struct {
	*httptest.Server
	mu        sync.Mutex
	sessions  map[string]bool
	nextID    int
	inits     atomic.Int32
	calls     []string // tool names called
	lastArgs  map[string]json.RawMessage
	authSeen  []string
	down      atomic.Bool
	tools     []map[string]any
	wavLen    int
	echoAuth  bool
	loseReply atomic.Bool
}

func newFakeMCP(t *testing.T) *fakeMCP {
	f := &fakeMCP{sessions: map[string]bool{}, wavLen: 2000}
	f.tools = []map[string]any{
		{"name": "check_health", "description": "Report health.", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
			"annotations": map[string]any{"readOnlyHint": true}},
		{"name": "generate_speech", "description": "Speak text.", "inputSchema": map[string]any{"type": "object", "required": []string{"text"},
			"properties": map[string]any{"text": map[string]any{"type": "string"}, "speed": map[string]any{"type": "number"}}}},
		{"name": "transcribe", "description": "Transcribe audio.", "inputSchema": map[string]any{"type": "object",
			"properties": map[string]any{"audio_path": map[string]any{"type": "string", "description": "path to audio"}, "audio_base64": map[string]any{"type": "string"}}}},
		{"name": "make_blob", "description": "Returns audio content.", "inputSchema": map[string]any{"type": "object"}},
		{"name": "make_json_blob", "description": "Returns JSON with base64 audio.", "inputSchema": map[string]any{"type": "object"}},
		{"name": "leak", "description": "Echoes the auth header it saw.", "inputSchema": map[string]any{"type": "object"}},
	}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Server.Close)
	return f
}

func (f *fakeMCP) endpoint() string { return f.URL + "/mcp/" }

func (f *fakeMCP) serve(w http.ResponseWriter, r *http.Request) {
	if f.down.Load() {
		http.Error(w, "down", http.StatusServiceUnavailable)
		return
	}
	if r.URL.Path != "/mcp/" {
		http.NotFound(w, r)
		return
	}
	f.mu.Lock()
	f.authSeen = append(f.authSeen, r.Header.Get("Authorization"))
	f.mu.Unlock()
	switch r.Method {
	case http.MethodGet:
		http.Error(w, "no standalone stream", http.StatusMethodNotAllowed)
		return
	case http.MethodDelete:
		w.WriteHeader(http.StatusOK)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	reply := func(result any) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
	}
	if req.Method == "initialize" {
		f.mu.Lock()
		f.nextID++
		id := fmt.Sprintf("sess%d", f.nextID)
		f.sessions[id] = true
		f.mu.Unlock()
		f.inits.Add(1)
		w.Header().Set("Mcp-Session-Id", id)
		reply(map[string]any{
			"protocolVersion": "2025-06-18",
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "VoiceStudio", "version": "1.29.0"},
		})
		return
	}
	f.mu.Lock()
	ok := f.sessions[r.Header.Get("Mcp-Session-Id")]
	f.mu.Unlock()
	if !ok {
		http.Error(w, "session not found", http.StatusNotFound)
		return
	}
	switch req.Method {
	case "notifications/initialized", "notifications/cancelled":
		w.WriteHeader(http.StatusAccepted)
	case "tools/list":
		reply(map[string]any{"tools": f.tools})
	case "tools/call":
		var p struct {
			Name      string                     `json:"name"`
			Arguments map[string]json.RawMessage `json:"arguments"`
		}
		json.Unmarshal(req.Params, &p)
		f.mu.Lock()
		f.calls = append(f.calls, p.Name)
		f.lastArgs = p.Arguments
		f.mu.Unlock()
		if f.loseReply.Load() {
			hj, _ := w.(http.Hijacker)
			c, _, _ := hj.Hijack()
			c.Close()
			return
		}
		text := func(s string) any {
			return map[string]any{"content": []any{map[string]any{"type": "text", "text": s}}}
		}
		switch p.Name {
		case "check_health":
			reply(text(`{"result":"ok"}`))
		case "generate_speech":
			reply(text(`{"result":"generated"}`))
		case "transcribe":
			reply(text(`{"result":"heard"}`))
		case "make_blob":
			reply(map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "done"},
				map[string]any{"type": "audio", "mimeType": "audio/wav", "data": base64.StdEncoding.EncodeToString(wavBytes(f.wavLen))},
			}})
		case "make_json_blob":
			b, _ := json.Marshal(map[string]any{"result": map[string]any{"audio": base64.StdEncoding.EncodeToString(wavBytes(f.wavLen)), "seconds": 1.5}})
			reply(text(string(b)))
		case "leak":
			reply(text("auth header was: " + r.Header.Get("Authorization")))
		default:
			reply(map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "unknown tool"}}})
		}
	default:
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "event: message\ndata: %s\n\n", b)
	}
}

func (f *fakeMCP) killSessions() {
	f.mu.Lock()
	f.sessions = map[string]bool{}
	f.mu.Unlock()
}

func (f *fakeMCP) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeMCP) args() map[string]json.RawMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastArgs
}

// fakeREST serves an OpenAPI document and a few operations. When token is
// set every request needs "Authorization: Bearer <token>".
type fakeREST struct {
	*httptest.Server
	token string
	mu    sync.Mutex
	hits  []string
	down  atomic.Bool
	got   []byte // last multipart upload content
}

func newFakeREST(t *testing.T, token string) *fakeREST {
	f := &fakeREST{token: token}
	mux := http.NewServeMux()
	mux.HandleFunc("/openapi.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, fakeSpec)
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"ok","device":"cuda"}`)
	})
	mux.HandleFunc("/profiles", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			fmt.Fprintf(w, `{"profiles":[{"id":"a1","name":"Ada"}],"q":%q}`, r.URL.Query().Get("q"))
		case http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"created":true,"echo":%s}`, b)
		}
	})
	mux.HandleFunc("/profiles/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodDelete {
			io.WriteString(w, `{"deleted":true}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/missing") {
			w.WriteHeader(http.StatusNotFound)
			io.WriteString(w, `{"detail":"no such profile"}`)
			return
		}
		io.WriteString(w, `{"id":"`+strings.TrimPrefix(r.URL.Path, "/profiles/")+`"}`)
	})
	mux.HandleFunc("/generate", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if fl, _, err := r.FormFile("ref_audio"); err == nil {
			f.mu.Lock()
			f.got, _ = io.ReadAll(fl)
			f.mu.Unlock()
		}
		w.Header().Set("Content-Type", "audio/wav")
		w.Header().Set("Content-Disposition", `attachment; filename="speech.wav"`)
		w.Write(wavBytes(3000))
	})
	mux.HandleFunc("/big", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"items":[%s"x"]}`, strings.Repeat(`"0123456789",`, 30000))
	})
	mux.HandleFunc("/json-audio", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"audio":%q,"ok":true}`, base64.StdEncoding.EncodeToString(wavBytes(4000)))
	})
	mux.HandleFunc("/whoami", func(w http.ResponseWriter, r *http.Request) {
		// Echoes the credential the way a careless API might.
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"authorization":%q}`, r.Header.Get("Authorization"))
	})
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.down.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		f.mu.Lock()
		f.hits = append(f.hits, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		if f.token != "" && r.Header.Get("Authorization") != "Bearer "+f.token {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintf(w, `{"detail":"bad credentials (got %q)"}`, r.Header.Get("Authorization"))
			return
		}
		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.Server.Close)
	return f
}

func (f *fakeREST) hitLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.hits...)
}

const fakeSpec = `{"openapi":"3.1.0","info":{"title":"Fake Voice API","version":"0.5.6"},
"paths":{
 "/health":{"get":{"summary":"Health check","tags":["system"],"operationId":"health"}},
 "/profiles":{
   "get":{"summary":"List voice profiles","tags":["profiles"],"parameters":[{"name":"q","in":"query","schema":{"type":"string"},"description":"filter"}]},
   "post":{"summary":"Create a profile","tags":["profiles"],"requestBody":{"required":true,"content":{"application/json":{"schema":{"$ref":"#/components/schemas/NewProfile"}}}}}},
 "/profiles/{profile_id}":{
   "parameters":[{"name":"profile_id","in":"path","required":true,"schema":{"type":"string"}}],
   "get":{"summary":"Get a profile","tags":["profiles"]},
   "delete":{"summary":"Delete a profile","tags":["profiles"]}},
 "/generate":{"post":{"summary":"Generate speech","tags":["tts"],"requestBody":{"content":{"multipart/form-data":{"schema":{"type":"object","required":["text"],"properties":{"text":{"type":"string"},"ref_audio":{"type":"string","format":"binary"},"speed":{"type":"number"}}}}}}}},
 "/history":{"get":{"summary":"Generation history","tags":["tts"]}},
 "/big":{"get":{"summary":"Huge JSON","tags":["test"]}},
 "/json-audio":{"get":{"summary":"JSON with embedded audio","tags":["test"]}},
 "/whoami":{"get":{"summary":"Echo credentials","tags":["test"]}}
},
"components":{"schemas":{"NewProfile":{"type":"object","required":["name"],"properties":{"name":{"type":"string"},"tags":{"type":"array","items":{"type":"string"}},"lang":{"type":"string","enum":["en","de"]}}}}}}`

// fakeOpenAI implements /v1/models and /v1/chat/completions.
type fakeOpenAI struct {
	*httptest.Server
	mu      sync.Mutex
	lastReq map[string]any
}

func newFakeOpenAI(t *testing.T, token string) *fakeOpenAI {
	f := &fakeOpenAI{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if token != "" && r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprintf(w, `{"error":{"message":"invalid key %s"}}`, r.Header.Get("Authorization"))
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			io.WriteString(w, `{"object":"list","data":[{"id":"llama3","object":"model","owned_by":"local"},{"id":"qwen","object":"model","owned_by":"local"}]}`)
		case "/v1/chat/completions":
			var req map[string]any
			json.NewDecoder(r.Body).Decode(&req)
			f.mu.Lock()
			f.lastReq = req
			f.mu.Unlock()
			if req["model"] == "boom" {
				w.WriteHeader(http.StatusBadRequest)
				io.WriteString(w, `{"error":{"message":"model not found"}}`)
				return
			}
			io.WriteString(w, `{"model":"llama3","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hello there"}}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.Server.Close)
	return f
}

func encodeStd(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func writeServices(t *testing.T, paths state.Paths, svcs ...Service) {
	t.Helper()
	if err := SaveFile(paths.ServicesFile(), File{Services: svcs}); err != nil {
		t.Fatal(err)
	}
}

type testEnv struct {
	t     *testing.T
	paths state.Paths
	p     *Provider
	logs  *syncBuf
	fired atomic.Int32
}

func newEnv(t *testing.T, svcs ...Service) *testEnv { return newEnvWith(t, nil, svcs...) }

// newEnvWith is newEnv with a chance to adjust the provider options.
func newEnvWith(t *testing.T, mod func(*Options), svcs ...Service) *testEnv {
	t.Helper()
	env := &testEnv{t: t, paths: state.Paths{Root: t.TempDir()}, logs: &syncBuf{}}
	if svcs != nil {
		writeServices(t, env.paths, svcs...)
	}
	opts := Options{
		Paths: env.paths, Log: slog.New(slog.NewTextHandler(env.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		ReloadEvery: 30 * time.Millisecond, ProbeEvery: 60 * time.Millisecond, ProbeTimeout: 2 * time.Second,
		MaxBackoff: 120 * time.Millisecond, Debounce: 20 * time.Millisecond, InitialWait: 5 * time.Second,
	}
	if mod != nil {
		mod(&opts)
	}
	p, err := New(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	p.SetOnChange(func() { env.fired.Add(1) })
	env.p = p
	t.Cleanup(p.Close)
	return env
}

func (env *testEnv) toolNames() []string {
	var out []string
	for _, tl := range env.p.Tools() {
		out = append(out, tl.Def.Name)
	}
	return out
}

func (env *testEnv) hasTool(name string) bool {
	for _, n := range env.toolNames() {
		if n == name {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

var testCaller = provider.Caller{DeviceID: "dev1", DeviceName: "pi", Agent: "omp"}

// call runs a tool and returns the result text (all text content joined).
func (env *testEnv) call(tool string, args any) (*mcp.CallToolResult, string) {
	env.t.Helper()
	raw, _ := json.Marshal(args)
	res, err := env.p.Call(context.Background(), tool, raw, testCaller)
	if err != nil {
		env.t.Fatalf("%s: %v", tool, err)
	}
	var sb strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			sb.WriteString(tc.Text)
			sb.WriteString("\n")
		}
	}
	return res, sb.String()
}

func (env *testEnv) approval(tool string, args any) (provider.Approval, error) {
	raw, _ := json.Marshal(args)
	return env.p.Approval(context.Background(), tool, raw, testCaller)
}

func (env *testEnv) write(path string, data []byte) string {
	env.t.Helper()
	abs := filepath.Join(env.paths.Root, filepath.FromSlash(path))
	os.MkdirAll(filepath.Dir(abs), 0o700)
	if err := os.WriteFile(abs, data, 0o600); err != nil {
		env.t.Fatal(err)
	}
	return abs
}
