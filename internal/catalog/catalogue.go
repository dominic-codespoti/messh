package catalog

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/provider"
)

type catalogueArgs struct {
	Tag  string `json:"tag"`
	Kind string `json:"kind"`
}

// serviceInfo is one catalogue row. It carries no URL, header or credential.
type serviceInfo struct {
	Name        string         `json:"name"`
	Kind        string         `json:"kind"`
	Description string         `json:"description,omitempty"`
	Tags        []string       `json:"tags,omitempty"`
	Up          bool           `json:"up"`
	LatencyMS   *int64         `json:"latency_ms,omitempty"`
	CheckedAgo  string         `json:"checked_ago,omitempty"`
	LastError   string         `json:"last_error,omitempty"`
	Use         map[string]any `json:"use"`
	Facts       map[string]any `json:"facts,omitempty"`
}

func (p *Provider) catalogue(args json.RawMessage) (*mcp.CallToolResult, error) {
	var a catalogueArgs
	if err := decodeArgs(args, &a); err != nil {
		return provider.ErrorResult("%v", err), nil
	}
	p.mu.RLock()
	names := make([]string, 0, len(p.entries))
	for n := range p.entries {
		names = append(names, n)
	}
	cfgErrs := slices.Clone(p.cfgErrs)
	entries := make([]*entry, 0, len(names))
	sort.Strings(names)
	for _, n := range names {
		entries = append(entries, p.entries[n])
	}
	p.mu.RUnlock()

	services := []serviceInfo{}
	for _, e := range entries {
		s := e.snapshot()
		if a.Kind != "" && s.cfg.Kind != a.Kind {
			continue
		}
		if a.Tag != "" && !slices.ContainsFunc(s.cfg.Tags, func(t string) bool { return strings.EqualFold(t, a.Tag) }) {
			continue
		}
		services = append(services, describeService(s))
	}
	out := map[string]any{"services": services}
	if len(services) == 0 && a.Tag == "" && a.Kind == "" {
		out["note"] = "no services are registered on this device (the owner adds them with `messh service add`)"
	}
	if len(cfgErrs) > 0 {
		out["config_errors"] = cfgErrs
	}
	return provider.JSONResult(out)
}

func describeService(s snap) serviceInfo {
	info := serviceInfo{
		Name: s.cfg.Name, Kind: s.cfg.Kind, Description: s.cfg.Description, Tags: s.cfg.Tags,
		Up: s.h.Up, LastError: s.h.LastError, Use: map[string]any{}, Facts: map[string]any{},
	}
	if !s.h.Checked.IsZero() {
		ms := s.h.Latency.Milliseconds()
		if s.h.Up {
			info.LatencyMS = &ms
		}
		info.CheckedAgo = time.Since(s.h.Checked).Round(time.Second).String()
	} else {
		info.LastError = "not probed yet"
	}
	switch s.cfg.Kind {
	case KindMCP:
		tools := make([]string, 0, len(s.proxies))
		for _, pt := range s.proxies {
			tools = append(tools, pt.Name)
		}
		if s.h.Up {
			info.Use["tools"] = tools
			info.Use["how"] = "call the listed tools directly (prefix them with this device's handle and __)"
		}
		if s.upstream.Name != "" {
			info.Facts["upstream"] = strings.TrimSpace(s.upstream.Name + " " + s.upstream.Version)
		}
		if s.upstream.Protocol != "" {
			info.Facts["mcp_protocol"] = s.upstream.Protocol
		}
		info.Facts["tool_count"] = len(s.proxies)
	case KindOpenAPI:
		info.Use["via"] = []string{toolDescribe, toolCall}
		info.Use["how"] = "service_describe {service, query} to find operations, then service_call {service, method, path, ...}"
		if s.doc != nil {
			info.Facts["api"] = strings.TrimSpace(s.doc.Title + " " + s.doc.Version)
			info.Facts["operations"] = len(s.doc.Ops)
		}
	case KindOpenAI:
		info.Use["via"] = []string{toolModels, toolChat}
		info.Use["how"] = "openai_models {service} to list models, openai_chat {service, model, messages}"
	case KindHTTP:
		info.Use["via"] = []string{toolCall}
		info.Use["how"] = "service_call {service, method, path, ...} (no API description is available)"
	}
	if len(info.Facts) == 0 {
		info.Facts = nil
	}
	return info
}

type describeArgs struct {
	Service string `json:"service"`
	Query   string `json:"query"`
	Tag     string `json:"tag"`
	Offset  int    `json:"offset"`
	Limit   int    `json:"limit"`
	Detail  *bool  `json:"detail"`
}

func (p *Provider) planDescribe(args json.RawMessage) (*plan, error) {
	var a describeArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	e, err := p.lookup(a.Service)
	if err != nil {
		return nil, err
	}
	if e.cfg.Kind != KindOpenAPI {
		return nil, fmt.Errorf("service %s is kind %s: it has no OpenAPI document (see the catalogue for how to use it)", e.cfg.Name, e.cfg.Kind)
	}
	if a.Offset < 0 {
		return nil, fmt.Errorf("offset must not be negative")
	}
	if a.Limit < 0 || a.Limit > 50 {
		return nil, fmt.Errorf("limit must be between 1 and 50")
	}
	pl := &plan{
		p: p, e: e, svc: e.cfg, tool: toolDescribe, op: toolDescribe, readOnly: true,
		exact:     map[string]any{"query": a.Query, "tag": a.Tag, "offset": a.Offset, "limit": a.Limit, "detail": a.Detail != nil && *a.Detail},
		autoMatch: func(entry string) bool { return entry == toolDescribe },
	}
	if a.Query != "" {
		pl.details = append(pl.details, provider.Detail{Label: "Query", Value: clip(a.Query, 120)})
	}
	pl.run = func(ctx context.Context) (*mcp.CallToolResult, error) { return p.runDescribe(e, a) }
	return pl, nil
}

func (p *Provider) runDescribe(e *entry, a describeArgs) (*mcp.CallToolResult, error) {
	s := e.snapshot()
	if s.doc == nil {
		msg := "the service's OpenAPI document has not been fetched"
		if s.h.LastError != "" {
			msg += ": " + s.h.LastError
		}
		return provider.ErrorResult("service %s: %s", e.cfg.Name, msg), nil
	}
	limit := a.Limit
	if limit == 0 {
		limit = 20
	}
	var matches []*apiOp
	for _, op := range s.doc.search(a.Query, a.Tag) {
		if permitsREST(&s.cfg, op.Method, op.Path) {
			matches = append(matches, op)
		}
	}
	start := min(a.Offset, len(matches))
	end := min(start+limit, len(matches))
	detail := len(matches) <= 8
	if a.Detail != nil {
		detail = *a.Detail
	}
	views := make([]opView, 0, end-start)
	for _, op := range matches[start:end] {
		if detail {
			views = append(views, s.doc.detail(op))
		} else {
			views = append(views, op.summary())
		}
	}
	out := map[string]any{
		"service":          e.cfg.Name,
		"api":              strings.TrimSpace(s.doc.Title + " " + s.doc.Version),
		"total_operations": len(s.doc.Ops),
		"matched":          len(matches),
		"offset":           start,
		"operations":       views,
	}
	if end < len(matches) {
		out["next_offset"] = end
	}
	if !detail && len(views) > 0 {
		out["hint"] = "set detail=true or narrow the query to see parameters and request bodies"
	}
	if a.Query == "" && a.Tag == "" {
		out["tags"] = s.doc.tagCounts()
	}
	return provider.JSONResult(out)
}
