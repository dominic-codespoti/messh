package jobs

import (
 "context"
 "encoding/json"
 "errors"
 "fmt"
 "strings"

 "github.com/modelcontextprotocol/go-sdk/mcp"
 "messh/internal/provider"
 "messh/internal/recipes"
)

// parseOptionalRecipeSubmit validates the selection, then resolves an immutable recipe into the ordinary scheduler request.
func (p *Provider) parseOptionalRecipeSubmit(raw json.RawMessage) (*request, *recipes.Recipe, json.RawMessage, error) {
 if err := ValidateSubmission(raw); err != nil { return nil, nil, nil, fmt.Errorf("invalid job_submit arguments: %w", err) }
 var a submitArgs
 dec := json.NewDecoder(strings.NewReader(string(raw)))
 dec.DisallowUnknownFields()
 if err := dec.Decode(&a); err != nil { return nil, nil, nil, err }
 if a.Recipe == nil { req, err := p.parseSubmit(raw); return req, nil, nil, err }
 if p.RecipeRegistry == nil { return nil, nil, nil, errors.New("recipe registry is unavailable") }
 selector := a.Recipe
 inv, err := p.RecipeRegistry.Resolve(selector.Name, selector.Version, selector.Digest, selector.Parameters)
 if err != nil { return nil, nil, nil, err }
 recipe, err := p.RecipeRegistry.Get(selector.Name, selector.Version, selector.Digest)
 if err != nil { return nil, nil, nil, err }
 generated, err := json.Marshal(struct {
  RequestID string `json:"request_id"`
  Command string `json:"command"`
  Args []string `json:"args"`
  Cwd string `json:"cwd,omitempty"`
  Env map[string]string `json:"env,omitempty"`
  Inputs []string `json:"inputs,omitempty"`
  Workspace string `json:"workspace,omitempty"`
  Resources Claims `json:"resources,omitempty"`
  TimeoutSeconds int `json:"timeout_seconds,omitempty"`
  Label string `json:"label,omitempty"`
 }{a.RequestID, inv.Command, inv.Args, inv.Cwd, inv.Env, inv.Inputs, inv.Workspace, Claims{GPUs:inv.Resources.GPUs, VRAMMB:int64(inv.Resources.VRAMMB), MemMB:int64(inv.Resources.MemMB), CPUs:inv.Resources.CPUs}, inv.TimeoutSeconds, selector.Name+"@"+selector.Version})
 if err != nil { return nil, nil, nil, err }
 req, err := p.parseSubmit(generated)
 if err != nil { return nil, nil, nil, err }
 req.RecipeID, req.RecipeVersion, req.RecipeDigest = selector.Name, selector.Version, recipe.Digest
 req.RecipeSnapshot = append(json.RawMessage(nil), inv.Snapshot...)
 req.RecipeParameters = append(json.RawMessage(nil), inv.Parameters...)
 return req, &recipe, append(json.RawMessage(nil), inv.Parameters...), nil
}

// RecipeTools are read-only agent-facing catalog discovery methods.
func (p *Provider) RecipeTools() []provider.Tool {
 return []provider.Tool{
  {Class:provider.ClassInfo, Def:&mcp.Tool{Name:"recipe_list", Description:"List owner-published job recipes and their versions, digests, full parameter schemas, fixed requirements, and declared results.", InputSchema:json.RawMessage("{\"type\":\"object\",\"properties\":{\"include_disabled\":{\"type\":\"boolean\",\"description\":\"Include disabled versions for inspection.\"}},\"additionalProperties\":false}"), Annotations:readOnly("List available job recipes")}},
  {Class:provider.ClassInfo, Def:&mcp.Tool{Name:"recipe_get", Description:"Get an exact owner-published job recipe by name, version, and digest, including its typed parameter schema and fixed execution requirements.", InputSchema:json.RawMessage("{\"type\":\"object\",\"properties\":{\"name\":{\"type\":\"string\"},\"version\":{\"type\":\"string\"},\"digest\":{\"type\":\"string\"}},\"required\":[\"name\",\"version\",\"digest\"],\"additionalProperties\":false}"), Annotations:readOnly("Get job recipe")}},
 }
}

type recipeListArgs struct { IncludeDisabled bool `json:"include_disabled,omitempty"` }
type recipeGetArgs struct { Name string `json:"name"`; Version string `json:"version"`; Digest string `json:"digest"` }

func (p *Provider) callRecipeList(_ context.Context, raw json.RawMessage, _ provider.Caller) (*mcp.CallToolResult, error) {
 var a recipeListArgs
 if err := decodeArgs(raw, &a); err != nil { return nil, err }
 if p.RecipeRegistry == nil { return nil, errors.New("recipe registry unavailable") }
 return provider.JSONResult(p.RecipeRegistry.List(a.IncludeDisabled))
}
func (p *Provider) callRecipeGet(_ context.Context, raw json.RawMessage, _ provider.Caller) (*mcp.CallToolResult, error) {
 var a recipeGetArgs
 if err := decodeArgs(raw, &a); err != nil { return nil, err }
 if p.RecipeRegistry == nil { return nil, errors.New("recipe registry unavailable") }
 r, err := p.RecipeRegistry.Get(a.Name, a.Version, a.Digest)
 if err != nil { return nil, err }
 return provider.JSONResult(r)
}
