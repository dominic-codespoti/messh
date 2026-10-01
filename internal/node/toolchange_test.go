package node

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/provider"
)

// dynamicProvider changes its tool set at runtime, like the service catalogue.
type dynamicProvider struct {
	mu    sync.Mutex
	tools []provider.Tool
	fn    func()
}

func (d *dynamicProvider) Name() string { return "dynamic" }
func (d *dynamicProvider) Tools() []provider.Tool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]provider.Tool(nil), d.tools...)
}
func (d *dynamicProvider) Call(context.Context, string, json.RawMessage, provider.Caller) (*mcp.CallToolResult, error) {
	return provider.JSONResult(map[string]bool{"ok": true})
}
func (d *dynamicProvider) SetOnChange(fn func()) { d.fn = fn }
func (d *dynamicProvider) add(name string) {
	d.mu.Lock()
	d.tools = append(d.tools, provider.Tool{
		Def:   &mcp.Tool{Name: name, InputSchema: json.RawMessage(`{"type":"object"}`)},
		Class: provider.ClassInfo,
	})
	d.mu.Unlock()
	d.fn()
}

func hasTool(n *Node, device, tool string) bool {
	for _, nd := range n.Nodes() {
		if nd.Name != device {
			continue
		}
		for _, t := range nd.Tools {
			if t.Name == tool {
				return true
			}
		}
	}
	return false
}

// A peer must see a new tool within seconds, not at its next probe
// (probeInterval is 45s), or a freshly registered service looks missing.
func TestPeerSeesToolChangesPromptly(t *testing.T) {
	desktop := startNode(t, "desktop")
	raspi := startNode(t, "raspi")
	pair(t, desktop, raspi)
	waitFor(t, "raspi to list desktop's tools", func() bool { return hasTool(raspi, "desktop", "node_info") })

	dp := &dynamicProvider{}
	desktop.register(dp)
	dp.add("voice__speak")
	waitFor(t, "raspi to see the new tool", func() bool { return hasTool(raspi, "desktop", "voice__speak") })
}
