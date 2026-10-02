// Package gateway is the MCP endpoint local agents connect to. It exposes
// every paired device's tools under one server, either directly, namespaced
// by device (full mode), or behind mesh_tools/mesh_call (compact mode).
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/identity"
	"messh/internal/provider"
)

// Separator joins a device handle and a tool name, e.g. "desktop__node_info".
// MCP clients feed tool names to model APIs that only accept [A-Za-z0-9_-].
const Separator = "__"

const maxToolName = 64

// Node describes one device as the gateway sees it.
type Node struct {
	ID       string
	Name     string
	Self     bool
	Online   bool
	LastSeen time.Time
	Addr     string
	Asleep   bool // peer announced it is going to sleep; probes suppressed until heard again
	Tools    []*mcp.Tool
}

// Backend is what the gateway needs from the node runtime.
type Backend interface {
	// Nodes returns this device first, then paired peers.
	Nodes() []Node
	// Call runs tool on the device with the given ID on behalf of agent.
	Call(ctx context.Context, deviceID, tool string, args json.RawMessage, agent string) (*mcp.CallToolResult, error)
}

// Tool modes select which server an agent talks to.
const (
	// ModeCompact lists only mesh-level tools; device tools are found with
	// mesh_tools and run with mesh_call, keeping the agent's context small.
	ModeCompact = "compact"
	// ModeFull lists every device tool as <device>__<tool>.
	ModeFull = "full"
)

// Gateway owns the agent-facing MCP servers, one per tool mode. Both serve
// the same routing state, so Sync updates both.
type Gateway struct {
	backend Backend
	full    *mcp.Server
	compact *mcp.Server
	log     *slog.Logger

	syncMu  sync.Mutex // serializes Sync so concurrent rebuilds cannot interleave
	mu      sync.Mutex
	tools   map[string]route // full-mode exposed name → target
	devices []device         // compact-mode catalogue: self first, then by handle
}

type route struct {
	deviceID string
	tool     string
}

// device is one node's callable tools as of the last Sync, under their own
// (unprefixed) names, sorted by name.
type device struct {
	id, name, handle string
	self             bool
	tools            []*mcp.Tool
}

// New creates a gateway; call Sync whenever the node set or tool lists change.
func New(b Backend, version string, log *slog.Logger) *Gateway {
	impl := &mcp.Implementation{Name: "messh", Version: version}
	caps := &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}}
	caps.AddExtension(tasksExtension, map[string]any{})
	g := &Gateway{
		backend: b,
		log:     log,
		tools:   map[string]route{},
		full: mcp.NewServer(impl, &mcp.ServerOptions{
			Instructions: "messh connects the devices on this LAN. Tools are named <device>" + Separator +
				"<tool>; call mesh_nodes to see which devices exist, whether they are online, and what each offers.",
			Capabilities: caps,
		}),
		compact: mcp.NewServer(impl, &mcp.ServerOptions{
			Instructions: compactInstructions,
			Capabilities: caps,
		}),
	}
	g.full.AddTool(&mcp.Tool{
		Name:        "mesh_nodes",
		Description: "List the devices on this messh mesh: name, ID, online status, last contact, and the tools each exposes.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, Title: "Mesh devices"},
	}, g.meshNodes)
	g.installTasks()
	g.addCompactTools()
	g.Sync()
	return g
}

// AddTool exposes a tool that belongs to the node as a whole rather than to
// one device, next to mesh_nodes, in both tool modes. Name it without the
// device separator so it cannot collide with a forwarded <device>__<tool> name.
func (g *Gateway) AddTool(def *mcp.Tool, h mcp.ToolHandler) {
	g.full.AddTool(def, h)
	g.compact.AddTool(def, h)
}

type serverKey struct{}

// Handler serves MCP for agents authenticated by verify. modeOf maps the
// authenticated agent to its tool mode (ModeFull, anything else is compact);
// it is consulted on every request so a mode change applies to the next one.
func (g *Gateway) Handler(verify auth.TokenVerifier, modeOf func(agent string) string) http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		s, _ := r.Context().Value(serverKey{}).(*mcp.Server)
		return s
	}, &mcp.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
	})
	// The SDK asks for the server more than once per request; pick it once here.
	pick := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var agent string
		if ti := auth.TokenInfoFromContext(r.Context()); ti != nil {
			agent = ti.UserID
		}
		s := g.compact
		if modeOf(agent) == ModeFull {
			s = g.full
		}
		h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), serverKey{}, s)))
	})
	return auth.RequireBearerToken(verify, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(pick)
}

