package catalog

import (
	"net/http"
	"net/url"
)

// Upstream is an enabled openai-kind service as the LLM proxy needs it: where
// it lives and the only transport allowed to reach it.
type Upstream struct {
	Name string
	// Base is the service URL as registered; it may or may not end in /v1.
	Base *url.URL
	// Transport injects the service's credential and dials only what the
	// entry's host policy allows (loopback unless allow_lan), checked again
	// after DNS. It does not follow redirects.
	Transport http.RoundTripper
	// AutoRead is the owner's auto.read: listing models needs no prompt.
	AutoRead bool

	secrets []string
}

// Redact removes the service's credential from s.
func (u Upstream) Redact(s string) string { return redactString(s, u.secrets) }

// OpenAIUpstream returns the named service if it is enabled and of kind
// openai. The credential never leaves the returned transport.
func (p *Provider) OpenAIUpstream(name string) (Upstream, bool) {
	e, err := p.lookup(name)
	if err != nil || e.cfg.Kind != KindOpenAI {
		return Upstream{}, false
	}
	base, err := url.Parse(e.cfg.URL)
	if err != nil {
		return Upstream{}, false
	}
	return Upstream{
		Name:      e.cfg.Name,
		Base:      base,
		Transport: e.http.Transport,
		AutoRead:  e.cfg.Auto.Read,
		secrets:   e.secret.secrets(),
	}, true
}
