package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/identity"
	"messh/internal/provider"
)

// peerSet manages MCP client sessions to paired peers and tracks verified contact.
type peerSet struct {
	n      *Node
	client *mcp.Client

	connecting sync.Map // peer ID → *sync.Mutex; one session creation per peer at a time
	mu         sync.Mutex
	conns      map[string]*peerConn
	contact    map[string]time.Time // last verified contact per peer
	busy       map[string]bool      // tool refresh in flight
}

type peerConn struct {
	addr      string
	session   *mcp.ClientSession
	transport *http.Transport
}

// peerCallError distinguishes a definitive JSON-RPC rejection from transport failure.
type peerCallError struct {
	err      error
	response bool
}

func (e *peerCallError) Error() string { return e.err.Error() }
func (e *peerCallError) Unwrap() error { return e.err }

func (c *peerConn) close() {
	c.session.Close()
	c.transport.CloseIdleConnections()
}

func newPeerSet(n *Node) *peerSet {
	return &peerSet{
		n:       n,
		client:  mcp.NewClient(&mcp.Implementation{Name: "messh", Version: n.build.Version}, nil),
		conns:   map[string]*peerConn{},
		contact: map[string]time.Time{},
		busy:    map[string]bool{},
	}
}

func (ps *peerSet) markContact(id string, at time.Time) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if at.After(ps.contact[id]) {
		ps.contact[id] = at
	}
}

func (ps *peerSet) lastContact(id string) time.Time {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.contact[id]
}

// candidates lists addresses to try: a fresh discovery sighting first, then
// addresses that worked before.
func (ps *peerSet) candidates(id string) []string {
	var out []string
	if s, ok := ps.n.sighting(id); ok && time.Since(s.Seen) < onlineWindow {
		out = append(out, s.Addr.String())
	}
	if p, ok := ps.n.roster.Get(id); ok {
		for _, a := range p.Addrs {
			if !slices.Contains(out, a) {
				out = append(out, a)
			}
		}
	}
	return out
}

func (ps *peerSet) session(ctx context.Context, id, addr string) (*mcp.ClientSession, error) {
	lock, _ := ps.connecting.LoadOrStore(id, new(sync.Mutex))
	lock.(*sync.Mutex).Lock()
	defer lock.(*sync.Mutex).Unlock()
	ps.mu.Lock()
	if c, ok := ps.conns[id]; ok && c.addr == addr {
		ps.mu.Unlock()
		return c.session, nil
	}
	ps.mu.Unlock()

	tr := ps.n.pinnedTransport(id)
	cs, err := ps.client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             "https://" + addr + "/mcp",
		HTTPClient:           &http.Client{Transport: tr},
		MaxRetries:           -1,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		tr.CloseIdleConnections()
		return nil, err
	}
	ps.mu.Lock()
	if old, ok := ps.conns[id]; ok {
		old.close()
	}
	ps.conns[id] = &peerConn{addr: addr, session: cs, transport: tr}
	ps.mu.Unlock()
	return cs, nil
}

func (ps *peerSet) drop(id string) {
	ps.mu.Lock()
	c, ok := ps.conns[id]
	delete(ps.conns, id)
	ps.mu.Unlock()
	if ok {
		c.close()
	}
}

func (ps *peerSet) forget(id string) {
	ps.drop(id)
	ps.mu.Lock()
	delete(ps.contact, id)
	ps.mu.Unlock()
}

func (ps *peerSet) closeAll() {
	ps.mu.Lock()
	conns := ps.conns
	ps.conns = map[string]*peerConn{}
	ps.mu.Unlock()
	for _, c := range conns {
		c.close()
	}
}

// withPeer runs fn against the first candidate address that works. A
// JSON-RPC error means the peer answered, so it is returned without trying
// other addresses.
func (ps *peerSet) withPeer(ctx context.Context, id string, fn func(*mcp.ClientSession) error) (addr string, tried []string, err error) {
	tried = ps.candidates(id)
	if len(tried) == 0 {
		return "", nil, errors.New("no known address yet; it appears once discovery hears the device or after pairing")
	}
	for _, a := range tried {
		var s *mcp.ClientSession
		s, err = ps.session(ctx, id, a)
		if err == nil {
			err = fn(s)
			var rpcErr *jsonrpc.Error
			if err == nil || errors.As(err, &rpcErr) {
				if err == nil {
					now := time.Now()
					ps.markContact(id, now)
					if serr := ps.n.roster.Seen(id, a, now); serr != nil {
						ps.n.log.Warn("save peer address", "error", serr)
					}
				}
				return a, tried, err
			}
		}
		if ctx.Err() != nil {
			return "", tried, ctx.Err()
		}
		ps.n.log.Debug("peer address failed", "peer", identity.Short(id), "addr", a, "error", err)
		ps.drop(id)
	}
	return "", tried, err
}

