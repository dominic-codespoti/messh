// Package recipes stores immutable owner-defined job recipes.
package recipes

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	"messh/internal/files"
	"messh/internal/state"
)

const (
	maxRegistryBytes = 4 << 20
	maxDefinitionBytes = 256 << 10
	maxSchemaBytes = 64 << 10
	maxArgs = 256
	maxInputs = 64
	maxEnv = 128
	maxTimeoutSeconds = 7 * 24 * 60 * 60
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Arg is exactly one fixed argv item or one parameter value. Parameter values
// occupy their own argv slot; they are never interpolated into shell text.
type Arg struct {
	Value *string `json:"value,omitempty"`
	Parameter string `json:"parameter,omitempty"`
}
// Definition fixes every execution setting except values explicitly bound to
// validated recipe parameters.
type Definition struct {
	Name string `json:"name"`
	Version string `json:"version"`
	Description string `json:"description,omitempty"`
	Command string `json:"command"`
	Args []Arg `json:"args,omitempty"`
	Env map[string]string `json:"env,omitempty"`
	EnvParameters map[string]string `json:"env_parameters,omitempty"`
	Workspace string `json:"workspace,omitempty"`
	Cwd string `json:"cwd,omitempty"`
	Inputs []string `json:"inputs,omitempty"`
	Resources Claims `json:"resources,omitempty"`
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	Parameters json.RawMessage `json:"parameters"`
	Results []string `json:"results,omitempty"`
}

// Claims are fixed resource requirements declared by the recipe owner.
type Claims struct {
	GPUs []int `json:"gpus,omitempty"`
	VRAMMB int `json:"vram_mb,omitempty"`
	MemMB int `json:"mem_mb,omitempty"`
	CPUs int `json:"cpus,omitempty"`
}

type record struct {
	Definition Definition `json:"definition"`
	Digest string `json:"digest"`
	Disabled bool `json:"disabled,omitempty"`
}
type disk struct { Version int `json:"version"`; Recipes []record `json:"recipes"` }
type compiled struct { record; schema *jsonschema.Resolved }

// Registry owns the durable immutable recipe catalog.
type Registry struct { mu sync.RWMutex; path string; recipes map[string]compiled }

// Recipe is the discoverable definition and digest.
type Recipe struct { Definition `json:"definition"`; Digest string `json:"digest"`; Disabled bool `json:"disabled"` }

// Invocation is the resolved safe argv/env/input execution snapshot.
type Invocation struct { Command string; Args []string; Env map[string]string; Workspace,Cwd string; Inputs []string; Resources Claims; TimeoutSeconds int; Parameters json.RawMessage; Snapshot json.RawMessage; Digest string }

func key(name, version string) string { return name + "\x00" + version }

// New loads the registry strictly. A corrupt or overly permissive file is a
// startup error; no partial catalog is ever exposed.
func New(path string) (*Registry,error) {
	r:=&Registry{path:path,recipes:map[string]compiled{}}
	st,err:=os.Lstat(path)
	if errors.Is(err,os.ErrNotExist) { return r,nil }; if err!=nil{return nil,err}
	if !st.Mode().IsRegular(){return nil,errors.New("recipe registry must be a regular file")}; if st.Mode().Perm()!=0o600{return nil,fmt.Errorf("recipe registry permissions must be 0600, got %04o",st.Mode().Perm())}
	b,err:=os.ReadFile(path); if err!=nil{return nil,err}
	if len(b)>maxRegistryBytes{return nil,fmt.Errorf("recipe registry exceeds %d bytes",maxRegistryBytes)}
	var d disk; dec:=json.NewDecoder(bytes.NewReader(b)); dec.DisallowUnknownFields(); if err=dec.Decode(&d);err!=nil{return nil,fmt.Errorf("decode recipe registry: %w",err)}; if err=requireEOF(dec);err!=nil{return nil,err}; if d.Version!=1{return nil,fmt.Errorf("unsupported recipe registry version %d",d.Version)}
	for _,v:=range d.Recipes { c,err:=compile(v);if err!=nil{return nil,fmt.Errorf("invalid stored recipe %s@%s: %w",v.Definition.Name,v.Definition.Version,err)};k:=key(v.Definition.Name,v.Definition.Version);if _,exists:=r.recipes[k];exists{return nil,fmt.Errorf("duplicate stored recipe %s@%s",v.Definition.Name,v.Definition.Version)};r.recipes[k]=c }
	return r,nil
}
func requireEOF(d *json.Decoder)error{var v any;if err:=d.Decode(&v);err!=io.EOF{if err==nil{return errors.New("multiple JSON values")};return err};return nil}
func canonical(v any)([]byte,error){return json.Marshal(v)}
func digest(b []byte)string{s:=sha256.Sum256(b);return hex.EncodeToString(s[:])}

func compile(v record)(compiled,error){
	d:=cloneDefinition(v.Definition)
	if !namePattern.MatchString(d.Name)||len(d.Name)>128{return compiled{},errors.New("name must match [A-Za-z0-9][A-Za-z0-9._-]{0,127}")}
	if strings.TrimSpace(d.Version)==""||len(d.Version)>64||strings.ContainsAny(d.Version,"\x00/\\") {return compiled{},errors.New("version must be 1-64 non-path characters")}
	if len(d.Description)>2048||strings.TrimSpace(d.Command)==""||len(d.Command)>4096||strings.ContainsRune(d.Command,0){return compiled{},errors.New("description or command is invalid")}
	if len(d.Args)>maxArgs||len(d.Inputs)>maxInputs||len(d.Env)+len(d.EnvParameters)>maxEnv{return compiled{},errors.New("recipe exceeds argv, input, or environment limits")}
	if d.TimeoutSeconds<0||d.TimeoutSeconds>maxTimeoutSeconds{return compiled{},errors.New("timeout_seconds must be between 0 and 604800")}
	if len(d.Results) > 128 { return compiled{}, errors.New("too many declared results") }
	for _, result := range d.Results { clean := path.Clean(result); if strings.TrimSpace(result)=="" || strings.ContainsAny(result, "\\:\x00") || strings.HasPrefix(result, "/") || clean==".." || strings.HasPrefix(clean, "../") { return compiled{}, errors.New("results must be workspace-relative paths without escapes") }; if _, err := files.ParseRef("ws/w/"+clean); err != nil { return compiled{}, fmt.Errorf("result path: %w", err) } }
	if d.Workspace != "" { if strings.ContainsAny(d.Workspace, "/\\") { return compiled{}, errors.New("workspace must be a single workspace name") }; if _,err:=files.ParseRef("ws/"+d.Workspace);err!=nil{return compiled{},fmt.Errorf("workspace: %w",err)} }
	if d.Cwd != "" { clean:=path.Clean(d.Cwd);if strings.ContainsAny(d.Cwd,"\\:\x00")||strings.HasPrefix(d.Cwd,"/")||clean==".."||strings.HasPrefix(clean,"../"){return compiled{},errors.New("cwd must remain inside the workspace")};if _,err:=files.ParseRef("ws/w/"+clean);err!=nil{return compiled{},fmt.Errorf("cwd: %w",err)};d.Cwd=clean }
	for _,ref:=range d.Inputs { if _,err:=files.ParseRef(ref);err!=nil{return compiled{},fmt.Errorf("input reference: %w",err)} }
	if d.Resources.VRAMMB<0||d.Resources.MemMB<0||d.Resources.CPUs<0||d.Resources.VRAMMB>64<<20||d.Resources.MemMB>64<<20||d.Resources.CPUs>4096{return compiled{},errors.New("resource claims must be non-negative and realistic")}
	for _,g:=range d.Resources.GPUs { if g<0||g>63{return compiled{},fmt.Errorf("invalid GPU index %d",g)} };sort.Ints(d.Resources.GPUs)
	for _, a := range d.Args {
		if (a.Parameter == "") == (a.Value == nil) { return compiled{}, errors.New("each arg must specify exactly one of value or parameter") }
		if a.Value != nil && strings.ContainsRune(*a.Value, 0) { return compiled{}, errors.New("arg contains NUL byte") }
	}
	for k,v:=range d.Env { if k==""||strings.ContainsAny(k,"=\x00")||strings.ContainsRune(v,0){return compiled{},fmt.Errorf("invalid fixed environment entry %q",k)} }
	for k,p:=range d.EnvParameters { if k==""||strings.ContainsAny(k,"=\x00")||p=="" {return compiled{},fmt.Errorf("invalid environment parameter binding %q",k)}; if _,ok:=d.Env[k];ok{return compiled{},fmt.Errorf("environment key %q is both fixed and bound",k)} }
	if len(d.Parameters)==0||len(d.Parameters)>maxSchemaBytes{return compiled{},errors.New("parameters must be a JSON Schema object no larger than 65536 bytes")}
	var schemaMap map[string]any; dec:=json.NewDecoder(bytes.NewReader(d.Parameters));dec.UseNumber();dec.DisallowUnknownFields();if err:=dec.Decode(&schemaMap);err!=nil{return compiled{},fmt.Errorf("parameters schema: %w",err)};if err:=requireEOF(dec);err!=nil{return compiled{},err};if schemaMap==nil{return compiled{},errors.New("parameters schema must be an object")}
	var sch jsonschema.Schema; if err:=json.Unmarshal(d.Parameters,&sch);err!=nil{return compiled{},fmt.Errorf("parameters schema: %w",err)}
	// The schema is self-contained: external retrieval would make a digest's
	// meaning depend on mutable network state.
	if hasExternalRef(schemaMap){return compiled{},errors.New("external schema references are not allowed")}
	resolved,err:=sch.Resolve(nil);if err!=nil{return compiled{},fmt.Errorf("resolve parameters schema: %w",err)}
	if schemaMap["type"] != "object" { return compiled{}, errors.New("parameters schema root type must be object") }
	canonicalSchema, err := canonical(schemaMap); if err != nil { return compiled{}, err }; d.Parameters = canonicalSchema
	for _, a := range d.Args { if a.Parameter!="" && !schemaHasProperty(schemaMap,a.Parameter) { return compiled{},fmt.Errorf("argv binding references undeclared parameter %q",a.Parameter) } }
	for _, p := range d.EnvParameters { if !schemaHasProperty(schemaMap,p) { return compiled{},fmt.Errorf("environment binding references undeclared parameter %q",p) } }
	canonicalDef,err:=canonical(d);if err!=nil{return compiled{},err};if len(canonicalDef)>maxDefinitionBytes{return compiled{},errors.New("recipe definition exceeds 262144 bytes")};sum:=digest(canonicalDef);if v.Digest!=""&&v.Digest!=sum{return compiled{},errors.New("stored recipe digest mismatch")};v.Definition=d;v.Digest=sum
	return compiled{record:v,schema:resolved},nil
}
func hasExternalRef(v any)bool{switch x:=v.(type){case map[string]any:for k,v:=range x{if (k=="$ref"||k=="$dynamicRef"||k=="$id") {s,_:=v.(string);if !strings.HasPrefix(s,"#"){return true}};if hasExternalRef(v){return true}};case []any:for _,v:=range x{if hasExternalRef(v){return true}}};return false}
func schemaHasProperty(s map[string]any,name string)bool{p,ok:=s["properties"].(map[string]any);if ok {if _,found:=p[name];found{return true}};return false}

// Publish creates a new name/version. Existing versions, including disabled
// versions, are immutable and cannot be replaced.
func(r *Registry)Publish(d Definition)(Recipe,error){c,err:=compile(record{Definition:d});if err!=nil{return Recipe{},invalid(err)};r.mu.Lock();defer r.mu.Unlock();k:=key(d.Name,d.Version);if _,ok:=r.recipes[k];ok{return Recipe{},coded("recipe_version_conflict",fmt.Sprintf("recipe %s@%s already exists and is immutable",d.Name,d.Version),nil)};r.recipes[k]=c;if err=r.saveLocked();err!=nil{delete(r.recipes,k);return Recipe{},coded("recipe_registry_write_failed","could not persist recipe registry",err)};return public(c),nil}
func public(c compiled)Recipe{ d:=cloneDefinition(c.Definition); return Recipe{Definition:d,Digest:c.Digest,Disabled:c.Disabled} }
func cloneDefinition(d Definition) Definition {
 d.Args = append([]Arg(nil), d.Args...); for i := range d.Args { if d.Args[i].Value != nil { value := *d.Args[i].Value; d.Args[i].Value = &value } }
 d.Env = cloneStringMap(d.Env); d.EnvParameters = cloneStringMap(d.EnvParameters); d.Inputs = append([]string(nil), d.Inputs...); d.Results = append([]string(nil), d.Results...); d.Resources.GPUs = append([]int(nil), d.Resources.GPUs...); d.Parameters = append(json.RawMessage(nil), d.Parameters...); return d
}
func cloneStringMap(m map[string]string) map[string]string { if m == nil { return nil }; out := make(map[string]string, len(m)); for k, v := range m { out[k] = v }; return out }
func(r *Registry)saveLocked()error{list:=make([]record,0,len(r.recipes));for _,c:=range r.recipes{list=append(list,c.record)};sort.Slice(list,func(i,j int)bool{if list[i].Definition.Name==list[j].Definition.Name{return list[i].Definition.Version<list[j].Definition.Version};return list[i].Definition.Name<list[j].Definition.Name});b,err:=json.Marshal(disk{Version:1,Recipes:list});if err!=nil{return err};if len(b)>maxRegistryBytes{return fmt.Errorf("recipe registry exceeds %d bytes",maxRegistryBytes)};return state.WriteFileAtomic(r.path,b,0o600)}
// List returns all recipes in stable name/version order.
func(r *Registry)List(includeDisabled bool)[]Recipe{r.mu.RLock();defer r.mu.RUnlock();out:=[]Recipe{};for _,c:=range r.recipes{if includeDisabled||!c.Disabled{out=append(out,public(c))}};sort.Slice(out,func(i,j int)bool{if out[i].Name==out[j].Name{return out[i].Version<out[j].Version};return out[i].Name<out[j].Name});return out}
// Get resolves an exact immutable selector. Empty version/digest are allowed
// for discovery only; execution requires both through Resolve.
func(r *Registry)Get(name,version,dig string)(Recipe,error){r.mu.RLock();defer r.mu.RUnlock();var found *compiled;for _,c:=range r.recipes{if c.Definition.Name==name&&(version==""||c.Definition.Version==version){if found==nil||c.Definition.Version>found.Definition.Version{cc:=c;found=&cc}}};if found==nil{return Recipe{},coded("recipe_not_found",fmt.Sprintf("recipe %s@%s not found",name,version),ErrNotFound)};if dig!=""&&found.Digest!=dig{return Recipe{},coded("recipe_digest_mismatch","recipe digest does not match",nil)};return public(*found),nil}
// Disable excludes this version from future resolution without changing its definition or digest.
func(r *Registry)Disable(name,version string)(Recipe,error){r.mu.Lock();defer r.mu.Unlock();k:=key(name,version);c,ok:=r.recipes[k];if !ok{return Recipe{},coded("recipe_not_found",fmt.Sprintf("recipe %s@%s not found",name,version),ErrNotFound)};if !c.Disabled{c.Disabled=true;r.recipes[k]=c;if err:=r.saveLocked();err!=nil{c.Disabled=false;r.recipes[k]=c;return Recipe{},coded("recipe_registry_write_failed","could not persist recipe registry",err)}};return public(c),nil}

// Resolve validates parameters with the complete JSON Schema implementation and
// builds argv using whole-argument bindings only.
func(r *Registry)Resolve(name,version,dig string,parameters json.RawMessage)(Invocation,error){
 r.mu.RLock();defer r.mu.RUnlock()
 c,ok:=r.recipes[key(name,version)];if !ok{return Invocation{},coded("recipe_not_found",fmt.Sprintf("recipe %s@%s not found",name,version),ErrNotFound)}
 if c.Digest!=dig{return Invocation{},coded("recipe_digest_mismatch","recipe digest does not match",nil)}
 if c.Disabled{return Invocation{},coded("recipe_disabled",fmt.Sprintf("recipe %s@%s is disabled",name,version),nil)}
 d:=c.Definition
 dec:=json.NewDecoder(bytes.NewReader(parameters));var values map[string]any
 if err:=dec.Decode(&values);err!=nil{return Invocation{},coded("invalid_recipe_parameters",fmt.Sprintf("recipe parameters must be a JSON object: %v",err),err)}
 if values==nil{return Invocation{},coded("invalid_recipe_parameters","recipe parameters must be a JSON object",nil)}
 if err:=requireEOF(dec);err!=nil{return Invocation{},coded("invalid_recipe_parameters",err.Error(),err)}
 if err:=c.schema.Validate(values);err!=nil{return Invocation{},coded("invalid_recipe_parameters",fmt.Sprintf("recipe parameters invalid: %v",err),err)}
 args:=make([]string,0,len(d.Args));for _,a:=range d.Args{if a.Parameter==""{args=append(args,*a.Value);continue};v:=values[a.Parameter];arg,err:=parameterString(v);if err!=nil{return Invocation{},coded("invalid_recipe_parameters",fmt.Sprintf("parameter %q: %v",a.Parameter,err),err)};args=append(args,arg)}
 env:=make(map[string]string,len(d.Env)+len(d.EnvParameters));for k,v:=range d.Env{env[k]=v};for k,p:=range d.EnvParameters{v,err:=parameterString(values[p]);if err!=nil{return Invocation{},coded("invalid_recipe_parameters",fmt.Sprintf("parameter %q: %v",p,err),err)};env[k]=v}
 paramBytes,err:=canonical(values);if err!=nil{return Invocation{},coded("invalid_recipe_parameters","could not canonicalize recipe parameters",err)};snap,err:=canonical(d);if err!=nil{return Invocation{},err}
 return Invocation{Command:d.Command,Args:args,Env:env,Workspace:d.Workspace,Cwd:d.Cwd,Inputs:append([]string(nil),d.Inputs...),Resources:Claims{GPUs:append([]int(nil),d.Resources.GPUs...),VRAMMB:d.Resources.VRAMMB,MemMB:d.Resources.MemMB,CPUs:d.Resources.CPUs},TimeoutSeconds:d.TimeoutSeconds,Parameters:paramBytes,Snapshot:snap,Digest:c.Digest},nil
}
func parameterString(v any)(string,error){switch x:=v.(type){case string:if strings.ContainsRune(x,0){return "",errors.New("string parameters must not contain NUL bytes")};return x,nil;case bool:b,_:=json.Marshal(x);return string(b),nil;case float64:b,err:=json.Marshal(x);return string(b),err;case nil:return "null",nil;default:return "",errors.New("argv and env bindings require a scalar parameter")}}

// Path returns the catalog's durable file path.
func Path(root string)string{return filepath.Join(root,"recipes.json")}
