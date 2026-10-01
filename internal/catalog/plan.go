package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/files"
	"messh/internal/provider"
)

// fileInput is a file an operation will read; its content hash is part of
// the approval identity so "allow once" cannot be reused for other bytes.
type fileInput struct {
	Label string
	Ref   string
	Abs   string
	// Optional marks a file that may not exist yet (a path the service creates).
	Optional bool
}

// plan is a parsed, validated call: everything needed to describe it for
// approval (without side effects) and to run it.
type plan struct {
	p        *Provider
	e        *entry
	svc      Service
	tool     string // provider tool name
	op       string // upstream tool, "METHOD /path-template", or generic tool name
	readOnly bool
	details  []provider.Detail
	exact    map[string]any // canonical identity of the request, without file hashes
	inputs   []fileInput
	// autoMatch tells whether an auto.tools entry names this operation.
	autoMatch func(entry string) bool
	run       func(ctx context.Context) (*mcp.CallToolResult, error)
}

// plan parses a generic or proxy tool call.
func (p *Provider) plan(tool string, args json.RawMessage) (*plan, error) {
	switch tool {
	case toolDescribe:
		return p.planDescribe(args)
	case toolCall:
		return p.planCall(args)
	case toolModels, toolChat:
		return p.planOpenAI(tool, args)
	}
	return p.planProxy(tool, args)
}

