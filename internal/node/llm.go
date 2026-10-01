package node

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/catalog"
	"messh/internal/gateway"
	"messh/internal/llmproxy"
	"messh/internal/provider"
	"messh/internal/state"
)

// hdrLLMAgent carries the calling agent's name on proxied model requests.
// The host believes it only because the request arrives from a paired,
// certificate-pinned peer, which authenticated the agent itself.
const hdrLLMAgent = "Messh-Agent"

// llmListTimeout bounds asking one device for its catalogue.
const llmListTimeout = 15 * time.Second

// llmHost answers model requests for this device's own services.
func (n *Node) llmHost() *llmproxy.Host {
	return &llmproxy.Host{
		Device:    n.name,
		Lookup:    n.openAIUpstream,
		Approvals: n.approvals,
		Log:       n.log.With("component", "llm"),
	}
}

func (n *Node) openAIUpstream(name string) (catalog.Upstream, bool) {
	if n.catalog == nil {
		return catalog.Upstream{}, false
	}
	return n.catalog.OpenAIUpstream(name)
}

// mountLLM serves the agent-facing model endpoint on the loopback server:
// /llm/<device>/<service>/v1/..., authenticated with the agent's messh token.
func (n *Node) mountLLM(mux *http.ServeMux) {
	f := &llmForwarder{n: n, host: n.llmHost(), tr: map[string]*http.Transport{}}
	n.goRun(func() {
		<-n.ctx.Done()
		f.closeIdle()
	})
	mux.HandleFunc("/llm/", func(w http.ResponseWriter, _ *http.Request) {
		llmproxy.WriteError(w, http.StatusNotFound, llmproxy.ErrNotFound,
			"use /llm/<device>/<service>/v1/... (see `messh llm ls`)")
	})
	mux.HandleFunc("/llm/{device}/{service}/{rest...}", f.serveLocal)
}

// mountMeshLLM serves model requests from paired peers for this device's services.
func (n *Node) mountMeshLLM(mux *http.ServeMux) {
	host := n.llmHost()
	mux.Handle("/v1/llm/{service}/{rest...}", n.requirePeer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		agent := r.Header.Get(hdrLLMAgent)
		if !state.ValidAgentName(agent) {
			llmproxy.WriteError(w, http.StatusBadRequest, llmproxy.ErrInvalid, "missing or invalid agent name")
			return
		}
		caller := provider.Caller{DeviceID: r.Header.Get(hdrPeerID), DeviceName: r.Header.Get(hdrPeerName), Agent: agent}
		host.Serve(w, r, caller, r.PathValue("service"), r.PathValue("rest"))
	})))
}

// llmForwarder routes local agents' model requests to the device hosting the
// service. It keeps one HTTP/1.1 transport per peer.
type llmForwarder struct {
	n    *Node
	host *llmproxy.Host
	mu   sync.Mutex
	tr   map[string]*http.Transport
}

func (f *llmForwarder) serveLocal(w http.ResponseWriter, r *http.Request) {
	n := f.n
	ti, err := n.verifyAgent(r.Context(), llmproxy.AgentToken(r), r)
	if err != nil {
		llmproxy.WriteError(w, http.StatusUnauthorized, llmproxy.ErrAuth,
			"invalid messh agent token: use the output of `messh agent token NAME` as the API key")
		return
	}
	agent := ti.UserID
	service, rest := r.PathValue("service"), r.PathValue("rest")
	ep, ok := llmproxy.Classify(r.Method, rest)
	if !ok {
		llmproxy.WriteNotForwarded(w, r.Method, rest)
		return
	}
	if !llmproxy.ValidService(service) {
		llmproxy.WriteError(w, http.StatusNotFound, llmproxy.ErrNotFound, fmt.Sprintf("invalid service name %q", service))
		return
	}
	id, label, err := n.files.resolveDevice(r.PathValue("device"))
	if err != nil {
		llmproxy.WriteError(w, http.StatusNotFound, llmproxy.ErrNotFound, err.Error())
		return
	}
	if r.ContentLength > llmproxy.MaxBody {
		llmproxy.WriteError(w, http.StatusRequestEntityTooLarge, llmproxy.ErrTooLarge, fmt.Sprintf("request body over %d MiB", llmproxy.MaxBody>>20))
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, llmproxy.MaxBody)
	if id == n.id.ID {
		f.host.Serve(w, r, provider.Caller{DeviceID: n.id.ID, DeviceName: n.name, Agent: agent}, service, rest)
		return
	}
	f.forward(w, r, id, label, agent, service, ep)
}

// forward streams the request to the peer's mesh route and the answer back.
// Cancelling the client's request cancels the peer's, which withdraws a
// pending prompt or stops the upstream generation.
func (f *llmForwarder) forward(w http.ResponseWriter, r *http.Request, id, label, agent, service string, ep llmproxy.Endpoint) {
	u := "https://messh-peer/v1/llm/" + url.PathEscape(service) + ep.Path
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	body := r.Body
	if ep.Method != http.MethodPost {
		body = http.NoBody
	}
	out, err := http.NewRequestWithContext(r.Context(), ep.Method, u, body)
	if err != nil {
		llmproxy.WriteError(w, http.StatusBadRequest, llmproxy.ErrInvalid, err.Error())
		return
	}
	out.Header = llmproxy.RequestHeaders(r.Header)
	out.Header.Set(hdrLLMAgent, agent)
	if ep.Method == http.MethodPost && r.ContentLength > 0 {
		out.ContentLength = r.ContentLength
	}
	resp, err := f.transport(id).RoundTrip(out)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			llmproxy.WriteError(w, http.StatusRequestEntityTooLarge, llmproxy.ErrTooLarge, fmt.Sprintf("request body over %d MiB", llmproxy.MaxBody>>20))
			return
		}
		llmproxy.WriteError(w, http.StatusBadGateway, llmproxy.ErrUnreached, fmt.Sprintf(
			"%s is unreachable: %v. If it is asleep, wake it with the mesh_wake tool, then retry.", label, err))
		return
	}
	defer resp.Body.Close()
	llmproxy.Relay(w, resp)
}

