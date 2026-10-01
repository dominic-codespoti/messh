package catalog

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// apiDoc is a parsed OpenAPI 3.x / Swagger 2.0 JSON document, kept just
// deeply enough to search operations and summarise their inputs.
type apiDoc struct {
	Title, Version string
	Ops            []apiOp
	hash           [32]byte
	root           map[string]any
}

type apiOp struct {
	Method, Path string
	ID           string
	Summary      string
	Description  string
	Tags         []string
	Deprecated   bool
	raw          map[string]any
	pathParams   []any // parameters declared on the path item
}

var httpMethods = []string{"get", "put", "post", "delete", "options", "head", "patch"}

// parseOpenAPI parses spec bytes (JSON only; YAML needs a dependency messh avoids).
func parseOpenAPI(b []byte) (*apiDoc, error) {
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		return nil, fmt.Errorf("spec is not JSON: %w", err)
	}
	if _, ok := root["openapi"]; !ok {
		if _, ok := root["swagger"]; !ok {
			return nil, fmt.Errorf("document has neither an openapi nor a swagger field")
		}
	}
	d := &apiDoc{root: root, hash: sha256.Sum256(b)}
	if info, ok := root["info"].(map[string]any); ok {
		d.Title, _ = info["title"].(string)
		d.Version, _ = info["version"].(string)
	}
	paths, _ := root["paths"].(map[string]any)
	for p, item := range paths {
		im, ok := item.(map[string]any)
		if !ok {
			continue
		}
		shared, _ := im["parameters"].([]any)
		for _, m := range httpMethods {
			om, ok := im[m].(map[string]any)
			if !ok {
				continue
			}
			op := apiOp{Method: strings.ToUpper(m), Path: p, raw: om, pathParams: shared}
			op.ID, _ = om["operationId"].(string)
			op.Summary, _ = om["summary"].(string)
			op.Description, _ = om["description"].(string)
			op.Deprecated, _ = om["deprecated"].(bool)
			if ts, ok := om["tags"].([]any); ok {
				for _, t := range ts {
					if s, ok := t.(string); ok {
						op.Tags = append(op.Tags, s)
					}
				}
			}
			d.Ops = append(d.Ops, op)
		}
	}
	sort.Slice(d.Ops, func(i, j int) bool {
		if d.Ops[i].Path != d.Ops[j].Path {
			return d.Ops[i].Path < d.Ops[j].Path
		}
		return slices.Index(httpMethods, strings.ToLower(d.Ops[i].Method)) < slices.Index(httpMethods, strings.ToLower(d.Ops[j].Method))
	})
	return d, nil
}

// template returns the documented path template matching a concrete path
// ("/profiles/abc" → "/profiles/{id}"), preferring templates with the most
// literal segments, or the path itself when none matches.
func (d *apiDoc) template(method, path string) string {
	if d == nil {
		return path
	}
	best, bestLiterals := "", -1
	segs := splitPath(path)
	for _, op := range d.Ops {
		if !strings.EqualFold(op.Method, method) {
			continue
		}
		ps := splitPath(op.Path)
		if len(ps) != len(segs) {
			continue
		}
		lit, ok := 0, true
		for i, s := range ps {
			if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
				continue
			}
			if s != segs[i] {
				ok = false
				break
			}
			lit++
		}
		if ok && lit > bestLiterals {
			best, bestLiterals = op.Path, lit
		}
	}
	if best == "" {
		return path
	}
	return best
}

