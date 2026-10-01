package catalog

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// maxToolName is the longest local tool name; the gateway prepends
// "<device>__" and model APIs cap names at 64 characters.
const maxToolName = 40

// Generic tool names.
const (
	toolCatalogue = "catalogue"
	toolDescribe  = "service_describe"
	toolCall      = "service_call"
	toolModels    = "openai_models"
	toolChat      = "openai_chat"
)

// proxyTool is one upstream MCP tool re-published under a local name.
type proxyTool struct {
	Name     string // local tool name, "<service>__<tool>" shortened to fit
	Upstream string // the upstream tool name
	Def      *mcp.Tool
	ReadOnly bool
	// PathArgs are top-level string arguments named "path" or "*_path" that
	// accept messh file refs.
	PathArgs []string
}

func sanitizeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return b.String()
}

func shortHash(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:3])
}

// proxyName derives "<service>__<tool>" within maxToolName characters. Names
// that had to be changed (sanitised or shortened) carry a hash of the
// original so different upstream names never collapse together.
func proxyName(service, tool string) string {
	base := service + "__" + sanitizeName(tool)
	if base == service+"__"+tool && len(base) <= maxToolName {
		return base
	}
	suffix := "_" + shortHash(tool)
	if len(base)+len(suffix) <= maxToolName {
		return base + suffix
	}
	return base[:maxToolName-len(suffix)] + suffix
}