func (f *llmForwarder) transport(peerID string) *http.Transport {
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.tr[peerID]; ok {
		return t
	}
	t := &http.Transport{
		// Connections are made to the peer's verified addresses only; the
		// request URL's host is a placeholder.
		DialTLSContext:      func(ctx context.Context, _, _ string) (net.Conn, error) { return f.dial(ctx, peerID) },
		DisableCompression:  true,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConnsPerHost: 4,
	}
	f.tr[peerID] = t
	return t
}

// dial tries the peer's addresses in the usual order and returns the first
// connection whose TLS handshake proves it is the pinned device. The request
// is written only after that, so a stale address never consumes the body.
func (f *llmForwarder) dial(ctx context.Context, id string) (net.Conn, error) {
	n := f.n
	addrs := n.peers.candidates(id)
	if len(addrs) == 0 {
		return nil, errors.New("no known address yet (it appears once discovery hears the device)")
	}
	var lastErr error
	for _, addr := range addrs {
		raw, err := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "tcp", addr)
		if err != nil {
			lastErr = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		cfg := n.pinnedTLS(id)
		cfg.NextProtos = []string{"http/1.1"}
		c := tls.Client(raw, cfg)
		hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = c.HandshakeContext(hctx)
		cancel()
		if err != nil {
			raw.Close()
			lastErr = err
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		now := time.Now()
		n.peers.markContact(id, now)
		if serr := n.roster.Seen(id, addr, now); serr != nil {
			n.log.Warn("save peer address", "error", serr)
		}
		return c, nil
	}
	return nil, fmt.Errorf("tried %s: %v", strings.Join(addrs, ", "), lastErr)
}

func (f *llmForwarder) closeIdle() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.tr {
		t.CloseIdleConnections()
	}
}

// registerLLMAPI adds the control API route behind `messh llm ls/config`.
func (n *Node) registerLLMAPI(api *http.ServeMux) {
	api.HandleFunc("GET "+llmproxy.ServicesPath, n.apiLLMServices)
}

// apiLLMServices asks every device (or ?device=) for its openai-kind services
// through the existing catalogue tool and adds the local proxy URL of each.
func (n *Node) apiLLMServices(w http.ResponseWriter, r *http.Request) {
	nodes := n.Nodes()
	handles := gateway.Handles(nodes)
	if ref := r.URL.Query().Get("device"); ref != "" {
		id, _, err := n.files.resolveDevice(ref)
		if err != nil {
			writeError(w, http.StatusNotFound, err.Error())
			return
		}
		for _, d := range nodes {
			if d.ID == id {
				nodes = []gateway.Node{d}
				break
			}
		}
	}
	rows := make([][]llmproxy.Listing, len(nodes))
	var wg sync.WaitGroup
	for i, d := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows[i] = n.llmServicesOn(r.Context(), d, handles[d.ID])
		}()
	}
	wg.Wait()
	out := []llmproxy.Listing{}
	for _, rs := range rows {
		out = append(out, rs...)
	}
	writeJSON(w, http.StatusOK, out)
}

func (n *Node) llmServicesOn(ctx context.Context, d gateway.Node, handle string) []llmproxy.Listing {
	base := llmproxy.Listing{Device: d.Name, Handle: handle, DeviceID: d.ID, Self: d.Self, Online: d.Online}
	ctx, cancel := context.WithTimeout(ctx, llmListTimeout)
	defer cancel()
	res, err := n.Call(ctx, d.ID, "catalogue", json.RawMessage(`{"kind":"openai"}`), cliAgent)
	if err == nil && res.IsError {
		err = errors.New(resultMessage(res))
	}
	var cat struct {
		Services []struct {
			Name        string `json:"name"`
			Kind        string `json:"kind"`
			Description string `json:"description"`
			Up          bool   `json:"up"`
		} `json:"services"`
	}
	if err == nil {
		if jerr := json.Unmarshal([]byte(resultMessage(res)), &cat); jerr != nil {
			err = fmt.Errorf("unexpected catalogue reply: %v", jerr)
		}
	}
	if err != nil {
		base.Error = err.Error()
		return []llmproxy.Listing{base}
	}
	var out []llmproxy.Listing
	for _, s := range cat.Services {
		if s.Kind != catalog.KindOpenAI {
			continue
		}
		l := base
		l.Service, l.Description, l.Up = s.Name, s.Description, s.Up
		l.BaseURL = "http://" + n.LocalAddr() + "/llm/" + url.PathEscape(handle) + "/" + url.PathEscape(s.Name) + "/v1"
		out = append(out, l)
	}
	return out
}

// resultMessage is the text of a tool result.
func resultMessage(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}
