package node

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"messh/internal/approval"
	"messh/internal/control"
	"messh/internal/provider"
)

// holdSurface records prompts and leaves them open until their request ends,
// like a toast nobody has touched yet.
type holdSurface struct {
	mu      sync.Mutex
	prompts []approval.Prompt
}

func (*holdSurface) Name() string    { return "hold" }
func (*holdSurface) Available() bool { return true }
func (*holdSurface) Parallel() bool  { return true }
func (h *holdSurface) Ask(ctx context.Context, p approval.Prompt) (approval.Answer, error) {
	h.mu.Lock()
	h.prompts = append(h.prompts, p)
	h.mu.Unlock()
	<-ctx.Done()
	return approval.Answer{}, ctx.Err()
}

func (h *holdSurface) prompt(t *testing.T, i int) approval.Prompt {
	t.Helper()
	waitFor(t, "a prompt", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		return len(h.prompts) > i
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.prompts[i]
}

func gatedRequest(exact string) approval.Request {
	return approval.Request{
		Caller: provider.Caller{DeviceID: "id-raspi", DeviceName: "raspi", Agent: "omp"},
		Tool:   "fake_run", Class: provider.ClassExec,
		Approval: provider.Approval{
			Title: "Run it", Exact: exact,
			Details: []provider.Detail{{Label: "Command", Value: exact}},
		},
	}
}

func post(t *testing.T, url, contentType, body string, mod ...func(*http.Request)) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	for _, m := range mod {
		m(req)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

func TestRespondEndpointNeedsOnlyTheRequestsNonce(t *testing.T) {
	hold := &holdSurface{}
	n := startGateNode(t, "desktop", hold)
	ticket := n.approvals.Submit(gatedRequest("build"))
	other := n.approvals.Submit(gatedRequest("other"))
	p := hold.prompt(t, 0)
	if want := n.localLn.Addr().String(); want != "127.0.0.1:"+strconv.Itoa(p.Port) {
		t.Fatalf("prompt port %d, node listens on %s", p.Port, want)
	}
	if p.ID != ticket.ID() { // the two prompts may arrive in either order
		p = hold.prompt(t, 1)
	}
	url := "http://" + n.LocalAddr() + "/v1/approvals/" + p.ID + "/respond"
	good := `{"nonce":"` + p.Nonce + `","decision":"once"}`
	pendingStill := func(what string) {
		t.Helper()
		if len(n.approvals.Pending()) != 2 {
			t.Fatalf("%s resolved a request", what)
		}
	}

	for name, tc := range map[string]struct {
		do   func() *http.Response
		want int
	}{
		"wrong nonce": {func() *http.Response {
			return post(t, url, "application/json", `{"nonce":"`+strings.Repeat("0", 32)+`","decision":"once"}`)
		}, http.StatusForbidden},
		"the other request's id with this nonce": {func() *http.Response {
			return post(t, "http://"+n.LocalAddr()+"/v1/approvals/"+other.ID()+"/respond", "application/json", good)
		}, http.StatusForbidden},
		"text/plain":      {func() *http.Response { return post(t, url, "text/plain", good) }, http.StatusUnsupportedMediaType},
		"no content type": {func() *http.Response { return post(t, url, "", good) }, http.StatusUnsupportedMediaType},
		"form encoding": {func() *http.Response {
			return post(t, url, "application/x-www-form-urlencoded", good)
		}, http.StatusUnsupportedMediaType},
		"cross-site origin": {func() *http.Response {
			return post(t, url, "application/json", good, func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") })
		}, http.StatusForbidden},
		"foreign Host header": {func() *http.Response {
			return post(t, url, "application/json", good, func(r *http.Request) { r.Host = "evil.example:7520" })
		}, http.StatusForbidden},
		"unknown field": {func() *http.Response {
			return post(t, url, "application/json", `{"nonce":"`+p.Nonce+`","decision":"once","scope":3}`)
		}, http.StatusBadRequest},
		"not json": {func() *http.Response { return post(t, url, "application/json", "nonce=x") }, http.StatusBadRequest},
		"oversized": {func() *http.Response {
			return post(t, url, "application/json", `{"nonce":"`+strings.Repeat("a", 10000)+`"}`)
		}, http.StatusBadRequest},
		"unknown decision": {func() *http.Response {
			return post(t, url, "application/json", `{"nonce":"`+p.Nonce+`","decision":"allow"}`)
		}, http.StatusBadRequest},
		"GET": {func() *http.Response {
			resp, err := http.Get(url)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			return resp
		}, http.StatusMethodNotAllowed},
	} {
		if got := tc.do().StatusCode; got != tc.want {
			t.Errorf("%s: status %d, want %d", name, got, tc.want)
		}
		pendingStill(name)
	}

	// The right nonce needs no control token and no other credential.
	if got := post(t, url, "application/json; charset=utf-8", good).StatusCode; got != http.StatusNoContent {
		t.Fatalf("valid response: status %d", got)
	}
	if ok, err := ticket.Wait(t.Context()); !ok || err != nil {
		t.Fatalf("the request was not allowed: %v, %v", ok, err)
	}
	// Single use: the nonce is dead once its request is decided.
	if got := post(t, url, "application/json", good).StatusCode; got != http.StatusNotFound {
		t.Fatalf("reused nonce: status %d, want 404", got)
	}
	if len(n.approvals.Pending()) != 1 {
		t.Fatal("answering one request touched another")
	}
	// The helper process's own client takes the same path.
	p2 := hold.prompt(t, 0)
	if p2.ID == ticket.ID() {
		p2 = hold.prompt(t, 1)
	}
	target := approval.RespondTarget{Port: p2.Port, ID: p2.ID, Nonce: p2.Nonce, Action: approval.ActionDeny}
	if err := target.Send(t.Context()); err != nil {
		t.Fatal(err)
	}
	var denied *approval.DeniedError
	if _, err := other.Wait(t.Context()); !errors.As(err, &denied) {
		t.Fatalf("deny: %v", err)
	}
	if err := target.Send(t.Context()); err == nil || !strings.Contains(err.Error(), "no longer waiting") {
		t.Fatalf("answering a decided request: %v", err)
	}
	if rules := n.approvals.Rules(); len(rules) != 0 {
		t.Fatalf("rules = %+v", rules)
	}
}

func TestRespondEndpointCannotReachTheControlAPI(t *testing.T) {
	n := startGateNode(t, "desktop", &holdSurface{})
	for _, path := range []string{"/v1/approvals", "/v1/rules", "/v1/audit", "/v1/status", "/v1/approvals/test"} {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			req, _ := http.NewRequestWithContext(t.Context(), method, "http://"+n.LocalAddr()+path, bytes.NewReader(nil))
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s without a token: status %d", method, path, resp.StatusCode)
			}
		}
	}
}

func TestApprovalTestEndpointUsesTheRealSurfaceAndSavesNothing(t *testing.T) {
	surface := &scriptSurface{}
	surface.answer(approval.Answer{Allow: true, Always: true, Scope: 1})
	n := startGateNode(t, "desktop", surface)
	api := &control.Client{Base: "http://" + n.LocalAddr(), Token: n.controlToken, HTTP: &http.Client{Timeout: 20 * time.Second}}

	var res control.ApprovalTestResult
	if err := api.Do(t.Context(), http.MethodPost, "/v1/approvals/test", struct{}{}, &res); err != nil {
		t.Fatal(err)
	}
	if res.Outcome != approval.OutcomeAllowAlways || res.Scope != 1 || res.Surface != "script" || !res.Allowed || res.Via != "script" {
		t.Fatalf("result = %+v", res)
	}
	shown := surface.shown()
	if len(shown) != 1 || !strings.HasPrefix(shown[0].Title, "TEST") {
		t.Fatalf("the surface showed %+v", shown)
	}
	if rules := n.approvals.Rules(); len(rules) != 0 {
		t.Fatalf("a test request saved a rule: %+v", rules)
	}
	bad := &control.Client{Base: api.Base, Token: "nope", HTTP: http.DefaultClient}
	if err := bad.Do(t.Context(), http.MethodPost, "/v1/approvals/test", struct{}{}, nil); err == nil {
		t.Fatal("the test endpoint ran without the control token")
	}
}
