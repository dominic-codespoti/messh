// Package filesprov provides the files_* tools: browsing and tidying the two
// directories messh shares between devices (ws/ and artifacts/). They only
// touch those directories, so they need no approval.
package filesprov

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/files"
	"messh/internal/provider"
)

const refHelp = "Refs look like ws/<workspace>/<path> (working files for jobs) or " +
	"artifacts/<source>/<file> (outputs messh captured, e.g. audio from a service). " +
	"To move files between devices use the mesh_copy tool."

// Provider exposes a Store as tools.
type Provider struct{ store *files.Store }

func New(s *files.Store) *Provider { return &Provider{store: s} }

func (p *Provider) Name() string { return "files" }

func (p *Provider) Tools() []provider.Tool {
	yes := true
	return []provider.Tool{
		{Class: provider.ClassInfo, Def: &mcp.Tool{
			Name: "files_list",
			Description: "List files on this device's messh file areas. With no ref, shows the two roots " +
				"(ws = job working files, artifacts = captured outputs). With a ref, lists that directory. " + refHelp,
			InputSchema: json.RawMessage(`{"type":"object","properties":{"ref":{"type":"string","description":"directory ref, e.g. ws/myjob or artifacts/voicestudio; omit for the roots"}},"additionalProperties":false}`),
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, Title: "List files"},
		}},
		{Class: provider.ClassInfo, Def: &mcp.Tool{
			Name:        "files_stat",
			Description: "Size, modification time and type of one file or directory, optionally with its SHA-256. " + refHelp,
			InputSchema: json.RawMessage(`{"type":"object","properties":{"ref":{"type":"string","description":"file or directory ref"},"sha256":{"type":"boolean","description":"also compute the SHA-256 of a file (reads the whole file)"}},"required":["ref"],"additionalProperties":false}`),
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, Title: "Stat a file"},
		}},
		{Class: provider.ClassInfo, Def: &mcp.Tool{
			Name:        "files_mkdir",
			Description: "Create a directory (and parents) under ws/ on this device, e.g. ws/myjob/inputs. Existing directories are fine. " + refHelp,
			InputSchema: json.RawMessage(`{"type":"object","properties":{"ref":{"type":"string","description":"directory ref under ws/"}},"required":["ref"],"additionalProperties":false}`),
			Annotations: &mcp.ToolAnnotations{IdempotentHint: true, Title: "Create a directory"},
		}},
		{Class: provider.ClassInfo, Def: &mcp.Tool{
			Name: "files_delete",
			Description: "Delete a file or a whole directory tree under ws/ or artifacts/ on this device. " +
				"The roots ws and artifacts themselves cannot be deleted. " + refHelp,
			InputSchema: json.RawMessage(`{"type":"object","properties":{"ref":{"type":"string","description":"file or directory ref to delete"}},"required":["ref"],"additionalProperties":false}`),
			Annotations: &mcp.ToolAnnotations{DestructiveHint: &yes, IdempotentHint: true, Title: "Delete files"},
		}},
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
	Note      string       `json:"note,omitempty"`
}

func (p *Provider) Call(_ context.Context, tool string, raw json.RawMessage, _ provider.Caller) (*mcp.CallToolResult, error) {
	var a args
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return provider.ErrorResult("invalid arguments: %v", err), nil
		}
	}
	if a.Ref == "" && tool != "files_list" {
		return provider.ErrorResult("ref is required"), nil
	}
	switch tool {
	case "files_list":
		l, err := p.store.List(a.Ref)
		if err != nil {
			return fail(err)
		}
		out := listOut{Ref: a.Ref, Entries: l.Entries, Truncated: l.Truncated, Skipped: l.Skipped}
		if out.Entries == nil {
			out.Entries = []files.Info{}
		}
		if l.Truncated {
			out.Note = fmt.Sprintf("listing cut off at %d entries", files.MaxListEntries)
		}
		return provider.JSONResult(out)
	case "files_stat":
		in, err := p.store.Stat(a.Ref, a.SHA256)
		if err != nil {
			return fail(err)
		}
		return provider.JSONResult(in)
	case "files_mkdir":
		if err := p.store.Mkdir(a.Ref); err != nil {
			return fail(err)
		}
		return provider.JSONResult(map[string]string{"ref": a.Ref, "status": "ready"})
	case "files_delete":
		if err := p.store.Delete(a.Ref); err != nil {
			return fail(err)
		}
		return provider.JSONResult(map[string]string{"ref": a.Ref, "status": "deleted"})
	}
	return nil, fmt.Errorf("unknown tool %q", tool)
}

// fail turns store errors, which are written for the model, into tool errors;
// anything unexpected goes back to the node as a failure.
func fail(err error) (*mcp.CallToolResult, error) {
	for _, known := range []error{files.ErrNotFound, files.ErrExists, files.ErrIsDirectory, files.ErrNotDirectory,
		files.ErrReadOnly, files.ErrRoot, files.ErrNotRegular, files.ErrInvalid} {
		if errors.Is(err, known) {
			return provider.ErrorResult("%v", err), nil
		}
	}
	return nil, err
}
