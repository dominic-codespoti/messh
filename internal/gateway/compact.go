package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/provider"
)

const compactInstructions = "messh connects the devices on this LAN. Call mesh_nodes to see the devices, whether they " +
	"are online, and the names of their tools; mesh_tools with a query to read matching tools' descriptions and " +
	"input schemas; then mesh_call to run one of them on its device."

const (
	defaultToolsLimit = 20
	maxToolsLimit     = 100
)

// addCompactTools registers the compact server's own mesh tools.
func (g *Gateway) addCompactTools() {
	g.compact.AddTool(&mcp.Tool{
		Name: "mesh_nodes",
		Description: "List the devices on this messh mesh: handle, name, ID, online status, last contact, and the " +
			"names of the tools each offers. Next, call mesh_tools with a query to read a tool's description and " +
			"input schema, then mesh_call to run it.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, Title: "Mesh devices"},
	}, g.meshNodesCompact)
	g.compact.AddTool(&mcp.Tool{
		Name: "mesh_tools",
		Description: "Search the tools the devices on this messh mesh offer and return their descriptions and input " +
			"schemas. Workflow: call mesh_nodes to see the devices and their tool names, then mesh_tools with a query " +
			"(for example \"job\", \"speech\", \"browser\", \"file\") to learn how to call the tools you need, then " +
			"mesh_call with the device and tool name returned here. Leave device empty to search every device. The " +
			"query matches tool names and descriptions case-insensitively; every word of it must appear. Tools whose " +
			"name matches the query come first, then those matching only in their description. Returns at most " +
			"limit tools (default 20, max 100) and how many matched in total.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"device":{"type":"string","description":"Device handle, name or ID from mesh_nodes. Empty searches every device."},` +
			`"query":{"type":"string","description":"Words to look for in tool names and descriptions. Empty lists every tool."},` +
			`"limit":{"type":"integer","minimum":1,"maximum":100,"description":"Maximum number of tools to return (default 20)."}` +
			`},"additionalProperties":false}`),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, Title: "Search mesh tools"},
	}, g.meshTools)
	g.compact.AddTool(&mcp.Tool{
		Name: "mesh_call",
		Description: "Run a tool on a device of this messh mesh and return that tool's own result. Workflow: call " +
			"mesh_nodes to pick the device, mesh_tools to get the tool's name and input schema, then mesh_call with " +
			"device (handle, name or ID from mesh_nodes), tool (the name exactly as mesh_tools lists it) and " +
			"arguments (an object matching that tool's input schema). The target device's approval rules apply " +
			"exactly as for any other call of that tool.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"device":{"type":"string","description":"Device handle, name or ID from mesh_nodes."},` +
			`"tool":{"type":"string","description":"Tool name as listed by mesh_tools, without a device prefix."},` +
			`"arguments":{"type":"object","description":"Arguments for the tool, matching its input schema. Omit for none."}` +
			`},"required":["device","tool"],"additionalProperties":false}`),
		Annotations: &mcp.ToolAnnotations{Title: "Run a mesh tool"},
	}, g.meshCall)
}

func (g *Gateway) snapshot() []device {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.devices // Sync replaces the slice wholesale and never mutates it
}

// meshNodesCompact is mesh_nodes without descriptions or addresses: just
// enough for an agent to pick a device and a tool name.
func (g *Gateway) meshNodesCompact(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	type nodeOut struct {
		Handle   string    `json:"handle"`
		Name     string    `json:"name"`
		ID       string    `json:"id"`
		Self     bool      `json:"self,omitempty"`
		Online   bool      `json:"online"`
		Asleep   bool      `json:"asleep,omitempty"`
		LastSeen time.Time `json:"last_seen,omitzero"`
		Tools    []string  `json:"tools"`
	}
	names := map[string][]string{}
	for _, d := range g.snapshot() {
		list := make([]string, 0, len(d.tools))
		for _, t := range d.tools {
			list = append(list, t.Name)
		}
		names[d.id] = list
	}
	nodes := g.backend.Nodes()
	handles := Handles(nodes)
	out := make([]nodeOut, 0, len(nodes))
	for _, n := range nodes {
		tools := names[n.ID]
		if tools == nil {
			tools = []string{}
		}
		out = append(out, nodeOut{Handle: handles[n.ID], Name: n.Name, ID: n.ID, Self: n.Self, Online: n.Online,
			Asleep: n.Asleep, LastSeen: n.LastSeen, Tools: tools})
	}
	return provider.JSONResult(out)
}

type toolInfo struct {
	Device      string               `json:"device"`
	Name        string               `json:"name"`
	Description string               `json:"description,omitempty"`
	InputSchema any                  `json:"input_schema"`
	Annotations *mcp.ToolAnnotations `json:"annotations,omitempty"`
}

type toolsResult struct {
	Matched  int        `json:"matched"`
	Returned int        `json:"returned"`
	Tools    []toolInfo `json:"tools"`
	Note     string     `json:"note,omitempty"`
}

