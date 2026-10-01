package llmproxy

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"messh/internal/provider"
)

func TestClassifyAllowsOnlyTheModelEndpoints(t *testing.T) {
	cases := []struct {
		method, rest, want string
		ok                 bool
	}{
		{"POST", "v1/chat/completions", "/v1/chat/completions", true},
		{"POST", "/v1/chat/completions", "/v1/chat/completions", true},
		{"POST", "chat/completions", "/v1/chat/completions", true}, // base URL without /v1
		{"GET", "v1/models", "/v1/models", true},
		{"GET", "models", "/v1/models", true},
		{"POST", "v1/completions", "/v1/completions", true},
		{"POST", "v1/embeddings", "/v1/embeddings", true},
		{"POST", "v1/responses", "/v1/responses", true},
		{"POST", "v1/messages", "/v1/messages", true},
		{"POST", "v1/models", "", false},          // wrong method
		{"GET", "v1/chat/completions", "", false}, // wrong method
		{"POST", "v1/files", "", false},           // not a model endpoint
		{"POST", "v1/v1/chat/completions", "", false},
		{"POST", "v1/chat/completions/", "", false},
		{"POST", "v1/../admin", "", false},
		{"POST", "v1/chat%2Fcompletions", "", false},
		{"DELETE", "v1/models", "", false},
		{"POST", "", "", false},
	}
	for _, c := range cases {
		ep, ok := Classify(c.method, c.rest)
		if ok != c.ok || ep.Path != c.want {
			t.Errorf("Classify(%s, %q) = %q, %v; want %q, %v", c.method, c.rest, ep.Path, ok, c.want, c.ok)
		}
	}
	if ep, _ := Classify("GET", "v1/models"); !ep.Read {
		t.Error("listing models is not a read")
	}
	if ep, _ := Classify("POST", "v1/chat/completions"); ep.Read {
		t.Error("a chat completion counts as a read")
	}
}

func TestUpstreamURLAvoidsDoubleV1(t *testing.T) {
	chat, _ := Classify("POST", "v1/chat/completions")
	cases := map[string]string{
		"http://127.0.0.1:8888":           "http://127.0.0.1:8888/v1/chat/completions",
		"http://127.0.0.1:8888/":          "http://127.0.0.1:8888/v1/chat/completions",
		"http://127.0.0.1:11434/v1":       "http://127.0.0.1:11434/v1/chat/completions",
		"http://127.0.0.1:11434/v1/":      "http://127.0.0.1:11434/v1/chat/completions",
		"http://127.0.0.1:9000/proxy":     "http://127.0.0.1:9000/proxy/v1/chat/completions",
		"http://127.0.0.1:9000/proxy/v1/": "http://127.0.0.1:9000/proxy/v1/chat/completions",
	}
	for base, want := range cases {
		b, _ := url.Parse(base)
		if got := UpstreamURL(b, chat, "").String(); got != want {
			t.Errorf("UpstreamURL(%s) = %s, want %s", base, got, want)
		}
	}
	b, _ := url.Parse("http://127.0.0.1:8888/v1")
	msgs, _ := Classify("POST", "v1/messages")
	if got := UpstreamURL(b, msgs, "beta=true").String(); got != "http://127.0.0.1:8888/v1/messages?beta=true" {
		t.Errorf("query: %s", got)
	}
}

func TestRequestHeadersDropCredentialsAndHopByHop(t *testing.T) {
	in := http.Header{}
	in.Set("Authorization", "Bearer agent-token")
	in.Set("X-Api-Key", "agent-token")
	in.Set("Cookie", "session=1")
	in.Set("Connection", "keep-alive, X-Foo")
	in.Set("X-Foo", "bar")
	in.Set("Proxy-Authorization", "Basic x")
	in.Set("Messh-Peer-Id", "spoofed")
	in.Set("Messh-Agent", "spoofed")
	in.Set("Content-Type", "application/json")
	in.Set("Accept", "text/event-stream")
	in.Set("anthropic-version", "2023-06-01")
	in.Set("OpenAI-Beta", "assistants=v2")
	out := RequestHeaders(in)
	want := map[string]string{
		"Content-Type": "application/json", "Accept": "text/event-stream",
		"Anthropic-Version": "2023-06-01", "Openai-Beta": "assistants=v2",
	}
	if len(out) != len(want) {
		t.Fatalf("forwarded %v, want only %v", out, want)
	}
	for k, v := range want {
		if out.Get(k) != v {
			t.Errorf("%s = %q, want %q", k, out.Get(k), v)
		}
	}
}

func TestCopyResponseHeadersDropsHopByHopAndCookies(t *testing.T) {
	src := http.Header{}
	src.Set("Content-Type", "text/event-stream")
	src.Set("Content-Length", "12")
	src.Set("Connection", "close, X-Private")
	src.Set("X-Private", "1")
	src.Set("Transfer-Encoding", "chunked")
	src.Set("Set-Cookie", "a=b")
	src.Set("X-Request-Id", "abc")
	dst := http.Header{}
	CopyResponseHeaders(dst, src)
	if dst.Get("Content-Type") != "text/event-stream" || dst.Get("X-Request-Id") != "abc" {
		t.Fatalf("lost end-to-end headers: %v", dst)
	}
	for _, h := range []string{"Content-Length", "Connection", "X-Private", "Transfer-Encoding", "Set-Cookie"} {
		if dst.Get(h) != "" {
			t.Errorf("%s was copied", h)
		}
	}
}

