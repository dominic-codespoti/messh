package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/buildinfo"
	"messh/internal/state"
)

// bearer adds an Authorization header to every request.
type bearer struct {
	token string
	next  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	resp, err := b.next.RoundTrip(r)
	if op := (*net.OpError)(nil); errors.As(err, &op) && op.Op == "dial" {
		return nil, fmt.Errorf("%w: nothing answers at %s: %v", state.ErrNodeNotRunning, r.URL.Host, err)
	}
	return resp, err
}

// localSession connects to this device's agent endpoint as the CLI agent.
func localSession(ctx context.Context, paths state.Paths) (*mcp.ClientSession, error) {
	run, err := paths.LoadRunInfo()
	if err != nil {
		return nil, err
	}
	tok, err := paths.ControlToken()
	if err != nil {
		return nil, err
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "messh-cli", Version: buildinfo.Version}, nil)
	return client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:             "http://" + run.Local + "/mcp",
		HTTPClient:           &http.Client{Transport: bearer{token: tok, next: http.DefaultTransport}},
		MaxRetries:           -1,
		DisableStandaloneSSE: true,
	}, nil)
}

func init() {
	add(&Command{
		Name:    "tools",
		Summary: "list the tools agents on this device see (full mode)",
		Flags: func(fs *flag.FlagSet) {
			fs.Duration("timeout", 30*time.Second, "give up after `DURATION`")
		},
		Output: `[{name, description}]`,
		Waits:  "up to --timeout for the local node to answer",
		Examples: []string{
			"messh tools",
			"messh tools --json",
		},
		Run: func(c *Context) error {
			paths, err := c.Paths()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), c.Duration("timeout"))
			defer cancel()
			s, err := localSession(ctx, paths)
			if err != nil {
				return err
			}
			defer s.Close()
			type toolRow struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			}
			var rows []toolRow
			var b strings.Builder
			for t, err := range s.Tools(ctx, nil) {
				if err != nil {
					return err
				}
				rows = append(rows, toolRow{Name: t.Name, Description: t.Description})
				desc, _, _ := strings.Cut(t.Description, "\n")
				fmt.Fprintf(&b, "%-32s %s\n", t.Name, desc)
			}
			if rows == nil {
				rows = []toolRow{}
			}
			return c.Emit(rows, func(w io.Writer) { io.WriteString(w, b.String()) })
		},
	})
	add(&Command{
		Name: "call",
		Args: []Arg{
			{Name: "TOOL", Help: "tool name, e.g. desktop__node_info"},
			{Name: "JSON", Help: "arguments as a JSON object", Optional: true},
		},
		Summary: "call one tool on this device and print its result",
		Flags: func(fs *flag.FlagSet) {
			fs.Duration("timeout", 2*time.Minute, "give up after `DURATION`")
		},
		Output: `{is_error, content[...], structured_content?} (the MCP result, faithfully; a tool error exits 1)`,
		Waits:  "up to --timeout for the tool to answer",
		Examples: []string{
			"messh call desktop__node_info",
			`messh call desktop__exec '{"cmd":"uptime"}'`,
		},
		Run: func(c *Context) error {
			var arguments json.RawMessage
			if len(c.Args) == 2 {
				var v any
				if err := json.Unmarshal([]byte(c.Args[1]), &v); err != nil {
					return usageErrorf("arguments are not valid JSON: %s", c.Args[1])
				}
				obj, ok := v.(map[string]any)
				if !ok {
					return usageErrorf("arguments must be a JSON object, not %s", c.Args[1])
				}
				raw, err := json.Marshal(obj)
				if err != nil {
					return err
				}
				arguments = raw
			}
			paths, err := c.Paths()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), c.Duration("timeout"))
			defer cancel()
			s, err := localSession(ctx, paths)
			if err != nil {
				return err
			}
			defer s.Close()
			params := &mcp.CallToolParams{Name: c.Args[0]}
			if arguments != nil {
				params.Arguments = arguments
			}
			res, err := s.CallTool(ctx, params)
			if err != nil {
				return err
			}
			if err := c.Emit(callJSON(res), func(w io.Writer) {
				for _, t := range callTexts(res) {
					fmt.Fprintln(w, t)
				}
			}); err != nil {
				return err
			}
			if res.IsError {
				return errors.New(toolErrorText(res))
			}
			return nil
		},
	})
}

// callJSON renders the MCP CallToolResult faithfully for --json.
func callJSON(res *mcp.CallToolResult) map[string]any {
	out := map[string]any{
		"is_error": res.IsError,
		"content":  res.Content,
	}
	if res.StructuredContent != nil {
		out["structured_content"] = res.StructuredContent
	}
	return out
}

// callTexts returns the text payloads of a tool result for human output.
func callTexts(res *mcp.CallToolResult) []string {
	var texts []string
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			texts = append(texts, t.Text)
		}
	}
	return texts
}

// toolErrorText summarizes a tool error result for the exit-1 failure message.
func toolErrorText(res *mcp.CallToolResult) string {
	if texts := callTexts(res); len(texts) > 0 {
		return strings.Join(texts, "\n")
	}
	return "tool returned an error"
}
