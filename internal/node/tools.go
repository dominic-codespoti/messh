package node

import (
	"cmp"
	"context"
	"encoding/json"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/provider"
)

type localTool struct {
	provider provider.Provider
	tool     provider.Tool
}

// register adds a provider. Providers that implement provider.Notifier get
// their tools re-read whenever they report a change.
func (n *Node) register(p provider.Provider) {
	n.toolsMu.Lock()
	n.providers = append(n.providers, p)
	n.toolsMu.Unlock()
	if np, ok := p.(provider.Notifier); ok {
		np.SetOnChange(n.rebuildTools)
	}
}

// rebuildTools re-reads every provider's tools, updates the server peers
// call, resyncs the local agent gateway, and tells recently seen peers to
// refetch (see announceTools); others catch up at their next probe.
func (n *Node) rebuildTools() {
	n.toolsMu.Lock()
	tools := map[string]localTool{}
	var order []string
	for _, p := range n.providers {
		for _, t := range p.Tools() {
			name := t.Def.Name
			if _, dup := tools[name]; dup {
				n.log.Warn("duplicate tool name; keeping the first", "tool", name, "provider", p.Name())
				continue
			}
			tools[name] = localTool{provider: p, tool: t}
			order = append(order, name)
		}
	}
	var stale []string
	for name := range n.peerNames {
		if _, ok := tools[name]; !ok {
			stale = append(stale, name)
		}
	}
	n.tools, n.toolOrder = tools, order
	n.peerNames = make(map[string]bool, len(order))
	for _, name := range order {
		n.peerNames[name] = true
	}
	n.toolsMu.Unlock()

	if len(stale) > 0 {
		n.peerSrv.RemoveTools(stale...)
	}
	for _, name := range order {
		n.peerSrv.AddTool(tools[name].tool.Def, n.peerToolHandler(name))
	}
	if n.gateway != nil {
		n.gateway.Sync()
	}
	n.announceTools()
}

func (n *Node) lookupTool(name string) (localTool, bool) {
	n.toolsMu.RLock()
	defer n.toolsMu.RUnlock()
	lt, ok := n.tools[name]
	return lt, ok
}

func (n *Node) localDefs() []*mcp.Tool {
	n.toolsMu.RLock()
	defer n.toolsMu.RUnlock()
	defs := make([]*mcp.Tool, 0, len(n.toolOrder))
	for _, name := range n.toolOrder {
		defs = append(defs, n.tools[name].tool.Def)
	}
	return defs
}

// dispatch runs a local tool for caller. Info tools run directly; every
// other class passes the approval gate first (see gate in approval.go), so
// this is the one place where a call can start work on this device.
func (n *Node) dispatch(ctx context.Context, name string, args json.RawMessage, caller provider.Caller) *mcp.CallToolResult {
	if !n.beginWork() {
		return provider.ErrorResult("node is preparing for an update")
	}
	defer n.endWork()
	lt, ok := n.lookupTool(name)
	if !ok {
		return provider.ErrorResult("unknown tool %q on %s", name, n.name)
	}
	var g *gateState
	if lt.tool.Class != provider.ClassInfo {
		var refusal *mcp.CallToolResult
		if ctx, g, refusal = n.gate(ctx, lt, name, args, caller); refusal != nil {
			return refusal
		}
	}
	start := time.Now()
	res, err := lt.provider.Call(ctx, name, args, caller)
	took := time.Since(start)
	n.log.Info("tool call", "tool", name, "device", cmp.Or(caller.DeviceName, caller.DeviceID), "agent", caller.Agent,
		"took", took.Round(time.Millisecond), "error", err)
	g.finish(res, err, took)
	if err != nil {
		return provider.ErrorResult("%s failed on %s: %v", name, n.name, err)
	}
	return res
}
