// Package llmproxy lets agents use OpenAI-compatible model servers that run
// on another device as an ordinary model provider. The agent's own node
// accepts the request on its loopback endpoint, authenticated with the
// agent's messh token, and streams it over the pinned mesh to the device
// that hosts the service. There the owner approves it and messh forwards it
// with the service's own credential, which never leaves that device.
package llmproxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/textproto"
	"net/url"
	"regexp"
	"strings"
)

const (
	// MaxBody caps a request body; larger requests get 413.
	MaxBody = 32 << 20
	// MaxRedact is the largest non-streaming error body that is redacted
	// before it is passed back; larger ones pass through as they are.
	MaxRedact = 1 << 20
)

// Endpoint is one forwardable upstream operation.
type Endpoint struct {
	Method string
	Path   string // canonical, always starting with /v1/
	Read   bool   // lists models; covered by the service's auto.read
}

// Label is how prompts and audit records name the endpoint.
func (e Endpoint) Label() string { return e.Method + " " + e.Path }

var endpoints = []Endpoint{
	{Method: http.MethodGet, Path: "/v1/models", Read: true},
	{Method: http.MethodPost, Path: "/v1/chat/completions"},
	{Method: http.MethodPost, Path: "/v1/completions"},
	{Method: http.MethodPost, Path: "/v1/embeddings"},
	{Method: http.MethodPost, Path: "/v1/responses"},
	{Method: http.MethodPost, Path: "/v1/messages"},
}

// Classify maps a request to an allowed endpoint. rest is the path after the
// service segment, with or without a leading slash or "v1/" (clients differ
// in whether their base URL carries /v1). Anything else is not forwarded.
func Classify(method, rest string) (Endpoint, bool) {
	p := "/" + strings.TrimPrefix(rest, "/")
	if !strings.HasPrefix(p, "/v1/") {
		p = "/v1" + p
	}
	for _, e := range endpoints {
		if e.Method == method && e.Path == p {
			return e, true
		}
	}
	return Endpoint{}, false
}

var serviceRE = regexp.MustCompile(`^[a-z0-9_-]{1,24}$`)

// ValidService reports whether name can be a catalogue service name.
func ValidService(name string) bool { return serviceRE.MatchString(name) }

// UpstreamURL joins a service base URL and an endpoint. A base that already
// ends in /v1 (Ollama's http://127.0.0.1:11434/v1) does not get a second one.
func UpstreamURL(base *url.URL, e Endpoint, rawQuery string) *url.URL {
	u := *base
	p := strings.TrimRight(base.Path, "/")
	p = strings.TrimSuffix(p, "/v1")
	u.Path, u.RawPath = p+e.Path, ""
	u.RawQuery, u.Fragment = rawQuery, ""
	u.User = nil
	return &u
}

// forwardHeaders are the only client headers sent on. Everything else,
// including Authorization, x-api-key, cookies and hop-by-hop headers, stays
// behind.
var forwardHeaders = []string{"Content-Type", "Accept", "Anthropic-Version", "Openai-Beta"}

// RequestHeaders returns the client headers to forward.
func RequestHeaders(in http.Header) http.Header {
	out := http.Header{}
	for _, k := range forwardHeaders {
		if v := in.Values(k); len(v) > 0 {
			out[k] = append([]string(nil), v...)
		}
	}
	return out
}

var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization",
	"Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// CopyResponseHeaders copies src into dst without hop-by-hop headers,
// cookies, or Content-Length (bodies are re-framed, and may be redacted).
func CopyResponseHeaders(dst, src http.Header) {
	drop := map[string]bool{"Set-Cookie": true, "Content-Length": true}
	for _, h := range hopHeaders {
		drop[h] = true
	}
	for _, v := range src.Values("Connection") {
		for _, f := range strings.Split(v, ",") {
			if f = strings.TrimSpace(f); f != "" {
				drop[textproto.CanonicalMIMEHeaderKey(f)] = true
			}
		}
	}
	for k, v := range src {
		if !drop[textproto.CanonicalMIMEHeaderKey(k)] {
			dst[k] = append([]string(nil), v...)
		}
	}
}

// AgentToken returns the messh token a client sent: OpenAI clients send the
// API key as a bearer token, Anthropic-style clients as x-api-key.
func AgentToken(r *http.Request) string {
	if tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(tok)
	}
	return strings.TrimSpace(r.Header.Get("X-Api-Key"))
}

