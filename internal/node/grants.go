package node

import (
	"context"
	"encoding/json"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"messh/internal/grants"
	"messh/internal/provider"
	"net/http"
	"time"
)

func (n *Node) startGrants() error {
	s, e := grants.New(n.paths.GrantsFile())
	if e != nil {
		return e
	}
	n.grants = s
	n.register(&capabilityProvider{store: s, node: n})
	return nil
}
func (n *Node) registerGrantAPI(api *http.ServeMux) {
	api.HandleFunc("GET /v1/grants", n.apiGrants)
	api.HandleFunc("POST /v1/grants", n.apiGrantCreate)
	api.HandleFunc("DELETE /v1/grants/{id}", n.apiGrantRevoke)
}
func (n *Node) apiGrants(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, n.grants.List())
}
func (n *Node) apiGrantCreate(w http.ResponseWriter, r *http.Request) {
	var g grants.Grant
	if !readJSON(w, r, &g) {
		return
	}
	g.ID = ""
	g.CreatedAt = time.Time{}
	g.RevokedAt = nil
	x, e := n.grants.Create(g)
	if e != nil {
		writeError(w, http.StatusBadRequest, e.Error())
		return
	}
	writeJSON(w, http.StatusCreated, x)
}
func (n *Node) apiGrantRevoke(w http.ResponseWriter, r *http.Request) {
	g, e := n.grants.Revoke(r.PathValue("id"))
	if e != nil {
		writeError(w, http.StatusNotFound, "capability grant not found")
		return
	}
	writeJSON(w, http.StatusOK, g)
}

type capabilityProvider struct {
	store *grants.Store
	node  *Node
}

func (p *capabilityProvider) Name() string { return "capabilities" }

type capabilityCheck struct {
	Kind   string          `json:"kind"`
	Tool   string          `json:"tool"`
	Args   json.RawMessage `json:"args"`
	Path   string          `json:"path"`
	Action string          `json:"action"`
}
type capabilityCheckResult struct {
	Decision grants.Decision `json:"decision"`
	ArgsHash string          `json:"args_hash,omitempty"`
	Note     string          `json:"note,omitempty"`
}

func (p *capabilityProvider) Tools() []provider.Tool {
	return []provider.Tool{
		{Class: provider.ClassInfo, Def: &mcp.Tool{Name: "capability_list", Description: "List this caller's finite grants only; device trust policies are not listed.", InputSchema: json.RawMessage(`{"type":"object","additionalProperties":false}`)}},
		{Class: provider.ClassInfo, Def: &mcp.Tool{Name: "capability_check", Description: "Preview the effective grant or trusted-device authorization. Tool checks prepare the native provider request without executing it; other owner approval policy can still apply.", InputSchema: json.RawMessage(`{"type":"object","properties":{"kind":{"type":"string","enum":["tool","file"]},"tool":{"type":"string"},"args":{},"path":{"type":"string"},"action":{"type":"string","enum":["read","write"]}},"required":["kind"],"additionalProperties":false}`)}}}
}
func (p *capabilityProvider) Call(ctx context.Context, tool string, raw json.RawMessage, caller provider.Caller) (*mcp.CallToolResult, error) {
	sub := grants.Subject{DeviceID: caller.DeviceID, Agent: caller.Agent}
	if tool == "capability_list" {
		all := p.store.List()
		own := make([]grants.Grant, 0)
		for _, g := range all {
			if g.Subject == sub {
				own = append(own, g)
			}
		}
		return provider.JSONResult(own)
	}
	var in capabilityCheck
	if e := json.Unmarshal(raw, &in); e != nil {
		return provider.ErrorResult("invalid capability check request"), nil
	}
	req := grants.Request{Kind: in.Kind, Path: in.Path, Action: in.Action}
	out := capabilityCheckResult{}
	if in.Kind == "tool" {
		if in.Tool == "" || len(in.Args) == 0 {
			return provider.ErrorResult("tool and args are required"), nil
		}
		lt, ok := p.node.lookupTool(in.Tool)
		if !ok {
			return provider.ErrorResult("unknown tool"), nil
		}
		g, ok := lt.provider.(provider.Gated)
		if !ok {
			return provider.ErrorResult("tool does not have a native approval key"), nil
		}
		ap, e := g.Approval(ctx, in.Tool, in.Args, caller)
		if e != nil {
			return provider.ErrorResult("tool request could not be prepared: %v", e), nil
		}
		req.Tool = in.Tool
		req.ArgsHash = ap.Exact
		out.ArgsHash = ap.Exact
		out.Note = "Authorization preview only; other owner approval policies may still apply."
		if p.node.trust.Allows(caller.DeviceID) {
			out.Decision = grants.Decision{Allowed: true, Code: "trusted_device"}
			return provider.JSONResult(out)
		}
	} else if in.Kind != "file" {
		return provider.ErrorResult("kind must be tool or file"), nil
	} else {
		out.Decision = p.node.fileGrantDecision(caller.DeviceID, caller.Agent, in.Path, in.Action)
		return provider.JSONResult(out)
	}
	out.Decision = p.store.Decide(sub, req)
	return provider.JSONResult(out)
}
