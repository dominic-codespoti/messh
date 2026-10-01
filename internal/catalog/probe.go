package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"messh/internal/state"
)

// Status is the outcome of probing one service once.
type Status struct {
	Up      bool
	Latency time.Duration
	Error   string
	// Summary is a short identity line: upstream server/version, API title,
	// operation or tool count.
	Summary string
	Name    string // MCP server name, when the service is an MCP server
}

// ProbeOnce checks svc a single time, the way the node's health loop does,
// without a running node. rootDir resolves relative auth value_file paths.
func ProbeOnce(ctx context.Context, svc Service, rootDir string, timeout time.Duration) Status {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	p := &Provider{ctx: ctx, paths: state.Paths{Root: rootDir}}
	e := &entry{cfg: svc, p: p, ctx: ctx, http: newHTTPClient(svc, rootDir), secret: secretSource{auth: svc.Auth, rootDir: rootDir}}
	start := time.Now()
	var err error
	var summary string
	if svc.Kind == KindMCP {
		e.mcp = newMCPConn(ctx, svc.URL, e.http)
		defer e.mcp.close()
		err = e.probeMCP(ctx)
		if err == nil {
			summary = fmt.Sprintf("%s %s, %d tools", e.upstream.Name, e.upstream.Version, len(e.upTools))
		}
	} else {
		err = e.probeHTTP(ctx)
		if err == nil && e.doc != nil {
			summary = fmt.Sprintf("%s %s, %d operations", e.doc.Title, e.doc.Version, len(e.doc.Ops))
		}
	}
	st := Status{Up: err == nil, Latency: time.Since(start), Summary: strings.TrimSpace(summary), Name: e.upstream.Name}
	if err != nil {
		st.Error = redactString(describeErr(err), e.secret.secrets())
	}
	return st
}

// Detection is what Detect learned about a base URL.
type Detection struct {
	Kind string // best kind to register
	URL  string // URL to register for that kind (the MCP endpoint for mcp)
	// Found lists every interface that answered, best first, as human text.
	Found []string
	// Alternatives are other kinds that also answered, with the URL for each.
	Alternatives []Alt
	AuthRequired bool   // the service answered 401/403 to an unauthenticated or wrongly authenticated request
	Name         string // MCP server name, when one answered
}

// Alt is another interface of the same service.
type Alt struct {
	Kind, URL, Note string
}

// Detect probes base (its URL, Auth, AllowLAN are used) for an MCP server,
// an OpenAI-compatible API, an OpenAPI document, or just HTTP, in that order
// of preference.
func Detect(ctx context.Context, base Service, rootDir string) (*Detection, error) {
	u, err := url.Parse(base.URL)
	if err != nil {
		return nil, err
	}
	d := &Detection{}
	probe := func(kind, target string, svc Service) Status {
		svc.Kind, svc.URL = kind, target
		return ProbeOnce(ctx, svc, rootDir, 4*time.Second)
	}
	root := strings.TrimRight(base.URL, "/")

	// Reachability and authentication first, so failures read well.
	hc := newHTTPClient(base, rootDir)
	if req, err := http.NewRequestWithContext(ctx, http.MethodGet, base.URL, nil); err == nil {
		cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		resp, err := noRedirect(hc).Do(req.WithContext(cctx))
		cancel()
		if err != nil {
			return nil, fmt.Errorf("cannot reach %s: %s", u.Host, describeErr(unwrapURLError(err)))
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		d.AuthRequired = resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden
	}

	// MCP: the URL itself, then /mcp and /mcp/ below it.
	candidates := []string{base.URL}
	if !strings.HasSuffix(root, "/mcp") {
		candidates = append(candidates, root+"/mcp", root+"/mcp/")
	}
	var mcpURL string
	for _, c := range candidates {
		if st := probe(KindMCP, c, base); st.Up {
			mcpURL, d.Name = c, st.Name
			d.Found = append(d.Found, fmt.Sprintf("MCP server at %s (%s)", pathOf(c), st.Summary))
			break
		} else if strings.Contains(st.Error, "401") || strings.Contains(st.Error, "403") {
			d.AuthRequired = true
		}
	}

	// OpenAI-compatible: /v1/models answering with a data list.
	var openaiOK bool
	if looksOpenAI(ctx, hc, strings.TrimSuffix(root, "/v1")+"/v1/models") {
		openaiOK = true
		d.Found = append(d.Found, "OpenAI-compatible API (/v1/models)")
	}

	// OpenAPI: /openapi.json under the base.
	var apiOK bool
	apiBase := strings.TrimSuffix(strings.TrimSuffix(root, "/mcp"), "/v1")
	if st := probe(KindOpenAPI, apiBase, base); st.Up && st.Summary != "" {
		apiOK = true
		d.Found = append(d.Found, "OpenAPI document at /openapi.json ("+st.Summary+")")
	}

	switch {
	case mcpURL != "":
		d.Kind, d.URL = KindMCP, mcpURL
		if openaiOK {
			d.Alternatives = append(d.Alternatives, Alt{KindOpenAI, base.URL, "OpenAI-compatible API"})
		}
		if apiOK {
			d.Alternatives = append(d.Alternatives, Alt{KindOpenAPI, apiBase, "REST API with OpenAPI document"})
		}
	case openaiOK:
		d.Kind, d.URL = KindOpenAI, base.URL
		if apiOK {
			d.Alternatives = append(d.Alternatives, Alt{KindOpenAPI, apiBase, "REST API with OpenAPI document"})
		}
	case apiOK:
		d.Kind, d.URL = KindOpenAPI, apiBase
	default:
		if st := probe(KindHTTP, base.URL, base); st.Up {
			d.Kind, d.URL = KindHTTP, base.URL
			d.Found = append(d.Found, "plain HTTP service (no MCP, OpenAI or OpenAPI interface found)")
		} else if d.AuthRequired {
			return d, errors.New("the service answers 401/403: supply credentials with --bearer, --header or --header-file")
		} else {
			return d, fmt.Errorf("nothing usable answered at %s: %s", base.URL, st.Error)
		}
	}
	return d, nil
}

func pathOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Path == "" {
		return "/"
	}
	return u.Path
}

// looksOpenAI checks that a /v1/models URL returns {"data":[...]}.
func looksOpenAI(ctx context.Context, hc *http.Client, target string) bool {
	cctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, target, nil)
	if err != nil {
		return false
	}
	resp, err := hc.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var env struct {
		Data *json.RawMessage `json:"data"`
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&env) == nil && env.Data != nil
}
