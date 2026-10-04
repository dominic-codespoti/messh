package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/identity"
	"messh/internal/state"
)

// `messh wsl status` is the diagnostic-owned half of the wsl command group:
// read-only inspection of the Windows+WSL desktop targets. The lifecycle
// slice owns `wsl setup` / `wsl refresh`; this file registers only status,
// so the two slices coexist through the longest-prefix command lookup.
//
// With no argument the local node's desktop_targets tool answers (on
// Windows: the host inventory; elsewhere: honestly unavailable). With
// WINDOWS_DEVICE the exact Windows ID (or handle/name/ID-prefix, same tiers
// as job routing) is used through the existing authenticated full/compact
// mesh routing, so explicit remote inspection works from any OS. There is no
// SSH fallback and no alias guessing: an unreachable Windows host fails as a
// transport error, never as a stopped-WSL verdict.
func init() {
	add(&Command{
		Name:    "wsl status",
		Args:    []Arg{{Name: "WINDOWS_DEVICE", Help: "Windows device to inspect (handle, name, or ID from `messh tools`/`mesh_nodes`); omit for this device", Optional: true}},
		Summary: "inspect the Windows+WSL desktop targets (read-only; never starts or changes anything)",
		Help: "Reports the captured WSL target (distro, expected IDs, mesh ports, stale-capable route facts) " +
			"with observed host/guest state from the answering Windows host's read-only inventories " +
			"(wsl --list, configured route facts, bounded TCP checks). With WINDOWS_DEVICE the named Windows " +
			"device answers over the authenticated mesh, so a reachable host with a stopped guest is " +
			"distinguishable from an unreachable host. A TCP connection is reported as TCP only, never as " +
			"authenticated messh identity. Never boots the guest, mutates routing, or starts services.",
		Flags: func(fs *flag.FlagSet) {
			fs.String("agent", "", "act as registered agent `NAME` (default: the CLI itself)")
			fs.Duration("timeout", 30*time.Second, "give up after `DURATION`")
		},
		Output:   `the desktop_targets report {host, guest, route, targets, policy, recovery}`,
		Examples: []string{"messh wsl status", "messh wsl status dompc-win"},
		Run:      runWSLStatus,
	})
}

func runWSLStatus(c *Context) error {
	transport := c.Duration("timeout")
	if transport <= 0 {
		return usageErrorf("--timeout must be positive")
	}
	// Local path first: when no device is named, the local node's own host
	// inventory is authoritative for the machine it runs on. LoadWSLTarget
	// distinguishes corrupt (error) from unconfigured (nil,nil) so the
	// local report can say so without a mesh round trip: on non-Windows the
	// inventory is unavailable and the guest verdict is honestly unknown.
	if len(c.Args) == 0 {
		paths, err := c.Paths()
		if err != nil {
			return err
		}
		cfg, err := paths.LoadWSLTarget()
		if err != nil {
			// Contract: lifecycle's LoadWSLTarget returns (nil,nil) when
			// unconfigured, so any error here is corruption, not absence.
			return withHint(fmt.Errorf("the local WSL target file is unreadable: %v", err),
				"re-capture it on the Windows host with `messh wsl setup`")
		}
		if cfg != nil {
			return wslStatusLocal(c, transport)
		}
		// Nil on non-Windows is unconfigured-or-elsewhere, reported
		// plainly: the local node truthfully has no Windows inventory.
		if runtime.GOOS != "windows" {
			return withHint(fmt.Errorf("no WSL target configured on this device, and this device is not Windows"),
				"name the Windows host explicitly: `messh wsl status WINDOWS_DEVICE`")
		}
		return wslStatusLocal(c, transport)
	}
	return wslStatusRemote(c, c.Args[0], transport)
}

// wslStatusLocal calls this device's own desktop_targets tool through the
// same authenticated routing, addressed by the running node's exact ID from
// its run file (never by name or alias). The desktop_targets tool takes no
// arguments.
func wslStatusLocal(c *Context, transport time.Duration) (err error) {
	raw, err := meshToolCall(c, "", "desktop_targets", json.RawMessage(`{}`), transport)
	if err != nil {
		return err
	}
	return c.Emit(raw, func(w io.Writer) { writeWSLStatus(w, raw) })
}

