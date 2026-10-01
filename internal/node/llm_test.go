package node

import (
	"bufio"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"messh/internal/approval"
	"messh/internal/catalog"
	"messh/internal/state"
)

const llmHostKey = "Bearer sk-host-secret-123"

// fakeLLM is an OpenAI-compatible server that requires the host's API key
// and records what reached it.
type fakeLLM struct {
	srv     *httptest.Server
	release chan struct{} // a streaming reply sends its last event once closed

	mu    sync.Mutex
	chats []http.Header
}

func newFakeLLM(t *testing.T) *fakeLLM {
	f := &fakeLLM{release: make(chan struct{})}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != llmHostKey {
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, `{"error":{"message":"bad key","type":"auth"}}`)
			return
		}
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/models":
			io.WriteString(w, `{"object":"list","data":[{"id":"m1"},{"id":"m2"}]}`)
		case "POST /v1/chat/completions":
			var body struct {
				Model  string `json:"model"`
				Stream bool   `json:"stream"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			f.mu.Lock()
			f.chats = append(f.chats, r.Header.Clone())
			f.mu.Unlock()
			if !body.Stream {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"model":"`+body.Model+`","choices":[{"message":{"role":"assistant","content":"hi"}}]}`)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n")
			w.(http.Flusher).Flush()
			select {
			case <-f.release:
			case <-r.Context().Done():
				return
			}
			io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\ndata: [DONE]\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeLLM) chatHeaders() []http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]http.Header(nil), f.chats...)
}

