// Package catalog exposes the local services a device's owner registered
// (MCP servers, REST/OpenAPI APIs, OpenAI-compatible endpoints, plain HTTP)
// as messh tools, behind the approval gate. Credentials stay in the services
// file on the host and never appear in tool output, approvals, or logs.
package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strings"

	"messh/internal/state"
)

// Service kinds.
const (
	KindMCP     = "mcp"
	KindOpenAPI = "openapi"
	KindOpenAI  = "openai"
	KindHTTP    = "http"
)

// Service is one registered local service.
type Service struct {
	Name        string   `json:"name"`
	Kind        string   `json:"kind"`
	URL         string   `json:"url"` // base URL (for mcp: the MCP endpoint)
	Description string   `json:"description,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Auth        *Auth    `json:"auth,omitempty"`
	// Health is an optional URL or path (relative to URL) probed with GET.
	Health string `json:"health,omitempty"`
	// OpenAPI is the spec URL or path for kind openapi (default /openapi.json).
	OpenAPI string `json:"openapi,omitempty"`
	// AllowLAN lets URL point at a private-network host. Without it only
	// loopback addresses are reachable, so a hand-edited file cannot turn the
	// node into a proxy to arbitrary hosts.
	AllowLAN bool `json:"allow_lan,omitempty"`
	Auto     Auto `json:"auto,omitempty"`
	// Allow and Deny restrict operations. MCP: upstream tool names. REST: "METHOD /path".
	// Patterns: "*" in a path matches one segment, "**" the rest; in tool names "*" matches any text.
	Allow   []string `json:"allow,omitempty"`
	Deny    []string `json:"deny,omitempty"`
	Enabled *bool    `json:"enabled,omitempty"` // default true
}

// Auth is a credential header injected by messh on every request to the service.
type Auth struct {
	Header    string `json:"header"`
	Value     string `json:"value,omitempty"`      // the full header value, e.g. "Bearer sk-..."
	ValueFile string `json:"value_file,omitempty"` // file holding the full header value
}

// Auto is the owner's pre-authorisation. Everything not matched still prompts.
type Auto struct {
	// Read pre-authorises REST GET/HEAD, listing the models of an OpenAI
	// endpoint, and MCP tools whose upstream annotations say readOnlyHint.
	Read bool `json:"read,omitempty"`
	// Tools pre-authorises named operations: MCP tool names, "METHOD /path-template",
	// or the generic tool names openai_chat / service_describe.
	Tools []string `json:"tools,omitempty"`
}

// Active reports whether the service is enabled.
func (s *Service) Active() bool { return s.Enabled == nil || *s.Enabled }

var (
	nameRE   = regexp.MustCompile(`^[a-z0-9_-]{1,24}$`)
	tagRE    = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)
	headerRE = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
)

// reservedHeaders can never be the credential header: they would let a
// service entry rewrite framing or routing of every request.
var reservedHeaders = []string{"host", "content-length", "content-type", "transfer-encoding", "connection", "upgrade", "te", "trailer"}

// Validate checks a service entry.
func (s *Service) Validate() error {
	if !nameRE.MatchString(s.Name) || strings.Contains(s.Name, "__") {
		return fmt.Errorf("invalid service name %q: use 1-24 of a-z 0-9 _ - (no \"__\")", s.Name)
	}
	switch s.Kind {
	case KindMCP, KindOpenAPI, KindOpenAI, KindHTTP:
	default:
		return fmt.Errorf("service %s: kind must be mcp, openapi, openai or http, not %q", s.Name, s.Kind)
	}
	u, err := url.Parse(s.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.Hostname() == "" {
		return fmt.Errorf("service %s: url must be an absolute http(s) URL", s.Name)
	}
	if u.User != nil {
		return fmt.Errorf("service %s: put credentials in auth, not in the url", s.Name)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("service %s: url must not carry a query or fragment", s.Name)
	}
	if err := checkHost(u.Hostname(), s.AllowLAN); err != nil {
		return fmt.Errorf("service %s: %w", s.Name, err)
	}
	if len(s.Description) > 300 {
		return fmt.Errorf("service %s: description over 300 characters", s.Name)
	}
	if len(s.Tags) > 8 {
		return fmt.Errorf("service %s: at most 8 tags", s.Name)
	}
	for _, t := range s.Tags {
		if !tagRE.MatchString(t) {
			return fmt.Errorf("service %s: invalid tag %q", s.Name, t)
		}
	}
	if a := s.Auth; a != nil {
		if !headerRE.MatchString(a.Header) || slices.Contains(reservedHeaders, strings.ToLower(a.Header)) {
			return fmt.Errorf("service %s: invalid auth header %q", s.Name, a.Header)
		}
		if (a.Value == "") == (a.ValueFile == "") {
			return fmt.Errorf("service %s: auth needs exactly one of value or value_file", s.Name)
		}
		if strings.ContainsAny(a.Value, "\r\n\x00") {
			return fmt.Errorf("service %s: auth value contains control characters", s.Name)
		}
	}
	if s.Health != "" {
		if _, err := s.resolve(s.Health); err != nil {
			return fmt.Errorf("service %s: health: %w", s.Name, err)
		}
	}
	if s.OpenAPI != "" {
		if s.Kind != KindOpenAPI {
			return fmt.Errorf("service %s: openapi only applies to kind openapi", s.Name)
		}
		if _, err := s.resolve(s.OpenAPI); err != nil {
			return fmt.Errorf("service %s: openapi: %w", s.Name, err)
		}
	}
	for _, list := range [][]string{s.Allow, s.Deny, s.Auto.Tools} {
		for _, e := range list {
			if strings.TrimSpace(e) == "" {
				return fmt.Errorf("service %s: empty entry in allow/deny/auto.tools", s.Name)
			}
		}
	}
	return nil
}

// resolve turns a configured health/spec reference (a path or a same-host
// URL) into an absolute URL on the service's own host.
func (s *Service) resolve(ref string) (string, error) {
	base, err := url.Parse(s.URL)
	if err != nil {
		return "", err
	}
	r, err := url.Parse(ref)
	if err != nil {
		return "", err
	}
	if r.Scheme != "" || r.Host != "" {
		if r.Scheme != base.Scheme || !strings.EqualFold(r.Host, base.Host) {
			return "", fmt.Errorf("%q must be on the service's own host %s", ref, base.Host)
		}
		if r.User != nil {
			return "", errors.New("credentials in URLs are not allowed")
		}
		return r.String(), nil
	}
	if !strings.HasPrefix(ref, "/") {
		return "", fmt.Errorf("%q must start with /", ref)
	}
	out := *base
	out.Path, out.RawPath = r.Path, ""
	out.RawQuery = r.RawQuery
	return out.String(), nil
}

// checkHost enforces the network policy for a configured host name or IP.
func checkHost(host string, allowLAN bool) error {
	if ip, err := netip.ParseAddr(host); err == nil {
		return checkIP(ip, allowLAN)
	}
	if strings.EqualFold(host, "localhost") {
		return nil
	}
	if !allowLAN {
		return fmt.Errorf("host %q is not loopback; set allow_lan (messh service add --allow-lan) to use a LAN host", host)
	}
	return nil // resolved addresses are checked again at dial time
}

func checkIP(ip netip.Addr, allowLAN bool) error {
	ip = ip.Unmap()
	if ip.IsLoopback() {
		return nil
	}
	if !allowLAN {
		return fmt.Errorf("address %s is not loopback; set allow_lan (messh service add --allow-lan) to use a LAN host", ip)
	}
	if ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return nil
	}
	return fmt.Errorf("address %s is not on a private network", ip)
}

// File is the services file.
type File struct {
	Services []Service `json:"services"`
}

// Find returns the index of the named service or -1.
func (f *File) Find(name string) int {
	return slices.IndexFunc(f.Services, func(s Service) bool { return s.Name == name })
}

// LoadFile reads the services file; a missing file is an empty one.
func LoadFile(path string) (File, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return File{}, nil
	}
	if err != nil {
		return File{}, err
	}
	return parseFile(data)
}

func parseFile(data []byte) (File, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	if len(bytes.TrimSpace(data)) == 0 {
		return File{}, nil
	}
	var f File
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return File{}, fmt.Errorf("parse services file: %w", err)
	}
	return f, nil
}

// SaveFile writes the services file atomically with owner-only permissions.
func SaveFile(path string, f File) error {
	if f.Services == nil {
		f.Services = []Service{}
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return state.WriteFileAtomic(path, append(data, '\n'), 0o600)
}

// ipAllowedAtDial vets an address the dialer is about to connect to, which
// also covers names that resolve to something other than they looked like.
func ipAllowedAtDial(address string, allowLAN bool) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return err
	}
	return checkIP(ip, allowLAN)
}