// Sync recomputes the exposed tool set from the backend for both modes.
func (g *Gateway) Sync() {
	g.syncMu.Lock()
	defer g.syncMu.Unlock()
	nodes := g.backend.Nodes()
	handles := Handles(nodes)

	next := map[string]route{}
	defs := map[string]*mcp.Tool{}
	devices := make([]device, 0, len(nodes))
	for _, n := range nodes {
		h := handles[n.ID]
		d := device{id: n.ID, name: n.Name, handle: h, self: n.Self}
		for _, t := range n.Tools {
			if t.Name == "" {
				g.log.Warn("skipping tool with an empty name", "device", n.Name)
				continue
			}
			def := *t
			if def.InputSchema == nil {
				def.InputSchema = json.RawMessage(`{"type":"object"}`)
			}
			if !objectSchema(def.InputSchema) {
				g.log.Warn("skipping tool whose input schema is not an object", "device", n.Name, "tool", t.Name)
				continue
			}
			d.tools = append(d.tools, &def)

			name := h + Separator + t.Name
			if len(name) > maxToolName {
				g.log.Warn("tool name too long for full mode; reachable via mesh_call only", "device", n.Name, "tool", t.Name)
				continue
			}
			full := def
			full.Name = name
			full.Description = fmt.Sprintf("[on %s] %s", n.Name, t.Description)
			next[name] = route{deviceID: n.ID, tool: t.Name}
			defs[name] = &full
		}
		sort.SliceStable(d.tools, func(i, j int) bool { return d.tools[i].Name < d.tools[j].Name })
		devices = append(devices, d)
	}
	sort.SliceStable(devices, func(i, j int) bool {
		if devices[i].self != devices[j].self {
			return devices[i].self
		}
		return devices[i].handle < devices[j].handle
	})

	g.mu.Lock()
	var stale []string
	for name := range g.tools {
		if _, ok := next[name]; !ok {
			stale = append(stale, name)
		}
	}
	g.tools = next
	g.devices = devices
	g.mu.Unlock()

	if len(stale) > 0 {
		g.full.RemoveTools(stale...)
	}
	for name, def := range defs {
		g.full.AddTool(def, g.forward(name))
	}
}

func (g *Gateway) forward(name string) mcp.ToolHandler {
	return func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		g.mu.Lock()
		r, ok := g.tools[name]
		g.mu.Unlock()
		if !ok {
			return provider.ErrorResult("tool %s is no longer available; call mesh_nodes", name), nil
		}
		return g.backend.Call(ctx, r.deviceID, r.tool, req.Params.Arguments, agentOf(req))
	}
}

func (g *Gateway) meshNodes(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	type toolOut struct {
		Name        string `json:"name"`
		Description string `json:"description,omitempty"`
	}
	type nodeOut struct {
		Handle   string    `json:"handle"`
		Name     string    `json:"name"`
		ID       string    `json:"id"`
		Self     bool      `json:"self,omitempty"`
		Online   bool      `json:"online"`
		LastSeen time.Time `json:"last_seen,omitzero"`
		Addr     string    `json:"addr,omitempty"`
		Asleep   bool      `json:"asleep,omitempty"`
		Tools    []toolOut `json:"tools"`
	}
	nodes := g.backend.Nodes()
	handles := Handles(nodes)
	out := make([]nodeOut, 0, len(nodes))
	for _, n := range nodes {
		o := nodeOut{Handle: handles[n.ID], Name: n.Name, ID: n.ID, Self: n.Self, Online: n.Online,
			LastSeen: n.LastSeen, Addr: n.Addr, Asleep: n.Asleep, Tools: []toolOut{}}
		for _, t := range n.Tools {
			o.Tools = append(o.Tools, toolOut{Name: handles[n.ID] + Separator + t.Name, Description: t.Description})
		}
		out = append(out, o)
	}
	return provider.JSONResult(out)
}

func agentOf(req *mcp.CallToolRequest) string {
	if req.Extra != nil && req.Extra.TokenInfo != nil {
		return req.Extra.TokenInfo.UserID
	}
	return ""
}

var handleUnsafe = regexp.MustCompile(`[^a-z0-9-]+`)

// Handles assigns each node a tool-name-safe handle derived from its name,
// disambiguating duplicates with a short ID suffix.
func Handles(nodes []Node) map[string]string {
	base := map[string]string{}
	count := map[string]int{}
	for _, n := range nodes {
		h := strings.Trim(handleUnsafe.ReplaceAllString(strings.ToLower(n.Name), "-"), "-")
		if h == "" {
			h = "device"
		}
		if len(h) > 24 {
			h = strings.TrimRight(h[:24], "-")
		}
		base[n.ID] = h
		count[h]++
	}
	out := make(map[string]string, len(nodes))
	for _, n := range nodes {
		h := base[n.ID]
		if count[h] > 1 {
			h += "-" + identity.Short(n.ID)[:4]
		}
		out[n.ID] = h
	}
	return out
}

// objectSchema reports whether schema is a JSON object with "type":"object";
// mcp.Server.AddTool panics on anything else.
func objectSchema(schema any) bool {
	data, err := json.Marshal(schema)
	if err != nil {
		return false
	}
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return false
	}
	return m["type"] == "object"
}