// startLLMNode starts a node whose services.json lists services.
func startLLMNode(t *testing.T, name string, surface approval.Surface, services ...catalog.Service) *Node {
	t.Helper()
	paths := state.Paths{Root: t.TempDir()}
	if len(services) > 0 {
		if err := catalog.SaveFile(paths.ServicesFile(), catalog.File{Services: services}); err != nil {
			t.Fatal(err)
		}
	}
	n, err := Start(t.Context(), Options{
		Paths: paths, Name: name, MeshAddr: "127.0.0.1:0", LocalAddr: "127.0.0.1:0",
		Logger: slog.New(slog.DiscardHandler), ApprovalSurface: surface,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	return n
}

type llmError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

func llmDo(t *testing.T, method, url string, hdr map[string]string, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp, string(data)
}

func wantLLMError(t *testing.T, what string, resp *http.Response, body string, status int, typ string) llmError {
	t.Helper()
	var e llmError
	if resp.StatusCode != status || json.Unmarshal([]byte(body), &e) != nil || e.Error.Type != typ || e.Error.Message == "" {
		t.Fatalf("%s: HTTP %d %s, want %d with an OpenAI-style %q error", what, resp.StatusCode, body, status, typ)
	}
	return e
}

func TestLLMProxyThroughTheMesh(t *testing.T) {
	up := newFakeLLM(t)
	surface := &scriptSurface{}
	desktop := startLLMNode(t, "desktop", surface, catalog.Service{
		Name: "unsloth", Kind: catalog.KindOpenAI, URL: up.srv.URL + "/v1", // a base that already ends in /v1
		Auth: &catalog.Auth{Header: "Authorization", Value: llmHostKey},
	})
	raspi := startLLMNode(t, "raspi", &scriptSurface{})
	pair(t, desktop, raspi)
	tok, err := raspi.paths.AddAgent("omp")
	if err != nil {
		t.Fatal(err)
	}
	base := "http://" + raspi.LocalAddr() + "/llm/desktop/unsloth/v1"
	bearer := map[string]string{"Authorization": "Bearer " + tok}
	chat := func(model, text string, stream bool) string {
		b, _ := json.Marshal(map[string]any{"model": model, "stream": stream,
			"messages": []map[string]string{{"role": "user", "content": text}}})
		return string(b)
	}

	// A bad token never reaches the host.
	resp, body := llmDo(t, "POST", base+"/chat/completions", map[string]string{"Authorization": "Bearer nope"}, chat("m1", "hi", false))
	wantLLMError(t, "bad token", resp, body, http.StatusUnauthorized, "authentication_error")

	// First request: the person at the desktop is asked and picks "always
	// allow this model". The upstream sees the host's key, never the token.
	surface.answer(approval.Answer{Allow: true, Always: true, Scope: 1})
	resp, body = llmDo(t, "POST", base+"/chat/completions", bearer, chat("m1", "first prompt", false))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"content":"hi"`) {
		t.Fatalf("chat: HTTP %d %s", resp.StatusCode, body)
	}
	prompts := surface.shown()
	if len(prompts) != 1 {
		t.Fatalf("%d prompts, want 1", len(prompts))
	}
	p := prompts[0]
	details := map[string]string{}
	for _, d := range p.Details {
		details[d.Label] = d.Value
	}
	if p.Title != "omp on raspi wants to use the unsloth language model" || p.Caller.DeviceID != raspi.ID() || p.Caller.Agent != "omp" ||
		details["Model"] != "m1" || details["Streaming"] != "no" || details["Endpoint"] != "POST /v1/chat/completions" ||
		len(p.Scopes) != 4 || p.Scopes[1].Key != "service:unsloth:llm:m1" {
		t.Fatalf("prompt = %+v", p)
	}
	if strings.Contains(strings.Join(slicesOfDetails(p), " "), "first prompt") {
		t.Fatal("the prompt text was shown in the approval")
	}
	hdrs := up.chatHeaders()
	if len(hdrs) != 1 || hdrs[0].Get("Authorization") != llmHostKey {
		t.Fatalf("upstream headers %v", hdrs)
	}
	for k, vs := range hdrs[0] {
		for _, v := range vs {
			if strings.Contains(v, tok) || strings.HasPrefix(strings.ToLower(k), "messh-") {
				t.Fatalf("leaked to the upstream: %s: %s", k, v)
			}
		}
	}

	// The saved rule covers another prompt to the same model, also when the
	// client sends the token as x-api-key.
	resp, body = llmDo(t, "POST", base+"/chat/completions", map[string]string{"X-Api-Key": tok}, chat("m1", "second prompt", false))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rule-covered chat: HTTP %d %s", resp.StatusCode, body)
	}
	if n := len(surface.shown()); n != 1 {
		t.Fatalf("rule-covered request prompted (%d prompts)", n)
	}

	// Streaming: the first event arrives while the upstream is still generating.
	req, _ := http.NewRequestWithContext(t.Context(), "POST", base+"/chat/completions", strings.NewReader(chat("m1", "stream", true)))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "text/event-stream")
	sresp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer sresp.Body.Close()
	if sresp.StatusCode != http.StatusOK || !strings.HasPrefix(sresp.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("stream: HTTP %d %v", sresp.StatusCode, sresp.Header)
	}
	rd := bufio.NewReader(sresp.Body)
	first := make(chan string, 1)
	go func() {
		line, _ := rd.ReadString('\n')
		first <- line
	}()
	select {
	case line := <-first:
		if !strings.Contains(line, `"Hel"`) {
			t.Fatalf("first event = %q", line)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first event did not arrive before the upstream finished: the stream is buffered")
	}
	close(up.release)
	rest, _ := io.ReadAll(rd)
	if !strings.Contains(string(rest), `"lo"`) || !strings.Contains(string(rest), "[DONE]") {
		t.Fatalf("rest of stream = %q", rest)
	}

	// Another model is not covered by the rule; denied -> 403, nothing sent.
	surface.answer(approval.Answer{})
	resp, body = llmDo(t, "POST", base+"/chat/completions", bearer, chat("m2", "hi", false))
	e := wantLLMError(t, "denied", resp, body, http.StatusForbidden, "permission_denied")
	if !strings.HasPrefix(e.Error.Message, "denied on desktop: ") {
		t.Fatalf("denied message %q", e.Error.Message)
	}
	if n := len(up.chatHeaders()); n != 3 {
		t.Fatalf("%d chats reached the upstream, want 3", n)
	}

	// Paths outside the allowlist are refused without a prompt.
	before := len(surface.shown())
	resp, body = llmDo(t, "POST", base+"/files", bearer, `{}`)
	wantLLMError(t, "disallowed path", resp, body, http.StatusNotFound, "not_found")
	resp, body = llmDo(t, "GET", base+"/chat/completions", bearer, "")
	wantLLMError(t, "wrong method", resp, body, http.StatusNotFound, "not_found")
	resp, body = llmDo(t, "POST", "http://"+raspi.LocalAddr()+"/llm/desktop/nosuch/v1/chat/completions", bearer, chat("m1", "hi", false))
	wantLLMError(t, "unknown service", resp, body, http.StatusNotFound, "not_found")
	if n := len(surface.shown()); n != before {
		t.Fatalf("refused requests prompted")
	}

	// Completions are audited with status and sizes, never content.
	recs, err := desktop.approvals.Audit(50)
	if err != nil {
		t.Fatal(err)
	}
	var done int
	for _, r := range recs {
		if r.Kind == approval.KindCompletion && r.OK != nil && *r.OK && strings.HasPrefix(r.Error, "HTTP 200, ") {
			done++
		}
		if strings.Contains(r.Error+r.Title, "prompt") {
			t.Fatalf("audit has content: %+v", r)
		}
	}
	if done != 3 {
		t.Fatalf("%d successful completions audited, want 3: %+v", done, recs)
	}
}

func TestLLMProxyOnTheHostItself(t *testing.T) {
	up := newFakeLLM(t)
	surface := &scriptSurface{}
	desktop := startLLMNode(t, "desktop", surface, catalog.Service{
		Name: "unsloth", Kind: catalog.KindOpenAI, URL: up.srv.URL,
		Auth: &catalog.Auth{Header: "Authorization", Value: llmHostKey},
	})
	surface.answer(approval.Answer{Allow: true})
	resp, body := llmDo(t, "GET", "http://"+desktop.LocalAddr()+"/llm/desktop/unsloth/v1/models",
		map[string]string{"Authorization": "Bearer " + desktop.controlToken}, "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"m1"`) {
		t.Fatalf("models: HTTP %d %s", resp.StatusCode, body)
	}
	if p := surface.shown(); len(p) != 1 || p[0].Title != "cli on desktop wants to list the models of unsloth" {
		t.Fatalf("prompts %+v", p)
	}
}

func TestLLMProxyPeerOffline(t *testing.T) {
	desktop := startLLMNode(t, "desktop", &scriptSurface{})
	raspi := startLLMNode(t, "raspi", &scriptSurface{})
	pair(t, desktop, raspi)
	desktop.Close()
	resp, body := llmDo(t, "POST", "http://"+raspi.LocalAddr()+"/llm/desktop/unsloth/v1/chat/completions",
		map[string]string{"Authorization": "Bearer " + raspi.controlToken}, `{"model":"m1"}`)
	e := wantLLMError(t, "offline peer", resp, body, http.StatusBadGateway, "device_unreachable")
	if !strings.Contains(e.Error.Message, "mesh_wake") {
		t.Fatalf("message %q does not mention mesh_wake", e.Error.Message)
	}
}

func slicesOfDetails(p approval.Prompt) []string {
	var out []string
	for _, d := range p.Details {
		out = append(out, d.Value)
	}
	return out
}