// wslStatusRemote calls desktop_targets on the named Windows device through
// the existing authenticated mesh routing. The device reference travels
// verbatim: the gateway resolves exact ID before handle before name before
// unique ID prefix, and ambiguity fails there rather than guessing here.
// Transport failures propagate as-is so an unreachable Windows host reads as
// unreachable, never as a stopped guest.
func wslStatusRemote(c *Context, device string, transport time.Duration) error {
	raw, err := meshToolCall(c, device, "desktop_targets", json.RawMessage(`{}`), transport)
	if err != nil {
		return err
	}
	return c.Emit(raw, func(w io.Writer) { writeWSLStatus(w, raw) })
}

// meshToolCall routes one ClassInfo tool call through the gateway in the
// CLI's own mode: full mode calls the exact namespaced tool, compact mode
// sends a mesh_call envelope for the gateway to resolve. Transport failures
// propagate as-is: an unreachable Windows host reads as unreachable, never
// as a stopped guest.
func meshToolCall(c *Context, device, tool string, args json.RawMessage, transport time.Duration) (json.RawMessage, error) {
	if device == "" {
		// Local status: route to this device's own ID exactly, never by
		// name or alias. The ID is stable and unambiguous in both modes.
		paths, err := c.Paths()
		if err != nil {
			return nil, err
		}
		id, err := localDeviceID(paths)
		if err != nil {
			return nil, err
		}
		device = id
	}
	paths, err := c.Paths()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), transport)
	defer cancel()
	s, err := localSession(ctx, paths)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	agent := c.String("agent")
	if agent == "" {
		agent = "cli"
	}
	mode, err := paths.AgentMode(agent)
	if err != nil {
		return nil, err
	}
	if mode == state.ToolsFull {
		return meshToolCallFull(ctx, s, device, tool, args)
	}
	return meshToolCallCompact(ctx, s, device, tool, args)
}

func meshToolCallFull(ctx context.Context, s *mcp.ClientSession, device, tool string, args json.RawMessage) (json.RawMessage, error) {
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "mesh_nodes"})
	if err != nil {
		return nil, err
	}
	raw, err := decodeToolResult(res)
	if err != nil {
		return nil, err
	}
	var nodes []jobMeshNode
	if err := json.Unmarshal(raw, &nodes); err != nil {
		return nil, fmt.Errorf("read mesh_nodes: %w", err)
	}
	target, err := resolveJobTarget(nodes, device)
	if err != nil {
		return nil, err
	}
	res, err = s.CallTool(ctx, &mcp.CallToolParams{Name: target.Handle + "__" + tool, Arguments: args})
	if err != nil {
		return nil, err
	}
	if res.IsError {
		return nil, errors.New(toolErrorText(res))
	}
	raw, err = json.Marshal(res.StructuredContent)
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("call %s returned no result", tool)
	}
	return raw, nil
}

func meshToolCallCompact(ctx context.Context, s *mcp.ClientSession, device, tool string, args json.RawMessage) (json.RawMessage, error) {
	var argObj map[string]any
	if len(args) > 0 {
		if err := json.Unmarshal(args, &argObj); err != nil {
			return nil, err
		}
	}
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "mesh_call", Arguments: mustMeshCallArgs(device, tool, argObj)})
	if err != nil {
		return nil, err
	}
	if res.IsError {
		return nil, errors.New(toolErrorText(res))
	}
	raw, err := json.Marshal(res.StructuredContent)
	if err != nil || len(raw) == 0 || string(raw) == "null" {
		return nil, fmt.Errorf("mesh_call %s returned no result", tool)
	}
	return raw, nil
}

// jobMeshNode is one mesh_nodes entry: the handle namespaces the device's
// tools as <handle>__<tool> in full mode.
type jobMeshNode struct {
	Handle string `json:"handle"`
	Name   string `json:"name"`
	ID     string `json:"id"`
}

// resolveJobTarget maps what was typed (a handle, a name, the ID, or a unique
// ID prefix) to one device: exact IDs and handles win over ambiguous names,
// and ambiguity fails rather than guessing.
func resolveJobTarget(nodes []jobMeshNode, ref string) (jobMeshNode, error) {
	ref = strings.TrimSpace(ref)
	lower := strings.ToLower(ref)
	for tier := 0; tier < 4; tier++ {
		selected, count := 0, 0
		for i, n := range nodes {
			match := false
			switch tier {
			case 0:
				match = strings.EqualFold(n.ID, ref)
			case 1:
				match = n.Handle == lower
			case 2:
				match = strings.EqualFold(n.Name, ref)
			case 3:
				match = len(lower) >= 4 && strings.HasPrefix(strings.ToLower(n.ID), lower)
			}
			if match {
				selected, count = i, count+1
			}
		}
		switch count {
		case 0:
		case 1:
			return nodes[selected], nil
		default:
			return jobMeshNode{}, fmt.Errorf("%q matches %d devices; use the handle or exact ID from mesh_nodes", ref, count)
		}
	}
	have := make([]string, 0, len(nodes))
	for _, n := range nodes {
		have = append(have, n.Handle)
	}
	return jobMeshNode{}, fmt.Errorf("no device %q on this mesh (have: %s)", ref, strings.Join(have, ", "))
}

