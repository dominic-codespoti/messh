package node

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/control"
	"messh/internal/identity"
	"messh/internal/provider"
)

// Headers set by requirePeer after verifying the caller's certificate. Values
// a client sends under these names are discarded.
const (
	hdrPeerID   = "Messh-Peer-Id"
	hdrPeerName = "Messh-Peer-Name"
)

// metaAgent carries the calling agent's name in tools/call _meta.
const metaAgent = "messh/agent"

func (n *Node) serverTLS() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{n.id.Cert},
		ClientAuth:   tls.RequireAnyClientCert, // identity is the pinned device ID, checked per request
		MinVersion:   tls.VersionTLS13,
	}
}

// pinnedTLS trusts exactly the device with peerID, ignoring certificate chains.
func (n *Node) pinnedTLS(peerID string) *tls.Config {
	return &tls.Config{
		Certificates:       []tls.Certificate{n.id.Cert},
		InsecureSkipVerify: true, // replaced by the device ID pin in VerifyConnection
		MinVersion:         tls.VersionTLS13,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("peer presented no certificate")
			}
			if got := identity.IDFromCert(cs.PeerCertificates[0]); got != peerID {
				return fmt.Errorf("device at this address is %s, expected %s", identity.Short(got), identity.Short(peerID))
			}
			return nil
		},
	}
}

func (n *Node) pinnedTransport(peerID string) *http.Transport {
	return &http.Transport{
		DialContext:         (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
		TLSClientConfig:     n.pinnedTLS(peerID),
		TLSHandshakeTimeout: 5 * time.Second,
		ForceAttemptHTTP2:   true,
		IdleConnTimeout:     90 * time.Second,
		MaxIdleConnsPerHost: 2,
	}
}

func (n *Node) meshHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/pair", n.handleHello)
	mux.Handle("/mcp", n.requirePeer(n.peerMCP()))
	mux.Handle("POST /v1/tools-changed", n.requirePeer(http.HandlerFunc(n.handleToolsChanged)))
	n.files.mount(mux)
	n.mountMeshLLM(mux)
	n.mountWake(mux)
	return mux
}

// peerMCP serves this device's own tools to paired peers. The tool set lives
// on n.peerSrv and is kept current by rebuildTools.
func (n *Node) peerMCP() http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return n.peerSrv }, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
	})
}

func (n *Node) peerToolHandler(name string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var caller provider.Caller
		if req.Extra != nil {
			caller.DeviceID = req.Extra.Header.Get(hdrPeerID)
			caller.DeviceName = req.Extra.Header.Get(hdrPeerName)
		}
		if agent, ok := req.Params.Meta[metaAgent].(string); ok {
			caller.Agent = agent
		}
		return n.dispatch(ctx, name, req.Params.Arguments, caller), nil
	}
}

// requirePeer admits only devices in the roster and stamps verified identity headers.
func (n *Node) requirePeer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := peerIDOf(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "client certificate required")
			return
		}
		p, paired := n.roster.Get(id)
		if !paired {
			writeError(w, http.StatusForbidden, fmt.Sprintf("device %s is not paired with %s", identity.Short(id), n.name))
			return
		}
		r.Header.Del(hdrPeerID)
		r.Header.Del(hdrPeerName)
		r.Header.Set(hdrPeerID, p.ID)
		r.Header.Set(hdrPeerName, p.Name)
		n.peers.markContact(p.ID, time.Now())
		next.ServeHTTP(w, r)
	})
}

// handleHello receives a pairing request and holds it open until this
// device's user accepts or rejects it (see `messh pair accept`).
func (n *Node) handleHello(w http.ResponseWriter, r *http.Request) {
	peerID, ok := peerIDOf(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "client certificate required")
		return
	}
	if peerID == n.id.ID {
		writeError(w, http.StatusBadRequest, "a device cannot pair with itself")
		return
	}
	var h control.Hello
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&h); err != nil {
		writeError(w, http.StatusBadRequest, "invalid pairing request")
		return
	}
	if !ValidName(h.Name) || h.Port < 1 || h.Port > 65535 {
		writeError(w, http.StatusBadRequest, "invalid name or port in pairing request")
		return
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "unknown remote address")
		return
	}
	addr := net.JoinHostPort(host, strconv.Itoa(h.Port))
	in, err := n.pairing.addIncoming(peerID, h.Name, addr, identity.PairingCode(n.id.ID, peerID), time.Now())
	switch {
	case errors.Is(err, errWindowClosed):
		writeError(w, http.StatusForbidden, fmt.Sprintf("%s is not accepting pairings; run `messh pair accept` on it first", n.name))
		return
	case err != nil:
		writeError(w, http.StatusTooManyRequests, err.Error())
		return
	}
	defer n.pairing.removeIncoming(in.ID)
	n.log.Info("pairing request", "from", h.Name, "id", identity.Short(peerID), "addr", addr, "code", in.Code)

	timer := time.NewTimer(time.Until(in.deadline))
	defer timer.Stop()
	select {
	case accepted := <-in.decided:
		if accepted {
			writeJSON(w, http.StatusOK, control.HelloReply{Accepted: true, ID: n.id.ID, Name: n.name})
		} else {
			writeJSON(w, http.StatusForbidden, control.HelloReply{})
		}
	case <-timer.C:
		writeError(w, http.StatusForbidden, fmt.Sprintf("nobody answered the pairing request on %s in time", n.name))
	case <-r.Context().Done():
	}
}

func peerIDOf(r *http.Request) (string, bool) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return "", false
	}
	return identity.IDFromCert(r.TLS.PeerCertificates[0]), true
}

// lanOnly drops connections from addresses outside loopback, private, and
// link-local ranges, keeping the mesh port off routed networks.
type lanOnly struct {
	net.Listener
	log *slog.Logger
}

func (l lanOnly) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if isLAN(c.RemoteAddr()) {
			return c, nil
		}
		l.log.Warn("rejected connection from outside the LAN", "remote", c.RemoteAddr().String())
		c.Close()
	}
}

func isLAN(a net.Addr) bool {
	ta, ok := a.(*net.TCPAddr)
	if !ok {
		return false
	}
	ip, ok := netip.AddrFromSlice(ta.IP)
	if !ok {
		return false
	}
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, control.Error{Error: msg})
}
