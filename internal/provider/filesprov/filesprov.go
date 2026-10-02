package filesprov

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"messh/internal/files"
	"messh/internal/grants"
	"messh/internal/provider"
)

type Provider struct {
	store     *files.Store
	authorize func(provider.Caller, string, string) grants.Decision
	canSee    func(provider.Caller, string, string, string) bool
}

func NewAuthorized(s *files.Store, a func(provider.Caller, string, string) grants.Decision, v func(provider.Caller, string, string, string) bool) *Provider {
	return &Provider{store: s, authorize: a, canSee: v}
}
func (p *Provider) Name() string { return "files" }
func (p *Provider) Tools() []provider.Tool {
	return []provider.Tool{
		{Class: provider.ClassInfo, Def: &mcp.Tool{Name: "files_list", Description: "List files in ws/ or artifacts/ on this device.", InputSchema: json.RawMessage(`{"type":"object","properties":{"ref":{"type":"string"}},"additionalProperties":false}`)}},
		{Class: provider.ClassInfo, Def: &mcp.Tool{Name: "files_stat", Description: "Read metadata for a file or directory.", InputSchema: json.RawMessage(`{"type":"object","properties":{"ref":{"type":"string"},"sha256":{"type":"boolean"}},"required":["ref"],"additionalProperties":false}`)}},
		{Class: provider.ClassInfo, Def: &mcp.Tool{Name: "files_mkdir", Description: "Create a directory under ws/.", InputSchema: json.RawMessage(`{"type":"object","properties":{"ref":{"type":"string"}},"required":["ref"],"additionalProperties":false}`)}},
		{Class: provider.ClassInfo, Def: &mcp.Tool{Name: "files_delete", Description: "Delete a file or directory tree.", InputSchema: json.RawMessage(`{"type":"object","properties":{"ref":{"type":"string"}},"required":["ref"],"additionalProperties":false}`)}},
	}
}

type args struct {
	Ref    string `json:"ref"`
	SHA256 bool   `json:"sha256"`
}
type listOut struct {
	Ref       string       `json:"ref"`
	Entries   []files.Info `json:"entries"`
	Truncated bool         `json:"truncated,omitempty"`
	Skipped   int          `json:"skipped,omitempty"`
}

func (p *Provider) Call(_ context.Context, tool string, raw json.RawMessage, caller provider.Caller) (*mcp.CallToolResult, error) {
	var a args
	if len(raw) > 0 {
		if e := json.Unmarshal(raw, &a); e != nil {
			return provider.ErrorResult("invalid arguments"), nil
		}
	}
	if a.Ref == "" && tool != "files_list" {
		return provider.ErrorResult("ref is required"), nil
	}
	action := "read"
	if tool == "files_mkdir" || tool == "files_delete" {
		action = "write"
	}
	if tool != "files_list" {
		if p.authorize == nil {
			return provider.ErrorFrom(&files.CapabilityDeniedError{Code: "no_matching_grant"}, "files"), nil
		}
		d := p.authorize(caller, a.Ref, action)
		if !d.Allowed {
			return provider.ErrorFrom(&files.CapabilityDeniedError{Code: d.Code}, "files"), nil
		}
	}
	switch tool {
	case "files_list":
		l, e := p.store.List(a.Ref)
		if e != nil {
			return fail(e)
		}
		entries := l.Entries
		visible := entries[:0]
		if p.canSee != nil {
			for _, x := range entries {
				if p.canSee(caller, a.Ref, x.Ref, "read") {
					visible = append(visible, x)
				}
			}
		}
		entries = visible
		return provider.JSONResult(listOut{Ref: a.Ref, Entries: entries})
	case "files_stat":
		x, e := p.store.Stat(a.Ref, a.SHA256)
		if e != nil {
			return fail(e)
		}
		return provider.JSONResult(x)
	case "files_mkdir":
		if e := p.store.Mkdir(a.Ref); e != nil {
			return fail(e)
		}
		return provider.JSONResult(map[string]string{"ref": a.Ref, "status": "ready"})
	case "files_delete":
		if e := p.store.Delete(a.Ref); e != nil {
			return fail(e)
		}
		return provider.JSONResult(map[string]string{"ref": a.Ref, "status": "deleted"})
	}
	return nil, fmt.Errorf("unknown tool %q", tool)
}
func fail(e error) (*mcp.CallToolResult, error) {
	for _, known := range []error{files.ErrNotFound, files.ErrExists, files.ErrIsDirectory, files.ErrNotDirectory, files.ErrReadOnly, files.ErrRoot, files.ErrNotRegular, files.ErrInvalid} {
		if errors.Is(e, known) {
			return provider.ErrorResult("%v", e), nil
		}
	}
	return nil, e
}