func (ps *peerSet) call(ctx context.Context, id, tool string, args json.RawMessage, agent string) (*mcp.CallToolResult, error) {
	p, ok := ps.n.roster.Get(id)
	if !ok {
		return provider.ErrorResult("device %s is not paired", identity.Short(id)), nil
	}
	params := &mcp.CallToolParams{Name: tool, Meta: mcp.Meta{metaAgent: agent}}
	if len(args) > 0 {
		params.Arguments = args
	}
	var res *mcp.CallToolResult
	_, tried, err := ps.withPeer(ctx, id, func(s *mcp.ClientSession) error {
		var err error
		res, err = s.CallTool(ctx, params)
		return err
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var rpcErr *jsonrpc.Error
		if errors.As(err, &rpcErr) {
			return provider.ErrorResult("%s rejected %s: %s", p.Name, tool, rpcErr.Message), nil
		}
		// The SDK reports non-2xx statuses by their status text.
		if strings.Contains(err.Error(), http.StatusText(http.StatusForbidden)) {
			return provider.ErrorResult("%s refused the call: it no longer lists %s as paired (pair them again)", p.Name, ps.n.name), nil
		}
		return provider.ErrorResult("%s is unreachable (tried %s): %v", p.Name, strings.Join(tried, ", "), err), nil
	}
	return res, nil
}

// refresh fetches the peer's tool list, which also verifies it is reachable.
func (ps *peerSet) refresh(ctx context.Context, id string) error {
	var tools []*mcp.Tool
	_, _, err := ps.withPeer(ctx, id, func(s *mcp.ClientSession) error {
		tools = tools[:0]
		for t, err := range s.Tools(ctx, nil) {
			if err != nil {
				return err
			}
			tools = append(tools, t)
		}
		return nil
	})
	if err != nil {
		return err
	}
	return ps.n.roster.SetTools(id, tools, time.Now())
}

// refreshAsync starts a refresh unless one is already running for the peer.
func (ps *peerSet) refreshAsync(id string) {
	ps.mu.Lock()
	if ps.busy[id] {
		ps.mu.Unlock()
		return
	}
	ps.busy[id] = true
	ps.mu.Unlock()
	ps.n.goRun(func() {
		defer func() {
			ps.mu.Lock()
			delete(ps.busy, id)
			ps.mu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(ps.n.ctx, 10*time.Second)
		defer cancel()
		if err := ps.refresh(ctx, id); err != nil && ps.n.ctx.Err() == nil {
			ps.n.log.Debug("peer refresh failed", "peer", identity.Short(id), "error", err)
		}
	})
}

// refreshSoon retries a refresh a few times after pairing, while the other
// side may still be waiting for its user to confirm.
func (ps *peerSet) refreshSoon(id string) {
	ps.n.goRun(func() {
		for _, d := range []time.Duration{0, 2 * time.Second, 5 * time.Second, 15 * time.Second, 30 * time.Second} {
			select {
			case <-ps.n.ctx.Done():
				return
			case <-time.After(d):
			}
			ctx, cancel := context.WithTimeout(ps.n.ctx, 10*time.Second)
			err := ps.refresh(ctx, id)
			cancel()
			if err == nil {
				return
			}
		}
	})
}

// callRaw preserves transport failures so durable operations can distinguish an unanswered peer from a peer response.
func (ps *peerSet) callRaw(ctx context.Context, id, tool string, args json.RawMessage, agent string) (*mcp.CallToolResult, error) {
	if _, ok := ps.n.roster.Get(id); !ok {
		return nil, fmt.Errorf("paired peer %s was removed", identity.Short(id))
	}
	params := &mcp.CallToolParams{Name: tool, Meta: mcp.Meta{metaAgent: agent}}
	if len(args) > 0 {
		params.Arguments = args
	}
	var res *mcp.CallToolResult
	_, _, err := ps.withPeer(ctx, id, func(s *mcp.ClientSession) error {
		var callErr error
		res, callErr = s.CallTool(ctx, params)
		var rpcErr *jsonrpc.Error
		if errors.As(callErr, &rpcErr) {
			switch rpcErr.Code {
			case -32003, -32004, -32005:
				// The SDK uses these private codes for client/server closure and
				// transport rejection; they do not confirm a target tool response.
				return callErr
			default:
				return &peerCallError{err: callErr, response: true}
			}
		}
		return callErr
	})
	if err != nil {
		var callErr *peerCallError
		if errors.As(err, &callErr) {
			return res, err
		}
		return res, &peerCallError{err: err}
	}
	return res, nil
}