func (g *Gateway) meshTools(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Device string `json:"device"`
		Query  string `json:"query"`
		Limit  int    `json:"limit"`
	}
	if err := decodeArgs(req, &a); err != nil {
		return provider.ErrorResult("mesh_tools: invalid arguments: %v", err), nil
	}
	limit := a.Limit
	switch {
	case limit < 0:
		return provider.ErrorResult("mesh_tools: limit must be between 1 and %d", maxToolsLimit), nil
	case limit == 0:
		limit = defaultToolsLimit
	case limit > maxToolsLimit:
		limit = maxToolsLimit
	}
	devices := g.snapshot()
	if a.Device != "" {
		d, err := resolve(devices, a.Device)
		if err != nil {
			return provider.ErrorResult("mesh_tools: %v", err), nil
		}
		devices = []device{d}
	}
	terms := strings.Fields(strings.ToLower(a.Query))

	type hit struct {
		rank int
		info toolInfo
	}
	var hits []hit
	for _, d := range devices {
		for _, t := range d.tools {
			r, ok := matchRank(t, terms)
			if !ok {
				continue
			}
			hits = append(hits, hit{r, toolInfo{Device: d.handle, Name: t.Name, Description: t.Description,
				InputSchema: t.InputSchema, Annotations: t.Annotations}})
		}
	}
	// Stable, so each tier keeps the catalogue order: this device first, then
	// devices by handle, then tool name.
	slices.SortStableFunc(hits, func(x, y hit) int { return x.rank - y.rank })
	res := toolsResult{Matched: len(hits), Tools: []toolInfo{}}
	for _, h := range hits[:min(limit, len(hits))] {
		res.Tools = append(res.Tools, h.info)
	}
	res.Returned = len(res.Tools)
	switch {
	case res.Matched == 0 && len(terms) > 0:
		res.Note = "no tool matched; try fewer or shorter words, or omit query to list every tool"
	case res.Matched == 0:
		res.Note = "no tools on the selected device(s); call mesh_nodes to see which devices offer tools"
	case res.Matched > res.Returned:
		res.Note = fmt.Sprintf("%d more matched; narrow the query or device, or raise limit (max %d)",
			res.Matched-res.Returned, maxToolsLimit)
	}
	return provider.JSONResult(res)
}

// matchRank reports whether every term appears in t's name or description
// and, if so, how relevant t is, lower first: 0 when the name equals a term or
// the whole query, 1 when every term is in the name, 2 when some are, and 3
// when the terms match only the description. No terms match everything at 0.
func matchRank(t *mcp.Tool, terms []string) (int, bool) {
	if len(terms) == 0 {
		return 0, true
	}
	name := strings.ToLower(t.Name)
	desc := strings.ToLower(t.Description)
	inName := 0
	for _, term := range terms {
		switch {
		case strings.Contains(name, term):
			inName++
		case !strings.Contains(desc, term):
			return 0, false
		}
	}
	switch {
	case name == strings.Join(terms, " ") || slices.Contains(terms, name):
		return 0, true
	case inName == len(terms):
		return 1, true
	case inName > 0:
		return 2, true
	}
	return 3, true
}

func (g *Gateway) meshCall(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		Device    string          `json:"device"`
		Tool      string          `json:"tool"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := decodeArgs(req, &a); err != nil {
		return provider.ErrorResult("mesh_call: invalid arguments: %v", err), nil
	}
	if a.Device == "" || a.Tool == "" {
		return provider.ErrorResult("mesh_call: device and tool are required; call mesh_nodes for devices and mesh_tools for tool names"), nil
	}
	args := bytes.TrimSpace(a.Arguments)
	if len(args) == 0 || bytes.Equal(args, []byte("null")) {
		args = []byte("{}")
	} else if args[0] != '{' {
		return provider.ErrorResult("mesh_call: arguments must be a JSON object matching the tool's input schema"), nil
	}

	d, err := resolve(g.snapshot(), a.Device)
	if err != nil {
		return provider.ErrorResult("mesh_call: %v", err), nil
	}
	var tool *mcp.Tool
	for _, t := range d.tools {
		if t.Name == a.Tool {
			tool = t
			break
		}
	}
	if tool == nil {
		return provider.ErrorResult("mesh_call: device %s has no tool %q; call mesh_tools with device %q (and a query) "+
			"to list its tools, or mesh_nodes to see every device's tool names", d.handle, a.Tool, d.handle), nil
	}
	res, err := g.backend.Call(ctx, d.id, tool.Name, json.RawMessage(args), agentOf(req))
	if err != nil {
		return provider.ErrorResult("%s on %s: %v", tool.Name, d.handle, err), nil
	}
	if res == nil {
		return provider.ErrorResult("%s on %s returned no result", tool.Name, d.handle), nil
	}
	return res, nil
}

// decodeArgs unmarshals a call's arguments into v; absent arguments leave v
// at its zero value.
func decodeArgs(req *mcp.CallToolRequest, v any) error {
	if req.Params == nil {
		return nil
	}
	raw := bytes.TrimSpace(req.Params.Arguments)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// resolve maps what an agent typed (a handle, a name, the ID, or a unique ID
// prefix of at least 4 characters) to a device, trying each form in turn.
func resolve(devices []device, ref string) (device, error) {
	lower := strings.ToLower(strings.TrimSpace(ref))
	tiers := []func(device) bool{
		func(d device) bool { return d.handle == lower },
		func(d device) bool { return strings.EqualFold(d.name, ref) },
		func(d device) bool {
			id := strings.ToLower(d.id)
			return id == lower || (len(lower) >= 4 && strings.HasPrefix(id, lower))
		},
	}
	for _, match := range tiers {
		var found []device
		for _, d := range devices {
			if match(d) {
				found = append(found, d)
			}
		}
		switch len(found) {
		case 0:
		case 1:
			return found[0], nil
		default:
			return device{}, fmt.Errorf("%q matches %d devices; use the handle from mesh_nodes", ref, len(found))
		}
	}
	have := make([]string, 0, len(devices))
	for _, d := range devices {
		have = append(have, d.handle)
	}
	return device{}, fmt.Errorf("no device %q on this mesh (have: %s); call mesh_nodes", ref, strings.Join(have, ", "))
}
