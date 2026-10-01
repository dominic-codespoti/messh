package catalog

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Conn is the catalogue's pooled MCP client connection, exported so other
// providers that front an MCP server (the browser provider fronts Playwright
// MCP) share its legacy-session and SSE handling and its retry rules.
type Conn struct{ c *mcpConn }

// NewConn returns a connection to the streamable-HTTP MCP endpoint at url.
// ctx should live as long as the owner of the connection. A nil client uses
// http.DefaultClient.
func NewConn(ctx context.Context, url string, client *http.Client) *Conn {
	if client == nil {
		client = http.DefaultClient
	}
	c := newMCPConn(ctx, url, client)
	return &Conn{c: c}
}

// Session returns the pooled session, connecting when there is none.
func (c *Conn) Session(ctx context.Context) (*mcp.ClientSession, error) { return c.c.session(ctx) }

// Drop forgets sess and closes it in the background.
func (c *Conn) Drop(sess *mcp.ClientSession) { c.c.drop(sess) }

// Identity reports what the upstream server called itself at connect time.
func (c *Conn) Identity() (name, version, protocol string) {
	i := c.c.identity()
	return i.Name, i.Version, i.Protocol
}

// ListTools fetches every upstream tool.
func (c *Conn) ListTools(ctx context.Context) ([]*mcp.Tool, error) { return c.c.listTools(ctx) }

// Call forwards one tool call; see mcpConn.call for when it is retried.
func (c *Conn) Call(ctx context.Context, name string, args json.RawMessage) (*mcp.CallToolResult, error) {
	return c.c.call(ctx, name, args)
}

// Close ends the session.
func (c *Conn) Close() { c.c.close() }

// IsProtocolError reports a JSON-RPC error reply: the server is alive and answered.
func IsProtocolError(err error) bool { return isProtocolError(err) }

// DescribeErr turns a transport error into a short sentence for agents.
func DescribeErr(err error) string { return describeErr(err) }