// Error types used in error bodies.
const (
	ErrAuth      = "authentication_error"
	ErrDenied    = "permission_denied"
	ErrNotFound  = "not_found"
	ErrInvalid   = "invalid_request_error"
	ErrTooLarge  = "request_too_large"
	ErrBusy      = "rate_limit_error"
	ErrApproval  = "approval_error"
	ErrUpstream  = "upstream_error"
	ErrUnreached = "device_unreachable"
)

// ErrorBody is an OpenAI-style error: {"error":{"message":...,"type":...}}.
func ErrorBody(typ, msg string) []byte {
	b, _ := json.Marshal(map[string]any{"error": map[string]string{"message": msg, "type": typ}})
	return b
}

// WriteError answers with an OpenAI-style error body.
func WriteError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(append(ErrorBody(typ, msg), '\n'))
}

// BodyInfo is what the approval prompt shows about a request body.
type BodyInfo struct {
	// Model is the top-level "model" string; HasModel is false when the
	// body has none (or it is not a string).
	Model    string
	HasModel bool
	Stream   bool // top-level "stream": true
}

// ErrDuplicateModel rejects a body with two top-level "model" keys: the
// prompt would show one and the server might use the other.
var ErrDuplicateModel = errors.New(`the request has more than one "model" field`)

// ScanBody reads the JSON request body to find its model and streaming flag,
// and returns a reader that replays the body exactly. The whole top-level
// object is read so a later duplicate "model" key cannot slip past the
// prompt; the host has to hold the request until the owner decides anyway.
// Bodies that are not JSON objects are passed on unchanged with an empty
// BodyInfo for the server to reject. The error is non-nil only for a failed
// read (for example an http.MaxBytesError) or a duplicate model.
func ScanBody(r io.Reader) (BodyInfo, io.Reader, error) {
	var seen bytes.Buffer
	src := &readErr{r: r}
	dec := json.NewDecoder(io.TeeReader(src, &seen))
	info, err := scanObject(dec)
	replay := io.MultiReader(bytes.NewReader(seen.Bytes()), src)
	if src.err != nil && !errors.Is(src.err, io.EOF) {
		return BodyInfo{}, nil, src.err
	}
	if errors.Is(err, ErrDuplicateModel) {
		return BodyInfo{}, nil, err
	}
	return info, replay, nil
}

type readErr struct {
	r   io.Reader
	err error
}

func (e *readErr) Read(p []byte) (int, error) {
	n, err := e.r.Read(p)
	if err != nil && e.err == nil {
		e.err = err
	}
	return n, err
}

func scanObject(dec *json.Decoder) (BodyInfo, error) {
	var info BodyInfo
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return info, err
	}
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return info, err
		}
		key, _ := t.(string)
		switch key {
		case "model":
			if info.HasModel {
				return BodyInfo{}, ErrDuplicateModel
			}
			v, err := dec.Token()
			if err != nil {
				return info, err
			}
			if s, ok := v.(string); ok {
				info.Model, info.HasModel = s, true
			} else if err := skipRest(dec, v); err != nil {
				return info, err
			}
		case "stream":
			v, err := dec.Token()
			if err != nil {
				return info, err
			}
			b, ok := v.(bool)
			info.Stream = ok && b
			if err := skipRest(dec, v); err != nil {
				return info, err
			}
		default:
			v, err := dec.Token()
			if err != nil {
				return info, err
			}
			if err := skipRest(dec, v); err != nil {
				return info, err
			}
		}
	}
	return info, nil
}

// skipRest consumes the rest of a value whose first token was t.
func skipRest(dec *json.Decoder, t json.Token) error {
	if d, ok := t.(json.Delim); !ok || (d != '{' && d != '[') {
		return nil
	}
	for depth := 1; depth > 0; {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := t.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			default:
				depth--
			}
		}
	}
	return nil
}

// Stream copies body to w, flushing after every read so server-sent events
// reach the client as they are produced. It returns the bytes written.
func Stream(w http.ResponseWriter, body io.Reader) (int64, error) {
	rc := http.NewResponseController(w)
	buf := make([]byte, 32<<10)
	var n int64
	for {
		k, err := body.Read(buf)
		if k > 0 {
			if _, werr := w.Write(buf[:k]); werr != nil {
				return n, werr
			}
			n += int64(k)
			if ferr := rc.Flush(); ferr != nil && !errors.Is(ferr, http.ErrNotSupported) {
				return n, ferr
			}
		}
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
}

// Relay passes a response from the next hop to the client: status, headers
// (without hop-by-hop ones) and the streamed body.
func Relay(w http.ResponseWriter, resp *http.Response) (int64, error) {
	CopyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	return Stream(w, resp.Body)
}