// buildProxies turns upstream tools into published tools, honouring allow
// and deny. The result is deterministic for a given upstream list.
func buildProxies(cfg Service, upstream []*mcp.Tool) []proxyTool {
	sorted := append([]*mcp.Tool(nil), upstream...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	used := map[string]bool{}
	var out []proxyTool
	for _, t := range sorted {
		if t == nil || t.Name == "" || !permitsTool(&cfg, t.Name) {
			continue
		}
		name := proxyName(cfg.Name, t.Name)
		for n := 2; used[name]; n++ {
			// Only reachable by a hash collision or duplicate upstream names.
			name = proxyName(cfg.Name, fmt.Sprintf("%s#%d", t.Name, n))
		}
		used[name] = true
		schema, pathArgs := objectSchema(t.InputSchema)
		desc := "[service " + cfg.Name + "] " + t.Description
		if len(pathArgs) > 0 {
			desc += "\nArguments " + strings.Join(pathArgs, ", ") + " take a messh file ref (ws/... or artifacts/... on this device), which is translated to the file's real path for the service."
		}
		def := &mcp.Tool{Name: name, Title: t.Title, Description: desc, InputSchema: schema}
		if t.Annotations != nil {
			a := *t.Annotations
			def.Annotations = &a
		}
		out = append(out, proxyTool{
			Name: name, Upstream: t.Name, Def: def, PathArgs: pathArgs,
			ReadOnly: t.Annotations != nil && t.Annotations.ReadOnlyHint,
		})
	}
	return out
}

// objectSchema returns a private copy of an upstream input schema coerced to
// an object schema, with file-ref hints added to *_path string properties.
func objectSchema(in any) (map[string]any, []string) {
	var schema map[string]any
	if b, err := json.Marshal(in); err == nil {
		json.Unmarshal(b, &schema)
	}
	if schema == nil || schema["type"] != "object" {
		return map[string]any{"type": "object"}, nil
	}
	props, _ := schema["properties"].(map[string]any)
	var pathArgs []string
	for name, p := range props {
		pm, ok := p.(map[string]any)
		if !ok || !isPathArg(name) {
			continue
		}
		if t, _ := pm["type"].(string); t != "" && t != "string" {
			continue
		}
		pathArgs = append(pathArgs, name)
		d, _ := pm["description"].(string)
		pm["description"] = strings.TrimSpace(d + " (messh file ref such as ws/<workspace>/file or artifacts/<source>/file)")
	}
	sort.Strings(pathArgs)
	return schema, pathArgs
}

func isPathArg(name string) bool {
	return name == "path" || strings.HasSuffix(name, "_path")
}

func rawSchema(s string) json.RawMessage { return json.RawMessage(s) }

const (
	schemaCatalogue = `{"type":"object","properties":{"tag":{"type":"string","description":"only services with this tag"},"kind":{"type":"string","enum":["mcp","openapi","openai","http"],"description":"only services of this kind"}},"additionalProperties":false}`

	schemaDescribe = `{"type":"object","required":["service"],"properties":{` +
		`"service":{"type":"string","description":"service name from catalogue"},` +
		`"query":{"type":"string","description":"words to look for in operation paths, summaries, tags and descriptions (all must match)"},` +
		`"tag":{"type":"string","description":"only operations with this tag"},` +
		`"offset":{"type":"integer","minimum":0,"description":"skip this many matches (paging)"},` +
		`"limit":{"type":"integer","minimum":1,"maximum":50,"description":"matches per page (default 20)"},` +
		`"detail":{"type":"boolean","description":"include parameters and request body shape for every match (default: only when 8 or fewer match)"}},"additionalProperties":false}`

	schemaServiceCall = `{"type":"object","required":["service","method","path"],"properties":{` +
		`"service":{"type":"string","description":"service name from catalogue"},` +
		`"method":{"type":"string","enum":["GET","POST","PUT","PATCH","DELETE","HEAD"]},` +
		`"path":{"type":"string","description":"path below the service base URL, e.g. /profiles/abc (no query string)"},` +
		`"query":{"type":"object","description":"query parameters; values may be strings, numbers, booleans or arrays of those"},` +
		`"body":{"description":"JSON request body"},` +
		`"form":{"type":"object","description":"multipart/form-data text fields (use with files for uploads)"},` +
		`"files":{"type":"object","additionalProperties":{"type":"string"},"description":"multipart file parts: field name -> file ref (ws/... or artifacts/... on this device)"},` +
		`"headers":{"type":"object","additionalProperties":{"type":"string"},"description":"extra request headers (credentials are added by messh; Authorization, Cookie and Host cannot be set)"}},"additionalProperties":false}`

	schemaModels = `{"type":"object","required":["service"],"properties":{"service":{"type":"string","description":"openai-kind service name from catalogue"}},"additionalProperties":false}`

	schemaChat = `{"type":"object","required":["service","model","messages"],"properties":{` +
		`"service":{"type":"string","description":"openai-kind service name from catalogue"},` +
		`"model":{"type":"string","description":"model id from openai_models"},` +
		`"messages":{"type":"array","minItems":1,"items":{"type":"object","required":["role","content"],"properties":{"role":{"type":"string","enum":["system","user","assistant"]},"content":{"type":"string"}},"additionalProperties":false}},` +
		`"temperature":{"type":"number"},"max_tokens":{"type":"integer","minimum":1}},"additionalProperties":false}`
)

// genericTools returns the fixed tools whose kinds have at least one
// reachable service; names lists those services for the description.
func genericTools(rest, restWithSpec, openai []string) []*mcp.Tool {
	var out []*mcp.Tool
	if len(restWithSpec) > 0 {
		out = append(out, &mcp.Tool{
			Name:        toolDescribe,
			Title:       "Describe a REST service",
			Description: "List or search the operations of a service's OpenAPI document (method, path, summary, parameters, request body shape), paged. Use before service_call. Services: " + strings.Join(restWithSpec, ", ") + ".",
			InputSchema: rawSchema(schemaDescribe),
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
		})
	}
	if len(rest) > 0 {
		out = append(out, &mcp.Tool{
			Name:  toolCall,
			Title: "Call a REST service",
			Description: "Make an HTTP request to a registered local service; messh adds its credentials. JSON replies come back as JSON, text as text, " +
				"and binary replies (audio, images, files) are saved on this device and returned as a file ref you can copy with the mesh copy tool. " +
				"Upload files with files {field: ref}. Requires approval on the device. Services: " + strings.Join(rest, ", ") + ".",
			InputSchema: rawSchema(schemaServiceCall),
		})
	}
	if len(openai) > 0 {
		out = append(out,
			&mcp.Tool{
				Name:        toolModels,
				Title:       "List models of an OpenAI-compatible service",
				Description: "List the model ids an OpenAI-compatible local service (Ollama, LM Studio, Unsloth Studio, ...) serves. Services: " + strings.Join(openai, ", ") + ".",
				InputSchema: rawSchema(schemaModels),
				Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
			},
			&mcp.Tool{
				Name:        toolChat,
				Title:       "Chat with a local model",
				Description: "Send a chat completion (non-streaming) to an OpenAI-compatible local service and return the assistant text and token usage. Requires approval on the device.",
				InputSchema: rawSchema(schemaChat),
			})
	}
	return out
}

func catalogueTool() *mcp.Tool {
	return &mcp.Tool{
		Name:  toolCatalogue,
		Title: "Services on this device",
		Description: "START HERE to use services on this device: lists the local services (voice generation, LLM servers, APIs, MCP servers) the owner registered, " +
			"with description, tags, whether each is up, and exactly which tools use it. Optional filters tag and kind. Tool names are shown without the device prefix.",
		InputSchema: rawSchema(schemaCatalogue),
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, Title: "Service catalogue"},
	}
}