// opView is what service_describe returns for one operation.
type opView struct {
	Method      string   `json:"method"`
	Path        string   `json:"path"`
	Summary     string   `json:"summary,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Deprecated  bool     `json:"deprecated,omitempty"`
	Description string   `json:"description,omitempty"`
	Parameters  []any    `json:"parameters,omitempty"`
	RequestBody any      `json:"request_body,omitempty"`
}

func (op *apiOp) summary() opView {
	return opView{Method: op.Method, Path: op.Path, Summary: firstNonEmpty(op.Summary, firstLine(op.Description)), Tags: op.Tags, Deprecated: op.Deprecated}
}

func (d *apiDoc) detail(op *apiOp) opView {
	v := op.summary()
	v.Description = clip(op.Description, 400)
	seen := map[string]bool{}
	opParams, _ := op.raw["parameters"].([]any)
	for _, list := range [][]any{opParams, op.pathParams} {
		for _, p := range list {
			pm, ok := d.deref(p).(map[string]any)
			if !ok {
				continue
			}
			name, _ := pm["name"].(string)
			in, _ := pm["in"].(string)
			if in == "body" {
				v.RequestBody = map[string]any{"content_type": "application/json", "schema": d.shape(pm["schema"], 0, nil)}
				continue
			}
			if seen[in+":"+name] {
				continue
			}
			seen[in+":"+name] = true
			entry := map[string]any{"name": name, "in": in}
			if req, _ := pm["required"].(bool); req {
				entry["required"] = true
			}
			schema := pm["schema"]
			if schema == nil {
				schema = pm // swagger 2.0 parameters carry type inline
			}
			entry["type"] = d.shape(schema, 1, nil)
			if desc, _ := pm["description"].(string); desc != "" {
				entry["description"] = clip(desc, 160)
			}
			v.Parameters = append(v.Parameters, entry)
		}
	}
	if rb, ok := d.deref(op.raw["requestBody"]).(map[string]any); ok {
		content, _ := rb["content"].(map[string]any)
		body := map[string]any{}
		if req, _ := rb["required"].(bool); req {
			body["required"] = true
		}
		mimes := make([]string, 0, len(content))
		for m := range content {
			mimes = append(mimes, m)
		}
		sort.Strings(mimes)
		for _, m := range mimes {
			cm, _ := content[m].(map[string]any)
			body[m] = d.shape(cm["schema"], 0, nil)
		}
		v.RequestBody = body
	}
	return v
}

// deref follows local "#/..." references; anything else is returned as is.
func (d *apiDoc) deref(v any) any {
	for range 8 {
		m, ok := v.(map[string]any)
		if !ok {
			return v
		}
		ref, ok := m["$ref"].(string)
		if !ok || !strings.HasPrefix(ref, "#/") {
			return v
		}
		var cur any = d.root
		for part := range strings.SplitSeq(ref[2:], "/") {
			part = strings.NewReplacer("~1", "/", "~0", "~").Replace(part)
			cm, ok := cur.(map[string]any)
			if !ok {
				return nil
			}
			cur = cm[part]
		}
		v = cur
	}
	return v
}

// shape renders a JSON schema as a compact example-like structure:
// {"text":"string","speed?":"number","files":["binary file"]}; a trailing ?
// marks optional properties.
func (d *apiDoc) shape(schema any, depth int, visiting []string) any {
	if ref, ok := schema.(map[string]any); ok {
		if r, _ := ref["$ref"].(string); r != "" {
			if slices.Contains(visiting, r) {
				return "(recursive " + r[strings.LastIndex(r, "/")+1:] + ")"
			}
			visiting = append(visiting[:len(visiting):len(visiting)], r)
		}
	}
	s, ok := d.deref(schema).(map[string]any)
	if !ok {
		return "any"
	}
	for _, k := range []string{"oneOf", "anyOf"} {
		if alts, ok := s[k].([]any); ok && len(alts) > 0 {
			var parts []string
			for _, a := range alts {
				parts = append(parts, fmt.Sprint(d.shape(a, depth+1, visiting)))
			}
			return strings.Join(parts, " | ")
		}
	}
	if alls, ok := s["allOf"].([]any); ok && len(alls) > 0 {
		merged := map[string]any{}
		for _, a := range alls {
			if m, ok := d.shape(a, depth, visiting).(map[string]any); ok {
				for k, v := range m {
					merged[k] = v
				}
			}
		}
		if len(merged) > 0 {
			return merged
		}
	}
	typ, _ := s["type"].(string)
	if typ == "" {
		if _, ok := s["properties"]; ok {
			typ = "object"
		}
	}
	switch typ {
	case "object":
		props, _ := s["properties"].(map[string]any)
		if len(props) == 0 {
			return "object"
		}
		if depth >= 3 {
			return "object"
		}
		req := map[string]bool{}
		if rs, ok := s["required"].([]any); ok {
			for _, r := range rs {
				if rn, ok := r.(string); ok {
					req[rn] = true
				}
			}
		}
		names := make([]string, 0, len(props))
		for n := range props {
			names = append(names, n)
		}
		sort.Strings(names)
		out := map[string]any{}
		for i, n := range names {
			if i >= 30 {
				out["..."] = fmt.Sprintf("%d more properties", len(names)-i)
				break
			}
			key := n
			if !req[n] {
				key += "?"
			}
			out[key] = d.shape(props[n], depth+1, visiting)
		}
		return out
	case "array":
		if depth >= 3 {
			return "array"
		}
		return []any{d.shape(s["items"], depth+1, visiting)}
	case "string":
		if f, _ := s["format"].(string); f == "binary" {
			return "binary file"
		}
		if en, ok := s["enum"].([]any); ok && len(en) > 0 {
			var parts []string
			for i, e := range en {
				if i >= 8 {
					parts = append(parts, "...")
					break
				}
				parts = append(parts, fmt.Sprint(e))
			}
			return "string (" + strings.Join(parts, "|") + ")"
		}
		return "string"
	case "":
		if en, ok := s["enum"].([]any); ok && len(en) > 0 {
			return fmt.Sprint("one of ", en)
		}
		return "any"
	}
	return typ
}

// search returns operations matching every whitespace-separated term of query
// (case-insensitive, over path, summary, operationId, tags, description) and
// the exact tag when set.
func (d *apiDoc) search(query, tag string) []*apiOp {
	terms := strings.Fields(strings.ToLower(query))
	var out []*apiOp
	for i := range d.Ops {
		op := &d.Ops[i]
		if tag != "" && !slices.ContainsFunc(op.Tags, func(t string) bool { return strings.EqualFold(t, tag) }) {
			continue
		}
		hay := strings.ToLower(op.Method + " " + op.Path + " " + op.Summary + " " + op.ID + " " + strings.Join(op.Tags, " ") + " " + op.Description)
		ok := true
		for _, t := range terms {
			if !strings.Contains(hay, t) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, op)
		}
	}
	return out
}

func (d *apiDoc) tagCounts() map[string]int {
	m := map[string]int{}
	for _, op := range d.Ops {
		for _, t := range op.Tags {
			m[t]++
		}
	}
	return m
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return clip(s, 120)
}

// clip shortens s to n runes with an ellipsis.
func clip(s string, n int) string {
	if n <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
