package browser

import (
	"errors"
	"testing"
)

func TestParseURLRefusesNonWebSchemes(t *testing.T) {
	for _, raw := range []string{
		"file:///C:/Windows/win.ini", "chrome://settings", "about:blank", "devtools://devtools/bundled/inspector.html",
		"chrome-extension://abc/page.html", "view-source:https://example.com", "data:text/html,<h1>x</h1>",
		"javascript:alert(1)", "ftp://example.com/", "blob:https://example.com/uuid", "FILE:///etc/passwd", "ws://example.com/",
	} {
		_, _, err := ParseURL(raw)
		var se *SchemeError
		if !errors.As(err, &se) {
			t.Errorf("ParseURL(%q) = %v, want a scheme refusal", raw, err)
		}
	}
	for _, raw := range []string{"", "   ", "example.com", "//example.com/x", "http://", "https://exa\nmple.com"} {
		if _, _, err := ParseURL(raw); err == nil {
			t.Errorf("ParseURL(%q) succeeded, want an error", raw)
		}
	}
}

func TestParseURLOrigin(t *testing.T) {
	cases := []struct {
		raw, origin, clean string
	}{
		{"https://GitHub.com/a/b?token=x#frag", "https://github.com", "https://github.com/a/b"},
		{"https://user:pw@example.com:443/p", "https://example.com", "https://example.com/p"},
		{"http://example.com:80", "http://example.com", "http://example.com/"},
		{"http://127.0.0.1:8080/x", "http://127.0.0.1:8080", "http://127.0.0.1:8080/x"},
		{"https://example.com.", "https://example.com", "https://example.com/"},
		{"http://[::1]:3000/", "http://[::1]:3000", "http://[::1]:3000/"},
		// Numeric spellings Chrome canonicalises must land on the same origin as the dotted form.
		{"http://2130706433/", "http://127.0.0.1", "http://127.0.0.1/"},
		{"http://0x7f.1/", "http://127.0.0.1", "http://127.0.0.1/"},
		{"http://0177.0.0.1/", "http://127.0.0.1", "http://127.0.0.1/"},
	}
	for _, c := range cases {
		o, clean, err := ParseURL(c.raw)
		if err != nil {
			t.Errorf("ParseURL(%q): %v", c.raw, err)
			continue
		}
		if o.String() != c.origin || clean != c.clean {
			t.Errorf("ParseURL(%q) = %s, %s; want %s, %s", c.raw, o, clean, c.origin, c.clean)
		}
	}
	a, _, _ := ParseURL("https://example.com")
	b, _, _ := ParseURL("https://www.example.com")
	if a == b {
		t.Error("a subdomain must be a different origin")
	}
	if _, _, err := ParseURL("https://bücher.example/"); err == nil {
		t.Error("a non-ASCII host should ask for punycode, not be guessed")
	}
}

func TestLocalNetwork(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1": true, "192.168.1.5": true, "10.0.0.2": true, "localhost": true, "router": true, "nas.local": true,
		"169.254.1.1": true, "example.com": false, "8.8.8.8": false, "github.com": false,
	} {
		if got := (Origin{Scheme: "http", Host: host, Port: "80"}).LocalNetwork(); got != want {
			t.Errorf("LocalNetwork(%s) = %v, want %v", host, got, want)
		}
	}
}

func TestPatternMatching(t *testing.T) {
	origin := func(raw string) Origin {
		o, _, err := ParseURL(raw)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	allow := []struct {
		pattern, url string
		want         bool
	}{
		{"example.com", "https://example.com/x", true},
		{"example.com", "http://example.com/x", false}, // allow entries without a scheme mean https
		{"example.com", "https://www.example.com/", false},
		{"example.com", "https://example.com:8443/", false},
		{"example.com:8443", "https://example.com:8443/", true},
		{"example.com:*", "https://example.com:9/", true},
		{"*.example.com", "https://www.example.com/", true},
		{"*.example.com", "https://a.b.example.com/", true},
		{"*.example.com", "https://example.com/", false}, // the bare domain is not a subdomain
		{"*.example.com", "https://badexample.com/", false},
		{"*.example.com", "https://example.com.evil.net/", false},
		{"http://localhost:3000", "http://localhost:3000/a", true},
		{"http://localhost:3000", "https://localhost:3000/a", false},
		{"EXAMPLE.com", "https://example.com", true},
	}
	for _, c := range allow {
		p, err := ParsePattern(c.pattern, false)
		if err != nil {
			t.Errorf("allow %q: %v", c.pattern, err)
			continue
		}
		if got := p.Match(origin(c.url)); got != c.want {
			t.Errorf("allow %q vs %s = %v, want %v", c.pattern, c.url, got, c.want)
		}
	}
	deny := []struct {
		pattern, url string
		want         bool
	}{
		{"bank.example", "http://bank.example/", true}, // deny entries cover both schemes and every port
		{"bank.example", "https://bank.example:8443/", true},
		{"*.bank.example", "https://login.bank.example/", true},
		{"*.bank.example", "https://bank.example/", false},
		{"0x7f.1", "http://127.0.0.1:9/", true}, // numeric spellings are the same host
		{"127.0.0.1", "http://2130706433/", true},
	}
	for _, c := range deny {
		p, err := ParsePattern(c.pattern, true)
		if err != nil {
			t.Errorf("deny %q: %v", c.pattern, err)
			continue
		}
		if got := p.Match(origin(c.url)); got != c.want {
			t.Errorf("deny %q vs %s = %v, want %v", c.pattern, c.url, got, c.want)
		}
	}
	for _, bad := range []string{"", "*", "https://*", "*.com.*", "exa*mple.com", "https://example.com/path", "ftp://example.com", "*.127.0.0.1", "a@b.com", "*.localhost"} {
		if _, err := ParsePattern(bad, false); err == nil {
			t.Errorf("allow pattern %q accepted", bad)
		}
	}
	if _, err := ParsePattern("*", true); err == nil {
		t.Error("deny pattern * accepted; list the sites instead")
	}
}
