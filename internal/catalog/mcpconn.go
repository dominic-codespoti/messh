package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const connectTimeout = 10 * time.Second

// upstreamInfo is what an MCP server reported about itself.
type upstreamInfo struct {
	Name, Version, Protocol string
}

// mcpConn keeps one MCP client session to an upstream server and replaces it
// when it fails. Legacy servers (session id in a header, SSE-framed replies)
// work through the SDK's streamable client; the standalone server-push stream
// is disabled because nothing here consumes server-initiated messages and
// some servers mishandle the GET.
type mcpConn struct {
	url    string
	client *http.Client
	ctx    context.Context // lives as long as the provider, outlives single calls

	mu   sync.Mutex
	sess *mcp.ClientSession
	info upstreamInfo
}

func newMCPConn(ctx context.Context, url string, client *http.Client) *mcpConn {
	return &mcpConn{url: url, client: client, ctx: ctx}
}

// session returns the pooled session, connecting when there is none.
func (c *mcpConn) session(ctx context.Context) (*mcp.ClientSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sess != nil {
		return c.sess, nil
	}
	cctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	// No capabilities: the SDK would otherwise advertise roots, and a server
	// that sees it sends roots/list as a server-to-client request. That needs
	// the standalone stream this client disables, so the server would wait
	// for an answer that cannot come (Playwright MCP stalls 60 s on its first
	// tool call).
	cl := mcp.NewClient(&mcp.Implementation{Name: "messh", Version: "0.1"},
		&mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}})
	sess, err := cl.Connect(cctx, &mcp.StreamableClientTransport{
		Endpoint:             c.url,
		HTTPClient:           c.client,
		MaxRetries:           -1,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		return nil, err
	}
	c.sess = sess
	c.info = upstreamInfo{}
	if ir := sess.InitializeResult(); ir != nil {
		c.info.Protocol = ir.ProtocolVersion
		if ir.ServerInfo != nil {
			c.info.Name, c.info.Version = ir.ServerInfo.Name, ir.ServerInfo.Version
		}
	}
	return sess, nil
}

// drop forgets sess (if it is still the pooled one) and closes it in the background.
func (c *mcpConn) drop(sess *mcp.ClientSession) {
	c.mu.Lock()
	if c.sess == sess {
		c.sess = nil
	}
	c.mu.Unlock()
	if sess != nil {
		go sess.Close()
	}
}

func (c *mcpConn) identity() upstreamInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.info
}

// listTools fetches every upstream tool, dropping the session on failure.
func (c *mcpConn) listTools(ctx context.Context) ([]*mcp.Tool, error) {
	sess, err := c.session(ctx)
	if err != nil {
		return nil, err
	}
	var tools []*mcp.Tool
	for t, err := range sess.Tools(ctx, nil) {
		if err != nil {
			if !isProtocolError(err) {
				c.drop(sess)
			}
			return nil, err
		}
		tools = append(tools, t)
	}
	return tools, nil
}

// call forwards one tool call. A dead session is replaced and the call
// retried once only when the request cannot have reached the server (or the
// server says the session is gone); a lost reply is never replayed, because
// the tool may have acted.
func (c *mcpConn) call(ctx context.Context, name string, args json.RawMessage) (*mcp.CallToolResult, error) {
	for attempt := 0; ; attempt++ {
		sess, err := c.session(ctx)
		if err == nil {
			var res *mcp.CallToolResult
			res, err = sess.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
			if err == nil {
				return res, nil
			}
			if isProtocolError(err) {
				return nil, err
			}
			if ctx.Err() == nil { // a cancelled call says nothing about the session
				c.drop(sess)
			}
		}
		if attempt == 0 && ctx.Err() == nil && neverSent(err) {
			continue
		}
		return nil, err
	}
}

func (c *mcpConn) close() {
	c.mu.Lock()
	sess := c.sess
	c.sess = nil
	c.mu.Unlock()
	if sess == nil {
		return
	}
	done := make(chan struct{})
	go func() { sess.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
}

// isProtocolError reports a JSON-RPC error reply: the server is alive and answered.
func isProtocolError(err error) bool {
	var je *jsonrpc.Error
	return errors.As(err, &je)
}

// neverSent reports failures after which the request provably or very
// probably did not run: no session could be established, the session was
// already closed or unknown to the server.
func neverSent(err error) bool {
	if errors.Is(err, mcp.ErrSessionMissing) || errors.Is(err, mcp.ErrConnectionClosed) {
		return true
	}
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// describeErr turns transport errors into a short sentence for agents.
func describeErr(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "timed out"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err // the URL is configuration, not something to hand to agents
	}
	return fmt.Sprint(err)
}
