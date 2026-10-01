// Package provider defines how capabilities plug into a messh node.
package provider

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Class selects the approval gate a tool call passes before it runs.
type Class string

const (
	// ClassInfo marks tools that report facts or only touch messh's own
	// sandbox directories; they run without approval.
	ClassInfo Class = "info"
	// ClassExec marks tools that run programs on the device.
	ClassExec Class = "exec"
	// ClassService marks tools that act through a local service (an MCP
	// server or REST API) the device's owner registered with messh.
	ClassService Class = "service"
	// ClassBrowser marks tools that drive a web browser on the device (view
	// or act on pages), approved per website.
	ClassBrowser Class = "browser"
)

// Approval describes a gated call to the person who must allow it. Providers
// build it; the node's policy matches it against saved rules, prompts when
// none match, and records the outcome.
type Approval struct {
	Title   string   // one line, e.g. "Run a job" or "Call voicestudio generate_speech"
	Details []Detail // ordered lines shown in the prompt
	// Exact identifies this precise request (tool, arguments, and any input
	// content hashes). "Allow once" approves exactly this value.
	Exact string
	// Scopes are the choices offered for "Always allow", narrowest first. A
	// scope's Key is what a saved rule matches; Exact is always offered as the
	// first choice and need not be repeated here.
	Scopes []Scope
	// Deferred means the provider accepts the call immediately and waits for
	// the decision itself via TicketFrom(ctx), e.g. a job that should start on
	// its own once approved. Otherwise the node decides before calling.
	Deferred bool
	// Auto, when non-empty, means the device owner pre-authorised this exact
	// kind of call in the provider's own configuration (the text says where,
	// e.g. "services.json: voicestudio auto_read"). The node runs it without
	// prompting and records Auto as the reason in the audit log.
	Auto string
}

// Detail is one labelled line of an approval prompt.
type Detail struct {
	Label string
	Value string
}

// Scope is one "Always allow" choice.
type Scope struct {
	Key   string // stable rule key, e.g. "exec:argv:<sha256>" or "service:voicestudio/generate_speech"
	Label string // human description, e.g. "this command with any input files"
	Broad bool   // shown with a warning
}

// Gated is implemented by providers that have tools of a class other than
// ClassInfo. The node asks for the Approval before every such call.
type Gated interface {
	Approval(ctx context.Context, tool string, args json.RawMessage, caller Caller) (Approval, error)
}

// Ticket is the pending decision handed to a provider whose Approval is Deferred.
type Ticket interface {
	ID() string
	// Wait blocks until the call is approved (true) or denied, expired, or
	// cancelled (false, with the reason as err when one exists).
	Wait(ctx context.Context) (bool, error)
	// Cancel withdraws a pending request (closes its prompt). Safe to call
	// after a decision or more than once.
	Cancel()
}

type ticketKey struct{}

// WithTicket attaches a deferred approval to ctx.
func WithTicket(ctx context.Context, t Ticket) context.Context {
	return context.WithValue(ctx, ticketKey{}, t)
}

// TicketFrom returns the deferred approval attached by the node, if any.
func TicketFrom(ctx context.Context) (Ticket, bool) {
	t, ok := ctx.Value(ticketKey{}).(Ticket)
	return t, ok
}

// Notifier is implemented by providers whose tool set changes at runtime
// (for example when a local service comes up). The node calls SetOnChange
// once at registration; the provider calls fn after its Tools() result changes.
type Notifier interface {
	SetOnChange(fn func())
}

// Tool is a tool definition plus the gate that governs it.
type Tool struct {
	Def   *mcp.Tool
	Class Class
}

// Caller identifies who asked for a call.
type Caller struct {
	DeviceID   string // the calling device; equals the node's own ID for local agents
	DeviceName string
	Agent      string // agent name registered on the calling device
}

// Provider supplies tools to a node.
type Provider interface {
	Name() string
	Tools() []Tool
	Call(ctx context.Context, tool string, args json.RawMessage, caller Caller) (*mcp.CallToolResult, error)
}

// JSONResult renders v as both structured content and indented JSON text, so
// clients that ignore structuredContent still see the data.
func JSONResult(v any) (*mcp.CallToolResult, error) {
	text, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode result: %w", err)
	}
	return &mcp.CallToolResult{
		Content:           []mcp.Content{&mcp.TextContent{Text: string(text)}},
		StructuredContent: json.RawMessage(text),
	}, nil
}

// ErrorResult reports a tool-level failure the model should see.
func ErrorResult(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}},
		IsError: true,
	}
}
