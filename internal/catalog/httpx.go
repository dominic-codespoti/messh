package catalog

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const userAgent = "messh-catalog/1"

// secretSource resolves a service's credential at request time, so a rotated
// value_file takes effect without a restart.
type secretSource struct {
	auth     *Auth
	rootDir  string // relative value_file paths resolve against the state dir
	readFile func(string) ([]byte, error)
}

// header returns the header name and value to inject, or "" when none is set.
func (s secretSource) header() (name, value string, err error) {
	if s.auth == nil {
		return "", "", nil
	}
	if s.auth.Value != "" {
		return s.auth.Header, s.auth.Value, nil
	}
	p := s.auth.ValueFile
	if !filepath.IsAbs(p) {
		p = filepath.Join(s.rootDir, p)
	}
	rf := s.readFile
	if rf == nil {
		rf = os.ReadFile
	}
	b, err := rf(p)
	if err != nil {
		// Do not echo the path: it is configuration, but errors travel to agents.
		return "", "", errors.New("cannot read the service's auth value_file")
	}
	v := strings.TrimSpace(string(b))
	if v == "" || strings.ContainsAny(v, "\r\n\x00") {
		return "", "", errors.New("the service's auth value_file is empty or malformed")
	}
	return s.auth.Header, v, nil
}

// secrets lists strings that must never leave the device: the value, and for
// "Scheme token" values the bare token as well.
func (s secretSource) secrets() []string {
	_, v, err := s.header()
	if err != nil || len(v) < 4 {
		return nil
	}
	out := []string{v}
	if _, tok, ok := strings.Cut(v, " "); ok && len(tok) >= 4 {
		out = append(out, tok)
	}
	return out
}

func redactString(s string, secrets []string) string {
	for _, sec := range secrets {
		if strings.Contains(s, sec) {
			s = strings.ReplaceAll(s, sec, "[redacted]")
		}
	}
	return s
}

// authTransport injects the credential and a User-Agent on every request.
type authTransport struct {
	base http.RoundTripper
	src  secretSource
}

func (t authTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	name, value, err := t.src.header()
	if err != nil {
		return nil, err
	}
	req = req.Clone(req.Context())
	if name != "" {
		req.Header.Set(name, value)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", userAgent)
	}
	return t.base.RoundTrip(req)
}

// newHTTPClient builds the only client used to reach a service. It refuses
// connections to anything the host policy does not allow (even after DNS),
// never uses environment proxies, and follows redirects only within the
// service's own origin.
func newHTTPClient(svc Service, rootDir string) *http.Client {
	allowLAN := svc.AllowLAN
	dialer := &net.Dialer{
		Timeout: 5 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			return ipAllowedAtDial(address, allowLAN)
		},
	}
	tr := &http.Transport{
		Proxy:               nil,
		DialContext:         dialer.DialContext,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     60 * time.Second,
		TLSHandshakeTimeout: 5 * time.Second,
	}
	base, _ := url.Parse(svc.URL)
	return &http.Client{
		Transport: authTransport{base: tr, src: secretSource{auth: svc.Auth, rootDir: rootDir}},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			if base == nil || req.URL.Scheme != base.Scheme || !strings.EqualFold(req.URL.Host, base.Host) {
				return fmt.Errorf("refusing redirect outside the service (%s)", req.URL.Host)
			}
			return nil
		},
	}
}

// noRedirect returns a copy of c that hands redirect responses back instead
// of following them: a redirect already proves the service answers.
func noRedirect(c *http.Client) *http.Client {
	cp := *c
	cp.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &cp
}

// closeIdle drops pooled connections for a retired client.
func closeIdle(c *http.Client) {
	if c == nil {
		return
	}
	if t, ok := c.Transport.(authTransport); ok {
		if tr, ok := t.base.(*http.Transport); ok {
			tr.CloseIdleConnections()
		}
	}
}

// cleanRel validates a caller-supplied path (no scheme, host, query,
// fragment, dot segments, or encoded separators) and returns it unchanged.
func cleanRel(p string) error {
	if p == "" || p[0] != '/' {
		return errors.New("path must start with /")
	}
	if strings.HasPrefix(p, "//") {
		return errors.New("path must not start with //")
	}
	if strings.ContainsAny(p, "?#\\\x00\r\n") {
		return errors.New("path must not contain ?, #, backslashes or control characters (use query for parameters)")
	}
	for seg := range strings.SplitSeq(p, "/") {
		dec, err := url.PathUnescape(seg)
		if err != nil {
			return fmt.Errorf("path has a bad escape: %v", err)
		}
		if dec == ".." || dec == "." || strings.ContainsAny(dec, "/\\") {
			return errors.New("path must not contain dot segments or encoded separators")
		}
	}
	return nil
}

// buildURL joins the service base URL, a validated relative path and query.
func buildURL(base string, p string, query url.Values) (string, error) {
	if err := cleanRel(p); err != nil {
		return "", err
	}
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	pre := strings.TrimRight(u.Path, "/")
	rp, err := url.PathUnescape(p)
	if err != nil {
		return "", err
	}
	u.Path = pre + rp
	u.RawPath = pre + p // EscapedPath uses it only when it encodes Path
	u.RawQuery = query.Encode()
	return u.String(), nil
}
