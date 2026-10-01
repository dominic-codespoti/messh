package llmproxy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"messh/internal/approval"
	"messh/internal/catalog"
	"messh/internal/provider"
)

// Host answers model requests for this device's openai-kind services. Every
// request is approved by the person at this device (or a saved rule, or
// auto.read for listing models) before anything reaches the service.
type Host struct {
	Device    string // this device's name, for messages
	Lookup    func(service string) (catalog.Upstream, bool)
	Approvals *approval.Engine
	Log       *slog.Logger // nil: discard
}

// Serve handles one request from caller for service; rest is the path after
// the service segment. The caller identity must already be verified: the
// local agent's token, or a paired peer's pinned certificate.
func (h *Host) Serve(w http.ResponseWriter, r *http.Request, caller provider.Caller, service, rest string) {
	ep, ok := Classify(r.Method, rest)
	if !ok {
		WriteNotForwarded(w, r.Method, rest)
		return
	}
	up, ok := h.lookup(service)
	if !ok {
		WriteError(w, http.StatusNotFound, ErrNotFound, fmt.Sprintf("no enabled OpenAI-compatible service %q on %s (see `messh llm ls`)", service, h.Device))
		return
	}

	var info BodyInfo
	var body io.Reader
	if ep.Method == http.MethodPost {
		if r.ContentLength > MaxBody {
			WriteError(w, http.StatusRequestEntityTooLarge, ErrTooLarge, fmt.Sprintf("request body over %d MiB", MaxBody>>20))
			return
		}
		var err error
		info, body, err = ScanBody(http.MaxBytesReader(w, r.Body, MaxBody))
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			WriteError(w, http.StatusRequestEntityTooLarge, ErrTooLarge, fmt.Sprintf("request body over %d MiB", MaxBody>>20))
			return
		case errors.Is(err, ErrDuplicateModel):
			WriteError(w, http.StatusBadRequest, ErrInvalid, err.Error())
			return
		case err != nil:
			if r.Context().Err() == nil {
				WriteError(w, http.StatusBadRequest, ErrInvalid, "reading the request body failed: "+err.Error())
			}
			return
		}
	}

	req := approval.Request{
		Caller:   caller,
		Tool:     "llm " + ep.Label(),
		Class:    provider.ClassService,
		Approval: BuildApproval(service, ep, info, caller, up.AutoRead),
	}
	// This blocks until the owner answers, which can take minutes; the
	// client's connection stays open meanwhile, and closing it withdraws
	// the prompt.
	dec, err := h.Approvals.Decide(r.Context(), req)
	switch {
	case r.Context().Err() != nil:
		return
	case err != nil:
		WriteError(w, http.StatusServiceUnavailable, ErrApproval, fmt.Sprintf("approval on %s failed: %v", h.Device, err))
		return
	case !dec.Allowed:
		WriteError(w, http.StatusForbidden, ErrDenied, fmt.Sprintf("denied on %s: %s", h.Device, dec.Reason))
		return
	}

	start := time.Now()
	status, sent, returned, ferr := h.forward(w, r, up, ep, body)
	ok = ferr == nil && status > 0 && status < 400
	msg := fmt.Sprintf("HTTP %d, %d bytes sent, %d bytes returned", status, sent, returned)
	if status == 0 {
		msg = fmt.Sprintf("no response, %d bytes sent", sent)
	}
	if ferr != nil {
		msg += "; " + up.Redact(ferr.Error())
	}
	h.Approvals.Complete(dec, req, ok, msg, time.Since(start))
}

// forward sends the approved request to the service and streams the answer.
// status is 0 when the service did not answer.
func (h *Host) forward(w http.ResponseWriter, r *http.Request, up catalog.Upstream, ep Endpoint, body io.Reader) (status int, sent, returned int64, err error) {
	cb := &countingReader{r: body}
	var rb io.Reader
	if body != nil {
		rb = cb
	}
	out, err := http.NewRequestWithContext(r.Context(), ep.Method, UpstreamURL(up.Base, ep, r.URL.RawQuery).String(), rb)
	if err != nil {
		WriteError(w, http.StatusBadGateway, ErrUpstream, "cannot build the upstream request")
		return 0, 0, 0, err
	}
	out.Header = RequestHeaders(r.Header)
	// Identity encoding keeps streamed events unbuffered and lets error
	// bodies be redacted.
	out.Header.Set("Accept-Encoding", "identity")
	if body != nil && r.ContentLength > 0 {
		out.ContentLength = r.ContentLength
	}
	resp, err := up.Transport.RoundTrip(out)
	if err != nil {
		if r.Context().Err() != nil {
			return 0, cb.n, 0, errors.New("caller went away")
		}
		h.log().Debug("llm upstream failed", "service", up.Name, "error", up.Redact(err.Error()))
		WriteError(w, http.StatusBadGateway, ErrUpstream, fmt.Sprintf("%s on %s did not answer: %s", up.Name, h.Device, up.Redact(err.Error())))
		return 0, cb.n, 0, err
	}
	defer resp.Body.Close()
	CopyResponseHeaders(w.Header(), resp.Header)

	if resp.StatusCode/100 != 2 && !isEventStream(resp.Header) && resp.ContentLength <= MaxRedact {
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, MaxRedact+1))
		if rerr == nil && len(data) <= MaxRedact {
			w.WriteHeader(resp.StatusCode)
			k, werr := io.WriteString(w, up.Redact(string(data)))
			return resp.StatusCode, cb.n, int64(k), werr
		}
		// Too large (or cut short) to redact as a whole: pass it on as it is.
		w.WriteHeader(resp.StatusCode)
		k, werr := w.Write(data)
		if werr != nil || rerr != nil {
			return resp.StatusCode, cb.n, int64(k), errors.Join(werr, rerr)
		}
		m, serr := Stream(w, resp.Body)
		return resp.StatusCode, cb.n, int64(k) + m, serr
	}
	w.WriteHeader(resp.StatusCode)
	n, serr := Stream(w, resp.Body)
	return resp.StatusCode, cb.n, n, serr
}

