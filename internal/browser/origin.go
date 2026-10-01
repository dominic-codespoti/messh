package browser

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// canonHost lowercases host and rewrites the numeric IPv4 spellings browsers
// accept (2130706433, 0x7f.1, 0177.0.0.1) to dotted decimal, so list entries
// and browser-reported URLs compare equal. Browsers report internationalised
// names in punycode; accepting Unicode here would need a table messh does
// not carry, so such names are refused with a pointer to the xn-- form.
func canonHost(host string) (string, error) {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" {
		return "", errors.New("no host")
	}
	for i := range len(host) {
		if host[i] >= 0x80 {
			return "", fmt.Errorf("host %q is not ASCII; write internationalised names in punycode (xn--...)", host)
		}
	}
	if ip, ok := parseLooseIPv4(host); ok {
		return ip, nil
	}
	return host, nil
}

// parseLooseIPv4 implements the WHATWG IPv4 number parser: 1-4 parts, each
// decimal, 0x hex or 0-prefixed octal, the last part filling the remaining bytes.
func parseLooseIPv4(host string) (string, bool) {
	parts := strings.Split(host, ".")
	if len(parts) == 0 || len(parts) > 4 {
		return "", false
	}
	nums := make([]uint64, len(parts))
	for i, p := range parts {
		if p == "" {
			return "", false
		}
		base, digits := 10, p
		switch {
		case strings.HasPrefix(p, "0x") || strings.HasPrefix(p, "0X"):
			base, digits = 16, p[2:]
			if digits == "" {
				digits = "0"
			}
		case len(p) > 1 && p[0] == '0':
			base, digits = 8, p[1:]
		}
		n, err := strconv.ParseUint(digits, base, 32)
		if err != nil {
			return "", false
		}
		nums[i] = n
	}
	last := nums[len(nums)-1]
	for _, n := range nums[:len(nums)-1] {
		if n > 255 {
			return "", false
		}
	}
	if last >= uint64(1)<<(8*uint(5-len(nums))) {
		return "", false
	}
	v := last
	for i, n := range nums[:len(nums)-1] {
		v += n << (8 * uint(3-i))
	}
	return fmt.Sprintf("%d.%d.%d.%d", byte(v>>24), byte(v>>16), byte(v>>8), byte(v)), true
}

// Origin is the unit of approval: scheme, host and port of a web page.
// Subdomains are distinct origins.
type Origin struct {
	Scheme string // "http" or "https"
	Host   string // lowercase, without brackets or trailing dot
	Port   string // always set (default ports filled in)
}

// String renders the origin the way browsers do, omitting default ports.
func (o Origin) String() string {
	if o.Scheme == "" {
		return ""
	}
	host := o.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if (o.Scheme == "http" && o.Port == "80") || (o.Scheme == "https" && o.Port == "443") {
		return o.Scheme + "://" + host
	}
	return o.Scheme + "://" + host + ":" + o.Port
}

// IsZero reports an empty origin.
func (o Origin) IsZero() bool { return o.Scheme == "" }

// LocalNetwork reports whether the host is loopback, a private or link-local
// address, or a single-label / .local / .internal name: a page on the owner's
// own network rather than the public internet.
func (o Origin) LocalNetwork() bool {
	if a, err := netip.ParseAddr(o.Host); err == nil {
		return a.IsLoopback() || a.IsPrivate() || a.IsLinkLocalUnicast() || a.IsUnspecified()
	}
	if o.Host == "localhost" || strings.HasSuffix(o.Host, ".localhost") {
		return true
	}
	return !strings.Contains(o.Host, ".") ||
		strings.HasSuffix(o.Host, ".local") || strings.HasSuffix(o.Host, ".internal") || strings.HasSuffix(o.Host, ".lan")
}

// SchemeError reports a page that is not a web page.
type SchemeError struct{ Scheme string }

func (e *SchemeError) Error() string {
	return fmt.Sprintf("%s: addresses are not allowed; only http and https web pages can be used", e.Scheme)
}

// ParseURL splits raw into its origin. Only http and https are accepted;
// everything else (file:, chrome:, about:, devtools:, chrome-extension:,
// view-source:, data:, javascript:, ...) is refused outright. clean is the
// URL without credentials, query and fragment, for display.
func ParseURL(raw string) (o Origin, clean string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Origin{}, "", errors.New("empty URL")
	}
	if strings.ContainsAny(raw, "\r\n\x00") {
		return Origin{}, "", errors.New("URL contains control characters")
	}
	u, perr := url.Parse(raw)
	if perr != nil {
		return Origin{}, "", fmt.Errorf("invalid URL: %v", urlErr(perr))
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" {
		return Origin{}, "", errors.New("URL has no scheme; use a full address such as https://example.com/")
	}
	if scheme != "http" && scheme != "https" {
		return Origin{}, "", &SchemeError{Scheme: scheme + ":"}
	}
	host, herr := canonHost(u.Hostname())
	if herr != nil {
		return Origin{}, "", herr
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[scheme]
	}
	o = Origin{Scheme: scheme, Host: host, Port: port}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	return o, o.String() + path, nil
}

