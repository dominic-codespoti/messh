package node

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/control"
	"messh/internal/state"
)

func startNode(t *testing.T, name string) *Node {
	t.Helper()
	n, err := Start(t.Context(), Options{
		Paths:     state.Paths{Root: t.TempDir()},
		Name:      name,
		MeshAddr:  "127.0.0.1:0",
		LocalAddr: "127.0.0.1:0",
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	return n
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// pair runs the full two-sided pairing: acceptor opens its window, both
// users see the same code, both accept.
func pair(t *testing.T, acceptor, initiator *Node) {
	t.Helper()
	acceptor.pairing.open(time.Minute, time.Now())
	og, err := initiator.startPairing(t.Context(), acceptor.MeshAddr())
	if err != nil {
		t.Fatal(err)
	}
	var in control.Incoming
	waitFor(t, "incoming pairing request", func() bool {
		list := acceptor.pairing.listIncoming()
		if len(list) == 1 {
			in = list[0]
		}
		return len(list) == 1
	})
	if in.Code != og.Code || in.PeerID != initiator.ID() || og.PeerID != acceptor.ID() {
		t.Fatalf("sides disagree: acceptor sees %s/%s, initiator sees %s/%s", in.Code, in.PeerID, og.Code, og.PeerID)
	}
	if err := acceptor.decideIncoming(in.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := initiator.confirmPairing(og.ID, true); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "initiator to record the pairing", func() bool {
		v, _ := initiator.outgoingView(og.ID)
		return v.State == control.OutPaired
	})
}

// agentSession connects to n's agent endpoint the way an MCP client would.
func agentSession(t *testing.T, n *Node) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-agent", Version: "1"}, nil)
	s, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:             "http://" + n.LocalAddr() + "/mcp",
		HTTPClient:           &http.Client{Transport: authHeader("Bearer " + n.controlToken)},
		MaxRetries:           -1,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

type authHeader string

func (a authHeader) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", string(a))
	return http.DefaultTransport.RoundTrip(r)
}

func TestAgentReachesPairedDeviceTools(t *testing.T) {
	desktop := startNode(t, "desktop")
	raspi := startNode(t, "raspi")
	pair(t, desktop, raspi)

	s := agentSession(t, raspi)
	waitFor(t, "desktop tools to reach the raspi gateway", func() bool {
		for tool, err := range s.Tools(t.Context(), nil) {
			if err == nil && tool.Name == "desktop__node_info" {
				return true
			}
		}
		return false
	})
	res, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: "desktop__node_info"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("tool error: %v", res.Content)
	}
	var info struct{ ID, Name string }
	if err := json.Unmarshal([]byte(res.Content[0].(*mcp.TextContent).Text), &info); err != nil {
		t.Fatal(err)
	}
	if info.ID != desktop.ID() || info.Name != "desktop" {
		t.Fatalf("node_info answered by %s/%s, want the desktop", info.Name, info.ID)
	}
}

func TestUnpairedDeviceIsRefused(t *testing.T) {
	desktop := startNode(t, "desktop")
	stranger := startNode(t, "stranger")

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "https://"+desktop.MeshAddr()+"/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := (&http.Client{Transport: stranger.pinnedTransport(desktop.ID())}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("unpaired device got %s, want 403", resp.Status)
	}
}

func TestPairingNeedsOpenWindow(t *testing.T) {
	desktop := startNode(t, "desktop")
	raspi := startNode(t, "raspi")

	og, err := raspi.startPairing(t.Context(), desktop.MeshAddr())
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "pairing to fail", func() bool {
		v, _ := raspi.outgoingView(og.ID)
		return v.State == control.OutFailed
	})
	if len(desktop.roster.List()) != 0 || len(raspi.roster.List()) != 0 {
		t.Fatal("a peer was recorded although the desktop never opened its pairing window")
	}
}

func TestInitiatorDecliningCodeKeepsPeerOut(t *testing.T) {
	desktop := startNode(t, "desktop")
	raspi := startNode(t, "raspi")
	desktop.pairing.open(time.Minute, time.Now())

	og, err := raspi.startPairing(t.Context(), desktop.MeshAddr())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raspi.confirmPairing(og.ID, false); err != nil {
		t.Fatal(err)
	}
	v, _ := raspi.outgoingView(og.ID)
	if v.State != control.OutCancelled {
		t.Fatalf("state after declining = %s, want %s", v.State, control.OutCancelled)
	}
	if len(raspi.roster.List()) != 0 {
		t.Fatal("initiator recorded a peer whose code its user declined")
	}
}

func TestSpoofedIdentityHeadersAreReplaced(t *testing.T) {
	desktop := startNode(t, "desktop")
	raspi := startNode(t, "raspi")
	pair(t, desktop, raspi)

	var seen http.Header
	h := desktop.requirePeer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { seen = r.Header.Clone() }))
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "/mcp", bytes.NewReader(nil))
	req.Header.Set(hdrPeerID, "forged")
	req.Header.Set(hdrPeerName, "forged")
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{raspi.id.Cert.Leaf}}
	h.ServeHTTP(httptest.NewRecorder(), req)
	if got := seen.Values(hdrPeerID); len(got) != 1 || got[0] != raspi.ID() {
		t.Fatalf("%s = %v, want only %s", hdrPeerID, got, raspi.ID())
	}
	if got := seen.Get(hdrPeerName); got != "raspi" {
		t.Fatalf("%s = %q, want raspi", hdrPeerName, got)
	}
}