// decodeToolResult renders a successful tool result as its structured JSON:
// the single text payload when present, else the structured content. A tool
// error becomes a plain failure with the device's message.
func decodeToolResult(res *mcp.CallToolResult) (json.RawMessage, error) {
	if res == nil {
		return nil, fmt.Errorf("the device returned no result")
	}
	if res.IsError {
		return nil, fmt.Errorf("%s", toolErrorText(res))
	}
	if texts := callTexts(res); len(texts) == 1 {
		return json.RawMessage([]byte(texts[0])), nil
	}
	if res.StructuredContent != nil {
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			return nil, fmt.Errorf("the device returned an unreadable result: %w", err)
		}
		return json.RawMessage(raw), nil
	}
	return nil, fmt.Errorf("the device returned an unreadable result")
}

func mustMeshCallArgs(device, tool string, args map[string]any) json.RawMessage {
	if args == nil {
		args = map[string]any{}
	}
	raw, _ := json.Marshal(map[string]any{"device": device, "tool": tool, "arguments": args})
	return raw
}

// localDeviceID returns the running local node's ID from its run file:
// stable, exact, and unambiguous in both gateway modes, so local status
// never depends on a handle or alias. It creates nothing: with no running
// node the caller gets ErrNodeNotRunning, not a fresh identity.
func localDeviceID(paths state.Paths) (string, error) {
	run, err := paths.LoadRunInfo()
	if err != nil {
		return "", err
	}
	if run.ID == "" {
		return "", fmt.Errorf("the running node recorded no device ID")
	}
	return run.ID, nil
}

func writeWSLStatus(w io.Writer, res json.RawMessage) {
	var rep struct {
		Host struct {
			OS        string `json:"os"`
			Inventory string `json:"inventory"`
			Detail    string `json:"detail"`
		} `json:"host"`
		Guest struct {
			Distro   string `json:"distro"`
			State    string `json:"state"`
			Evidence string `json:"evidence"`
			Service  string `json:"service"`
			Identity string `json:"identity"`
		} `json:"guest"`
		Route struct {
			Detail string `json:"detail"`
		} `json:"route"`
		Targets []struct {
			Role string `json:"role"`
			Name string `json:"name"`
			ID   string `json:"id"`
		} `json:"targets"`
		Recovery []string `json:"recovery"`
	}
	if err := json.Unmarshal([]byte(res), &rep); err != nil {
		fmt.Fprintln(w, string(res))
		return
	}
	for _, t := range rep.Targets {
		fmt.Fprintf(w, "%-7s %s  %s\n", t.Role, t.Name, identity.Short(t.ID))
	}
	fmt.Fprintf(w, "host: %s (inventory %s)\n", rep.Host.OS, rep.Host.Inventory)
	if rep.Host.Detail != "" {
		fmt.Fprintf(w, "  %s\n", oneLine(rep.Host.Detail))
	}
	distro := rep.Guest.Distro
	if distro == "" {
		distro = "WSL guest"
	}
	fmt.Fprintf(w, "guest: %s is %s\n", distro, rep.Guest.State)
	if rep.Guest.Evidence != "" {
		fmt.Fprintf(w, "  %s\n", oneLine(rep.Guest.Evidence))
	}
	fmt.Fprintf(w, "service: %s\n", rep.Guest.Service)
	if rep.Guest.Identity != "" {
		fmt.Fprintf(w, "  identity %s\n", oneLine(rep.Guest.Identity))
	}
	if rep.Route.Detail != "" {
		fmt.Fprintf(w, "route: %s\n", oneLine(rep.Route.Detail))
	}
	for _, r := range rep.Recovery {
		fmt.Fprintf(w, "next: %s\n", oneLine(r))
	}
}