func urlErr(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// Pattern is one allow_origins / deny_origins entry.
type Pattern struct {
	scheme  string // "" matches either scheme (deny lists only)
	host    string // exact host, or the suffix after "*." when wild
	wild    bool
	port    string // "" default port of the scheme (any port in a deny list), "*" any
	deny    bool
	display string
}

func (p Pattern) String() string { return p.display }

// ParsePattern parses [scheme://]host[:port]. host may be "*.example.com"
// (any subdomain, not example.com itself). A pattern without scheme matches
// https only in an allow list and both schemes in a deny list. A port must be
// given when it is not the scheme's default; ":*" matches any port.
func ParsePattern(s string, deny bool) (Pattern, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return Pattern{}, errors.New("empty entry")
	}
	p := Pattern{display: raw, deny: deny}
	rest := strings.ToLower(raw)
	if i := strings.Index(rest, "://"); i >= 0 {
		p.scheme = rest[:i]
		rest = rest[i+3:]
		if p.scheme != "http" && p.scheme != "https" {
			return Pattern{}, fmt.Errorf("%q: only http and https origins exist", s)
		}
	} else if !deny {
		p.scheme = "https"
	}
	if strings.ContainsAny(rest, "/?#@ ") {
		return Pattern{}, fmt.Errorf("%q: give an origin (scheme://host:port), not a URL", s)
	}
	host := rest
	if strings.HasPrefix(rest, "[") { // IPv6 literal
		end := strings.Index(rest, "]")
		if end < 0 {
			return Pattern{}, fmt.Errorf("%q: unterminated [", s)
		}
		host = rest[1:end]
		rest = rest[end+1:]
		if rest != "" {
			if !strings.HasPrefix(rest, ":") {
				return Pattern{}, fmt.Errorf("%q: unexpected text after ]", s)
			}
			p.port = rest[1:]
		}
	} else if h, port, err := net.SplitHostPort(rest); err == nil {
		host, p.port = h, port
	} else if strings.Contains(rest, ":") {
		return Pattern{}, fmt.Errorf("%q: bad port", s)
	}
	if p.port != "" && p.port != "*" {
		for _, r := range p.port {
			if r < '0' || r > '9' {
				return Pattern{}, fmt.Errorf("%q: bad port", s)
			}
		}
	}
	host = strings.TrimSuffix(host, ".")
	switch {
	case host == "":
		return Pattern{}, fmt.Errorf("%q: no host", s)
	case host == "*":
		return Pattern{}, fmt.Errorf("%q: a bare * is not an origin; list the sites you mean", s)
	case strings.HasPrefix(host, "*."):
		p.wild = true
		host = host[2:]
		if host == "" || !strings.Contains(host, ".") && !deny {
			return Pattern{}, fmt.Errorf("%q: wildcard needs a registrable domain such as *.example.com", s)
		}
		if strings.Contains(host, "*") {
			return Pattern{}, fmt.Errorf("%q: only a leading *. is supported", s)
		}
		if _, err := netip.ParseAddr(host); err == nil {
			return Pattern{}, fmt.Errorf("%q: a wildcard cannot cover an IP address", s)
		}
	case strings.Contains(host, "*"):
		return Pattern{}, fmt.Errorf("%q: only a leading *. is supported", s)
	}
	var herr error
	if host, herr = canonHost(host); herr != nil {
		return Pattern{}, fmt.Errorf("%q: %v", s, herr)
	}
	p.host = host
	return p, nil
}

// Match reports whether o is covered by p.
func (p Pattern) Match(o Origin) bool {
	if o.IsZero() {
		return false
	}
	if p.scheme != "" && p.scheme != o.Scheme {
		return false
	}
	switch p.port {
	case "*":
	case "":
		if !p.deny && o.Port != map[string]string{"http": "80", "https": "443"}[o.Scheme] {
			return false
		}
	default:
		if p.port != o.Port {
			return false
		}
	}
	if p.wild {
		return strings.HasSuffix(o.Host, "."+p.host)
	}
	return o.Host == p.host
}

// MatchAny reports whether any pattern in list covers o. Unparseable entries
// are ignored here; Config.Validate reports them.
func MatchAny(list []string, deny bool, o Origin) bool {
	for _, s := range list {
		if p, err := ParsePattern(s, deny); err == nil && p.Match(o) {
			return true
		}
	}
	return false
}
