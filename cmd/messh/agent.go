package main

import (
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"messh/internal/state"
)

func init() {
	add(&Command{
		Name:    "agent add",
		Args:    []Arg{{Name: "NAME", Help: `agent to register (1-32 chars of a-z, 0-9, '-'; "cli" is reserved)`}},
		Summary: "register an agent and print how it connects to this device",
		Help: "Re-running add for an existing agent keeps its token and only changes the tool mode.\n" +
			"The token itself is never printed; clients fetch it at request time with the token command.",
		Flags: func(fs *flag.FlagSet) {
			choiceFlag(fs, "tools", "", "tool mode the agent sees", state.ToolsCompact, state.ToolsFull)
		},
		Output:  `{agent, mode, created, mcp_url, token_command}`,
		Mutates: true,
		Examples: []string{
			"messh agent add pi",
			"messh agent add omp --tools full",
		},
		Run: func(c *Context) error { return agentAdd(c, c.Args[0], c.String("tools")) },
	})
	add(&Command{
		Name: "agent mode",
		Args: []Arg{
			{Name: "NAME", Help: "registered agent"},
			{Name: "MODE", Help: "compact keeps the agent's context small, full shows every device tool", Choices: []string{state.ToolsCompact, state.ToolsFull}},
		},
		Summary: "switch which tools an agent sees (applies from its next request)",
		Output:  `{agent, mode}`,
		Mutates: true,
		Examples: []string{
			"messh agent mode pi full",
		},
		Run: func(c *Context) error {
			paths, err := c.Paths()
			if err != nil {
				return err
			}
			if err := paths.SetAgentMode(c.Args[0], c.Args[1]); err != nil {
				return err
			}
			return c.Emit(map[string]string{"agent": c.Args[0], "mode": c.Args[1]}, func(w io.Writer) {
				fmt.Fprintf(w, "Agent %q now uses %s tools; this applies from its next request.\n", c.Args[0], c.Args[1])
			})
		},
	})
	add(&Command{
		Name:    "agent ls",
		Summary: "list registered agents and their tool modes",
		Output:  `[{name, mode}]`,
		Examples: []string{
			"messh agent ls",
		},
		Run: func(c *Context) error {
			paths, err := c.Paths()
			if err != nil {
				return err
			}
			names, err := paths.Agents()
			if err != nil {
				return err
			}
			type row struct {
				Name string `json:"name"`
				Mode string `json:"mode"`
			}
			rows := []row{}
			var b strings.Builder
			tw := tabwriter.NewWriter(&b, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tTOOLS")
			for _, n := range names {
				mode, err := paths.AgentMode(n)
				if err != nil {
					mode = "error: " + err.Error()
				}
				rows = append(rows, row{Name: n, Mode: mode})
				fmt.Fprintf(tw, "%s\t%s\n", n, mode)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
			return c.Emit(rows, func(w io.Writer) { io.WriteString(w, b.String()) })
		},
	})
	add(&Command{
		Name:    "agent rm",
		Args:    []Arg{{Name: "NAME", Help: "registered agent"}},
		Summary: "revoke an agent's token (re-adding the name registers it again)",
		Output:  `{agent, removed}`,
		Mutates: true,
		Examples: []string{
			"messh agent rm old-pi",
		},
		Run: func(c *Context) error {
			paths, err := c.Paths()
			if err != nil {
				return err
			}
			if err := paths.RemoveAgent(c.Args[0]); err != nil {
				return err
			}
			return c.Emit(map[string]any{"agent": c.Args[0], "removed": true}, func(w io.Writer) {
				fmt.Fprintf(w, "Removed agent %q; its token no longer works.\n", c.Args[0])
			})
		},
	})
	add(&Command{
		Name:    "agent token",
		Args:    []Arg{{Name: "NAME", Help: "registered agent"}},
		Summary: "print an agent's token (a secret; prefer the token command harnesses run)",
		Help: "Prints a secret: anyone holding the token can act as the agent. Prefer letting clients\n" +
			"run the token command (see `messh agent add`) so the token is never stored in a file.",
		Flags: func(fs *flag.FlagSet) {
			fs.Bool("bearer", false, "print the token as an Authorization header value")
		},
		Output: `{agent, token (with the "Bearer " prefix when --bearer is given)}`,
		Examples: []string{
			"messh agent token pi",
			"messh agent token pi --bearer",
		},
		Run: func(c *Context) error {
			paths, err := c.Paths()
			if err != nil {
				return err
			}
			tok, err := paths.AgentToken(c.Args[0])
			if err != nil {
				return err
			}
			if c.Bool("bearer") {
				tok = "Bearer " + tok
			}
			return c.Emit(map[string]string{"agent": c.Args[0], "token": tok}, func(w io.Writer) {
				fmt.Fprintln(w, tok)
			})
		},
	})
}

// agentAdd registers name (or keeps an existing agent's token), applies the
// requested tool mode, and prints generic MCP connection details.
func agentAdd(c *Context, name, mode string) error {
	paths, err := c.Paths()
	if err != nil {
		return err
	}
	_, err = paths.AgentToken(name)
	existed := err == nil
	if _, err := paths.AddAgent(name); err != nil {
		return err
	}
	if mode != "" {
		if err := paths.SetAgentMode(name, mode); err != nil {
			return err
		}
	}
	current, err := paths.AgentMode(name)
	if err != nil {
		return err
	}

	var stateDir string
	if c.Set("state") {
		stateDir = paths.Root
	}
	tokenCmd := tokenCommand(name, true, stateDir)
	url := localMCPURL(paths)
	var b strings.Builder
	if existed {
		fmt.Fprintf(&b, "Agent %q is already registered; its token is unchanged. Tool mode: %s.\n\n", name, current)
	} else {
		fmt.Fprintf(&b, "Registered agent %q. Tool mode: %s.\n\n", name, current)
	}
	fmt.Fprintf(&b, "Connect an MCP client to %s with header \"Authorization: $(%s)\".\n\n", url, tokenCmd)
	other := state.ToolsFull
	if current == state.ToolsFull {
		other = state.ToolsCompact
	}
	switch current {
	case state.ToolsFull:
		fmt.Fprintf(&b, "Tools: full - the agent sees every device tool as <device>__<tool> next to the mesh tools.\n")
	default:
		fmt.Fprintf(&b, "Tools: compact - the agent sees only the mesh tools (mesh_nodes, mesh_tools, mesh_call, mesh_copy, ...)\n")
		fmt.Fprintf(&b, "and reaches each device's tools with mesh_tools (search) and mesh_call (run), keeping its context small.\n")
	}
	fmt.Fprintf(&b, "Switch with `messh agent mode %s %s%s`; it applies from the agent's next request.\n", name, other, stateFlag(stateDir))
	fmt.Fprintf(&b, "Teach a shell-capable agent the messh CLI: messh skill install --for omp|pi|agents\n")

	return c.Emit(map[string]any{
		"agent":         name,
		"mode":          current,
		"created":       !existed,
		"mcp_url":       url,
		"token_command": tokenCmd,
	}, func(w io.Writer) {
		io.WriteString(w, b.String())
	})
}