func (h *Host) lookup(service string) (catalog.Upstream, bool) {
	if h.Lookup == nil || !ValidService(service) {
		return catalog.Upstream{}, false
	}
	return h.Lookup(service)
}

func (h *Host) log() *slog.Logger {
	if h.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return h.Log
}

func isEventStream(hdr http.Header) bool {
	return strings.HasPrefix(strings.ToLower(hdr.Get("Content-Type")), "text/event-stream")
}

// WriteNotForwarded answers 404 for a path outside the allowlist.
func WriteNotForwarded(w http.ResponseWriter, method, rest string) {
	parts := make([]string, len(endpoints))
	for i, e := range endpoints {
		parts[i] = e.Label()
	}
	WriteError(w, http.StatusNotFound, ErrNotFound, fmt.Sprintf("%s /%s is not forwarded; allowed: %s",
		method, strings.TrimPrefix(rest, "/"), strings.Join(parts, ", ")))
}

// BuildApproval describes a model request to the person who must allow it.
// The exact hash covers service, endpoint and model, never the prompt: each
// chat request differs, so "always allow" must be about the model.
func BuildApproval(service string, ep Endpoint, info BodyInfo, caller provider.Caller, autoRead bool) provider.Approval {
	who := callerLabel(caller)
	model := info.Model
	ap := provider.Approval{
		Title: who + " wants to use the " + service + " language model",
		Details: []provider.Detail{
			{Label: "Service", Value: service + " (OpenAI-compatible)"},
			{Label: "Endpoint", Value: ep.Label()},
		},
	}
	if ep.Read {
		ap.Title = who + " wants to list the models of " + service
	} else {
		shown := "(not given: the server's default)"
		if model != "" {
			shown = clip(model, 200)
		}
		streaming := "no"
		if info.Stream {
			streaming = "yes"
		}
		ap.Details = append(ap.Details,
			provider.Detail{Label: "Model", Value: shown},
			provider.Detail{Label: "Streaming", Value: streaming})
	}
	ap.Details = append(ap.Details, provider.Detail{Label: "Requested by", Value: who})

	id, _ := json.Marshal(map[string]any{"v": 1, "service": service, "endpoint": ep.Label(), "model": model})
	sum := sha256.Sum256(id)
	ap.Exact = "llm:" + hex.EncodeToString(sum[:])

	if model != "" && !ep.Read {
		ap.Scopes = append(ap.Scopes, provider.Scope{
			Key: "service:" + service + ":llm:" + model, Label: "this model (" + clip(model, 80) + ") on " + service + ", any prompt",
		})
	}
	ap.Scopes = append(ap.Scopes,
		provider.Scope{Key: "service:" + service + ":llm", Label: "any model on " + service + ", any prompt"},
		provider.Scope{Key: "service:" + service + ":*", Label: "anything on service " + service, Broad: true},
	)
	if ep.Read && autoRead {
		ap.Auto = "services.json: " + service + " auto.read"
	}
	return ap
}

func callerLabel(c provider.Caller) string {
	switch {
	case c.Agent != "" && c.DeviceName != "":
		return c.Agent + " on " + c.DeviceName
	case c.DeviceName != "":
		return c.DeviceName
	case c.Agent != "":
		return c.Agent
	}
	return c.DeviceID
}

func clip(s string, n int) string {
	var b strings.Builder
	for i, r := range s {
		if i >= n {
			b.WriteString("...")
			break
		}
		if r < 0x20 || r == 0x7f {
			r = '?'
		}
		b.WriteRune(r)
	}
	return b.String()
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	k, err := c.r.Read(p)
	c.n += int64(k)
	return k, err
}
