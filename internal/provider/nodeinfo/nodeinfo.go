// Package nodeinfo provides the node_info tool: identity plus live hardware,
// runtime, browser, and presence facts for the device.
package nodeinfo

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/provider"
	"messh/internal/sysinfo"
)

const toolName = "node_info"

// Info is the node_info result.
type Info struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	MesshVersion string `json:"messh_version"`
	sysinfo.Info
}

// Provider reports facts about the device it runs on.
type Provider struct {
	DeviceID, DeviceName, Version string
}

func (p *Provider) Name() string { return "nodeinfo" }

func (p *Provider) Tools() []provider.Tool {
	return []provider.Tool{{
		Class: provider.ClassInfo,
		Def: &mcp.Tool{
			Name: toolName,
			Description: "Report this device's OS, CPU, memory, GPUs (with live VRAM use), installed " +
				"runtimes, browsers, and whether the user is idle or the session is locked.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, Title: "Device info"},
		},
	}}
}

func (p *Provider) Call(ctx context.Context, tool string, _ json.RawMessage, _ provider.Caller) (*mcp.CallToolResult, error) {
	if tool != toolName {
		return nil, fmt.Errorf("unknown tool %q", tool)
	}
	return provider.JSONResult(Info{
		ID:           p.DeviceID,
		Name:         p.DeviceName,
		MesshVersion: p.Version,
		Info:         sysinfo.Collect(ctx),
	})
}