func TestAgentTokenFromBearerOrAPIKey(t *testing.T) {
	r := httptest.NewRequest("POST", "/", nil)
	r.Header.Set("Authorization", "Bearer tok1")
	if got := AgentToken(r); got != "tok1" {
		t.Errorf("bearer: %q", got)
	}
	r = httptest.NewRequest("POST", "/", nil)
	r.Header.Set("x-api-key", "tok2")
	if got := AgentToken(r); got != "tok2" {
		t.Errorf("x-api-key: %q", got)
	}
	r = httptest.NewRequest("POST", "/", nil)
	if got := AgentToken(r); got != "" {
		t.Errorf("none: %q", got)
	}
}

func TestScanBodyFindsModelAndReplaysExactly(t *testing.T) {
	cases := []struct {
		body     string
		model    string
		hasModel bool
		stream   bool
	}{
		{`{"model":"qwen3:8b","messages":[{"role":"user","content":"hi {\"model\":\"x\"}"}],"stream":true}`, "qwen3:8b", true, true},
		{`{"messages":[{"role":"user","content":"hi"}],"stream":false,"model":"late"}`, "late", true, false},
		{`{"messages":[{"role":"user","content":"no model"}],"options":{"model":"nested"}}`, "", false, false},
		{`{"model":42}`, "", false, false},
		{`{"stream":"yes"}`, "", false, false},
		{`not json at all`, "", false, false},
		{``, "", false, false},
	}
	for _, c := range cases {
		info, replay, err := ScanBody(strings.NewReader(c.body))
		if err != nil {
			t.Errorf("%s: %v", c.body, err)
			continue
		}
		if info.Model != c.model || info.HasModel != c.hasModel || info.Stream != c.stream {
			t.Errorf("%s: got %+v", c.body, info)
		}
		got, _ := io.ReadAll(replay)
		if string(got) != c.body {
			t.Errorf("replay changed the body: %q -> %q", c.body, got)
		}
	}
}

func TestScanBodyRejectsDuplicateModel(t *testing.T) {
	_, _, err := ScanBody(strings.NewReader(`{"model":"approved","messages":[],"model":"other"}`))
	if !errors.Is(err, ErrDuplicateModel) {
		t.Fatalf("err = %v", err)
	}
}

func TestScanBodyReportsReadErrors(t *testing.T) {
	rec := httptest.NewRecorder()
	big := `{"model":"m","messages":"` + strings.Repeat("x", 100) + `"}`
	r := http.MaxBytesReader(rec, io.NopCloser(strings.NewReader(big)), 20)
	_, _, err := ScanBody(r)
	var tooBig *http.MaxBytesError
	if !errors.As(err, &tooBig) {
		t.Fatalf("err = %v, want MaxBytesError", err)
	}
}

func TestErrorBodyIsOpenAIStyle(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteError(rec, http.StatusForbidden, ErrDenied, "denied on desktop: denied by the user")
	if rec.Code != 403 || rec.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("status %d, type %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	var e struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Error.Message != "denied on desktop: denied by the user" || e.Error.Type != "permission_denied" {
		t.Fatalf("body = %s", rec.Body.String())
	}
}

func TestBuildApprovalKeysOnServiceEndpointAndModel(t *testing.T) {
	caller := provider.Caller{DeviceID: "id", DeviceName: "raspi", Agent: "omp"}
	chat, _ := Classify("POST", "v1/chat/completions")
	a := BuildApproval("unsloth", chat, BodyInfo{Model: "qwen", HasModel: true, Stream: true}, caller, true)
	if a.Title != "omp on raspi wants to use the unsloth language model" {
		t.Errorf("title %q", a.Title)
	}
	if a.Auto != "" {
		t.Error("auto.read pre-authorised a chat completion")
	}
	keys := []string{}
	for _, s := range a.Scopes {
		keys = append(keys, s.Key)
	}
	if strings.Join(keys, " ") != "service:unsloth:llm:qwen service:unsloth:llm service:unsloth:*" || !a.Scopes[2].Broad || a.Scopes[0].Broad {
		t.Errorf("scopes %+v", a.Scopes)
	}
	details := map[string]string{}
	for _, d := range a.Details {
		details[d.Label] = d.Value
	}
	if details["Model"] != "qwen" || details["Streaming"] != "yes" || details["Endpoint"] != "POST /v1/chat/completions" {
		t.Errorf("details %+v", a.Details)
	}

	// Same model, another prompt or streaming flag: same exact hash.
	b := BuildApproval("unsloth", chat, BodyInfo{Model: "qwen", HasModel: true}, caller, false)
	if a.Exact != b.Exact {
		t.Error("exact hash depends on more than service, endpoint and model")
	}
	other := BuildApproval("unsloth", chat, BodyInfo{Model: "llama", HasModel: true}, caller, false)
	emb, _ := Classify("POST", "v1/embeddings")
	otherEP := BuildApproval("unsloth", emb, BodyInfo{Model: "qwen", HasModel: true}, caller, false)
	if other.Exact == a.Exact || otherEP.Exact == a.Exact {
		t.Error("exact hash ignores the model or the endpoint")
	}

	// No model: no per-model scope.
	none := BuildApproval("unsloth", chat, BodyInfo{}, caller, false)
	if len(none.Scopes) != 2 || none.Scopes[0].Key != "service:unsloth:llm" {
		t.Errorf("no-model scopes %+v", none.Scopes)
	}

	// Listing models is a read: auto.read allows it, otherwise it prompts.
	models, _ := Classify("GET", "v1/models")
	if r := BuildApproval("unsloth", models, BodyInfo{}, caller, true); r.Auto == "" || !strings.Contains(r.Title, "list the models") {
		t.Errorf("models with auto.read: %+v", r)
	}
	if r := BuildApproval("unsloth", models, BodyInfo{}, caller, false); r.Auto != "" {
		t.Error("models without auto.read was pre-authorised")
	}
}
