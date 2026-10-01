package catalog

import (
	"strings"
)

// opPattern is a parsed allow/deny/auto entry for a REST operation:
// "METHOD /path/pattern". METHOD may be "*" or "ANY".
type opPattern struct {
	method string // upper-case, "" = any
	segs   []string
}

// parseOpPattern parses "GET /profiles/{id}". ok is false when the entry has
// no leading path (an MCP-style name).
func parseOpPattern(entry string) (opPattern, bool) {
	m, p, found := strings.Cut(strings.TrimSpace(entry), " ")
	p = strings.TrimSpace(p)
	if !found || !strings.HasPrefix(p, "/") {
		return opPattern{}, false
	}
	m = strings.ToUpper(m)
	if m == "*" || m == "ANY" {
		m = ""
	}
	return opPattern{method: m, segs: splitPath(p)}, true
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

// matches reports whether the concrete (or templated) operation fits.
// "{x}" and "*" match one segment; a final "**" matches any remainder.
func (op opPattern) matches(method, path string) bool {
	if op.method != "" && op.method != strings.ToUpper(method) {
		return false
	}
	segs := splitPath(path)
	for i, ps := range op.segs {
		if ps == "**" && i == len(op.segs)-1 {
			return true
		}
		if i >= len(segs) {
			return false
		}
		if ps == "*" || (strings.HasPrefix(ps, "{") && strings.HasSuffix(ps, "}")) {
			continue
		}
		if ps != segs[i] {
			return false
		}
	}
	return len(segs) == len(op.segs)
}

// matchRESTList reports whether any entry matches the operation.
func matchRESTList(list []string, method, path string) bool {
	for _, e := range list {
		if op, ok := parseOpPattern(e); ok && op.matches(method, path) {
			return true
		}
	}
	return false
}

// matchName is a "*"-wildcard match for tool names.
func matchName(pattern, name string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == name
	}
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(name, parts[0]) {
		return false
	}
	name = name[len(parts[0]):]
	for _, p := range parts[1 : len(parts)-1] {
		i := strings.Index(name, p)
		if i < 0 {
			return false
		}
		name = name[i+len(p):]
	}
	last := parts[len(parts)-1]
	return strings.HasSuffix(name, last)
}

func matchNameList(list []string, name string) bool {
	for _, e := range list {
		if matchName(e, name) {
			return true
		}
	}
	return false
}

// permits applies the allow/deny lists to an operation. deny always wins;
// a non-empty allow list must match.
func permitsREST(s *Service, method, path string) bool {
	if matchRESTList(s.Deny, method, path) {
		return false
	}
	return len(s.Allow) == 0 || matchRESTList(s.Allow, method, path)
}

func permitsTool(s *Service, tool string) bool {
	if matchNameList(s.Deny, tool) {
		return false
	}
	return len(s.Allow) == 0 || matchNameList(s.Allow, tool)
}