// decodeArgs strictly decodes tool arguments into v.
func decodeArgs(args json.RawMessage, v any) error {
	if len(bytes.TrimSpace(args)) == 0 || string(bytes.TrimSpace(args)) == "null" {
		args = json.RawMessage("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	if dec.More() {
		return errors.New("invalid arguments: trailing data")
	}
	return nil
}

// canonical re-encodes JSON with sorted keys and exact numbers.
func canonical(raw json.RawMessage) (any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

func hashHex(v any) string {
	b, _ := json.Marshal(v) // map keys are sorted by encoding/json
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// approval builds the prompt data for the plan.
func (pl *plan) approval(caller provider.Caller) (provider.Approval, error) {
	identity := map[string]any{"v": 1, "service": pl.svc.Name, "kind": pl.svc.Kind, "tool": pl.tool, "op": pl.op}
	for k, v := range pl.exact {
		identity[k] = v
	}
	if len(pl.inputs) > 0 {
		hashes := map[string]any{}
		for _, in := range pl.inputs {
			sum, err := hashFile(in.Abs)
			if err != nil {
				// A *_path argument may name a file the service will create.
				if in.Optional && errors.Is(err, errAbsent) {
					sum, err = "(absent)", nil
				} else {
					return provider.Approval{}, fmt.Errorf("file %s (%s): %w", in.Ref, in.Label, err)
				}
			}
			hashes[in.Label+"="+in.Ref] = sum
		}
		identity["files"] = hashes
	}
	details := []provider.Detail{
		{Label: "Service", Value: pl.svc.Name + " (" + pl.svc.Kind + ")"},
		{Label: "Operation", Value: pl.op},
	}
	details = append(details, pl.details...)
	if who := callerLabel(caller); who != "" {
		details = append(details, provider.Detail{Label: "Requested by", Value: who})
	}
	ap := provider.Approval{
		Title:   "Call " + pl.svc.Name + ": " + pl.op,
		Details: details,
		Exact:   hashHex(identity),
		Scopes: []provider.Scope{
			{Key: "service:" + pl.svc.Name + ":" + pl.op, Label: "this operation (" + pl.op + ") on " + pl.svc.Name + ", with any arguments"},
			{Key: "service:" + pl.svc.Name + ":*", Label: "anything on service " + pl.svc.Name, Broad: true},
		},
	}
	if pl.svc.Auto.Read && pl.readOnly {
		ap.Auto = "services.json: " + pl.svc.Name + " auto.read"
	} else {
		for _, entry := range pl.svc.Auto.Tools {
			if pl.autoMatch != nil && pl.autoMatch(entry) {
				ap.Auto = "services.json: " + pl.svc.Name + " auto.tools " + fmt.Sprintf("%q", entry)
				break
			}
		}
	}
	return ap, nil
}

func callerLabel(c provider.Caller) string {
	switch {
	case c.Agent != "" && c.DeviceName != "":
		return c.Agent + " on " + c.DeviceName
	case c.DeviceName != "":
		return c.DeviceName
	case c.Agent != "":
		return c.Agent
	}
	return c.DeviceID
}

var errAbsent = errors.New("does not exist")

func hashFile(abs string) (string, error) {
	f, err := os.Open(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", errAbsent
		}
		return "", err
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return "", errors.New("not a regular file")
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// resolveRef maps an unqualified local file ref to its absolute path.
func (p *Provider) resolveRef(s string) (files.Ref, string, error) {
	if dev, _ := files.SplitQualified(s); dev != "" {
		return "", "", fmt.Errorf("file reference %q: use a plain ws/... or artifacts/... ref; copy remote files here first", s)
	}
	ref, err := files.ParseRef(s)
	if err != nil {
		return "", "", err
	}
	abs, err := files.Resolve(p.paths, ref)
	if err != nil {
		return "", "", err
	}
	return ref, abs, nil
}

var (
	driveRE  = regexp.MustCompile(`^[A-Za-z]:`)
	secretRE = regexp.MustCompile(`(?i)(key|token|secret|passw|authorization|cookie|credential)`)
)

// translatePathArg turns a file ref into the host path the upstream needs.
// Absolute paths, home-relative paths and ".." are refused: services reach
// files through refs only.
func (p *Provider) translatePathArg(v string) (out string, input *fileInput, err error) {
	if strings.HasPrefix(v, files.RootWorkspaces+"/") || strings.HasPrefix(v, files.RootArtifacts+"/") {
		ref, abs, err := p.resolveRef(v)
		if err != nil {
			return "", nil, err
		}
		return abs, &fileInput{Ref: string(ref), Abs: abs}, nil
	}
	if dev, rest := files.SplitQualified(v); dev != "" && !driveRE.MatchString(v) &&
		(strings.HasPrefix(rest, files.RootWorkspaces+"/") || strings.HasPrefix(rest, files.RootArtifacts+"/")) {
		return "", nil, fmt.Errorf("file reference %q: use a plain ws/... or artifacts/... ref; copy remote files here first", v)
	}
	if strings.HasPrefix(v, "/") || strings.HasPrefix(v, `\`) || strings.HasPrefix(v, "~") || driveRE.MatchString(v) ||
		slices.Contains(strings.FieldsFunc(v, func(r rune) bool { return r == '/' || r == '\\' }), "..") {
		return "", nil, fmt.Errorf("path argument %q must be a messh file ref (ws/<workspace>/... or artifacts/<source>/...), not a host path", clip(v, 60))
	}
	return v, nil, nil
}

// summarize renders one argument for an approval prompt: bounded, with
// credential-looking and base64 values hidden.
func summarize(key string, raw json.RawMessage) string {
	if secretRE.MatchString(key) {
		return "(hidden)"
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch {
		case strings.HasSuffix(strings.ToLower(key), "base64") || strings.HasPrefix(s, "data:") || (len(s) > 512 && !strings.ContainsAny(s, " \n")):
			return fmt.Sprintf("<%d characters of data>", len(s))
		}
		return clip(strings.ReplaceAll(s, "\n", " "), 160)
	}
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return "(unreadable)"
	}
	return clip(buf.String(), 160)
}

// argDetails lists the top-level arguments of a JSON object, sorted.
func argDetails(m map[string]json.RawMessage) []provider.Detail {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []provider.Detail
	for i, k := range keys {
		if i >= 12 {
			out = append(out, provider.Detail{Label: "…", Value: fmt.Sprintf("%d more arguments", len(keys)-i)})
			break
		}
		out = append(out, provider.Detail{Label: "arg " + k, Value: summarize(k, m[k])})
	}
	return out
}

// planProxy parses a call to a re-published upstream MCP tool.
func (p *Provider) planProxy(tool string, args json.RawMessage) (*plan, error) {
	e, pt, ok := p.proxyOwner(tool)
	if !ok {
		return nil, fmt.Errorf("unknown tool %q", tool)
	}
	if !permitsTool(&e.cfg, pt.Upstream) {
		return nil, fmt.Errorf("service %s does not allow tool %s", e.cfg.Name, pt.Upstream)
	}
	m := map[string]json.RawMessage{}
	if t := bytes.TrimSpace(args); len(t) > 0 && string(t) != "null" {
		if err := json.Unmarshal(t, &m); err != nil {
			return nil, fmt.Errorf("arguments must be a JSON object: %w", err)
		}
	}
	canon := make(map[string]any, len(m))
	for k, v := range m {
		c, err := canonical(v)
		if err != nil {
			return nil, fmt.Errorf("argument %s: %w", k, err)
		}
		canon[k] = c
	}
	send := make(map[string]json.RawMessage, len(m))
	for k, v := range m {
		send[k] = v
	}
	var inputs []fileInput
	for _, name := range pt.PathArgs {
		raw, ok := m[name]
		if !ok {
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			continue // not a string: the upstream will validate it
		}
		out, in, err := p.translatePathArg(s)
		if err != nil {
			return nil, fmt.Errorf("argument %s: %w", name, err)
		}
		if in != nil {
			in.Label, in.Optional = name, true
			inputs = append(inputs, *in)
		}
		send[name], _ = json.Marshal(out)
	}
	body, _ := json.Marshal(send)
	pl := &plan{
		p: p, e: e, svc: e.cfg, tool: tool, op: pt.Upstream, readOnly: pt.ReadOnly,
		details:   argDetails(m),
		exact:     map[string]any{"args": canon},
		inputs:    inputs,
		autoMatch: func(entry string) bool { return matchName(entry, pt.Upstream) },
	}
	pl.run = func(ctx context.Context) (*mcp.CallToolResult, error) {
		return p.runProxy(ctx, e, pt, body)
	}
	return pl, nil
}

// runProxy forwards a call to the upstream MCP server and converts the reply.
func (p *Provider) runProxy(ctx context.Context, e *entry, pt proxyTool, args json.RawMessage) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	secrets := e.secret.secrets()
	res, err := e.mcp.call(ctx, pt.Upstream, args)
	if err != nil {
		if !isProtocolError(err) {
			e.requestProbe()
		}
		return provider.ErrorResult("service %s: %s", e.cfg.Name, redactString(describeErr(err), secrets)), nil
	}
	sk := &sink{paths: p.paths, source: e.cfg.Name}
	out, err := sk.convert(res)
	if err != nil {
		return provider.ErrorResult("service %s: %s", e.cfg.Name, redactString(err.Error(), secrets)), nil
	}
	return redactResult(out, secrets), nil
}

// redactResult removes secrets from every text a result carries.
func redactResult(r *mcp.CallToolResult, secrets []string) *mcp.CallToolResult {
	if len(secrets) == 0 {
		return r
	}
	for _, c := range r.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			t.Text = redactString(t.Text, secrets)
		}
	}
	if r.StructuredContent != nil {
		if b, err := json.Marshal(r.StructuredContent); err == nil {
			if s := redactString(string(b), secrets); s != string(b) {
				r.StructuredContent = json.RawMessage(s)
			}
		}
	}
	return r
}
