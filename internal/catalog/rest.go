package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/provider"
)

const (
	callTimeout    = 10 * time.Minute
	maxBuffered    = 32 << 20  // largest JSON/text reply parsed in memory
	maxDrain       = 256 << 20 // how far a truncated reply is read to count its size
	maxInlineBytes = 100_000   // JSON/text returned inline beyond this is truncated
)

type callArgs struct {
	Service string            `json:"service"`
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Query   map[string]any    `json:"query"`
	Body    json.RawMessage   `json:"body"`
	Headers map[string]string `json:"headers"`
	Form    map[string]any    `json:"form"`
	Files   map[string]string `json:"files"`
}

var callMethods = []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD"}

// forbiddenHeaders cannot be set by agents: they carry credentials, routing
// or framing that messh controls.
var forbiddenHeaders = []string{
	"authorization", "proxy-authorization", "cookie", "host", "content-length", "transfer-encoding",
	"connection", "upgrade", "te", "trailer", "x-forwarded-for", "x-forwarded-host", "forwarded",
}

// valueStrings renders a query/form value (scalar or array of scalars).
func valueStrings(v any) ([]string, error) {
	scalar := func(x any) (string, error) {
		switch t := x.(type) {
		case string:
			return t, nil
		case bool, float64:
			return fmt.Sprint(t), nil
		case nil:
			return "", nil
		}
		b, err := json.Marshal(x)
		return string(b), err
	}
	if arr, ok := v.([]any); ok {
		out := make([]string, 0, len(arr))
		for _, x := range arr {
			s, err := scalar(x)
			if err != nil {
				return nil, err
			}
			out = append(out, s)
		}
		return out, nil
	}
	s, err := scalar(v)
	return []string{s}, err
}

func toValues(m map[string]any) (url.Values, error) {
	vals := url.Values{}
	for k, v := range m {
		ss, err := valueStrings(v)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", k, err)
		}
		vals[k] = ss
	}
	return vals, nil
}

// planCall parses a service_call request for a REST or plain HTTP service.
func (p *Provider) planCall(args json.RawMessage) (*plan, error) {
	var a callArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	e, err := p.lookup(a.Service)
	if err != nil {
		return nil, err
	}
	if e.cfg.Kind != KindOpenAPI && e.cfg.Kind != KindHTTP {
		return nil, fmt.Errorf("service %s is kind %s; service_call is for openapi and http services", e.cfg.Name, e.cfg.Kind)
	}
	method := strings.ToUpper(a.Method)
	if !slices.Contains(callMethods, method) {
		return nil, fmt.Errorf("method must be one of %s", strings.Join(callMethods, ", "))
	}
	if err := cleanRel(a.Path); err != nil {
		return nil, err
	}
	decoded, _ := url.PathUnescape(a.Path) // validated by cleanRel
	if !permitsREST(&e.cfg, method, decoded) {
		return nil, fmt.Errorf("service %s does not allow %s %s", e.cfg.Name, method, decoded)
	}
	query, err := toValues(a.Query)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	form, err := toValues(a.Form)
	if err != nil {
		return nil, fmt.Errorf("form: %w", err)
	}
	hasBody := len(bytes.TrimSpace(a.Body)) > 0 && string(bytes.TrimSpace(a.Body)) != "null"
	multipartReq := len(a.Files) > 0
	switch {
	case hasBody && (len(a.Form) > 0 || multipartReq):
		return nil, errors.New("body cannot be combined with form or files")
	case (hasBody || len(a.Form) > 0 || multipartReq) && (method == "GET" || method == "HEAD"):
		return nil, fmt.Errorf("%s requests carry no body, form or files", method)
	}
	hdrs := http.Header{}
	for k, v := range a.Headers {
		lk := strings.ToLower(k)
		if !headerRE.MatchString(k) || slices.Contains(forbiddenHeaders, lk) ||
			(e.cfg.Auth != nil && strings.EqualFold(e.cfg.Auth.Header, k)) {
			return nil, fmt.Errorf("header %q cannot be set", k)
		}
		if strings.ContainsAny(v, "\r\n\x00") {
			return nil, fmt.Errorf("header %q has control characters", k)
		}
		hdrs.Set(k, v)
	}
	var inputs []fileInput
	fieldAbs := map[string]string{}
	fieldNames := make([]string, 0, len(a.Files))
	for field := range a.Files {
		fieldNames = append(fieldNames, field)
	}
	sort.Strings(fieldNames)
	for _, field := range fieldNames {
		ref, abs, err := p.resolveRef(a.Files[field])
		if err != nil {
			return nil, fmt.Errorf("files.%s: %w", field, err)
		}
		inputs = append(inputs, fileInput{Label: field, Ref: string(ref), Abs: abs})
		fieldAbs[field] = abs
	}
	var bodyCanon any
	if hasBody {
		if bodyCanon, err = canonical(a.Body); err != nil {
			return nil, fmt.Errorf("body is not valid JSON: %w", err)
		}
	}
	full, err := buildURL(e.cfg.URL, a.Path, query)
	if err != nil {
		return nil, err
	}

	e.mu.Lock()
	doc := e.doc
	e.mu.Unlock()
	template := doc.template(method, decoded)
	op := method + " " + template

	hdrNames := make([]string, 0, len(hdrs))
	for k := range hdrs {
		hdrNames = append(hdrNames, k)
	}
	sort.Strings(hdrNames)
	var details []provider.Detail
	if len(a.Query) > 0 {
		details = append(details, provider.Detail{Label: "Path", Value: decoded}, provider.Detail{Label: "Query", Value: clip(query.Encode(), 200)})
	} else if template != decoded {
		details = append(details, provider.Detail{Label: "Path", Value: decoded})
	}
	if hasBody {
		details = append(details, provider.Detail{Label: "Body", Value: summarizeBody(a.Body)})
	}
	if len(form) > 0 {
		keys := make([]string, 0, len(form))
		for k := range form {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			details = append(details, provider.Detail{Label: "Form " + k, Value: summarize(k, mustJSON(strings.Join(form[k], ", ")))})
		}
	}
	for _, in := range inputs {
		details = append(details, provider.Detail{Label: "File " + in.Label, Value: in.Ref})
	}
	if len(hdrNames) > 0 {
		details = append(details, provider.Detail{Label: "Extra headers", Value: strings.Join(hdrNames, ", ")})
	}

	exact := map[string]any{
		"method": method, "path": decoded, "query": query.Encode(),
		"form": form, "headers": hdrs,
	}
	if hasBody {
		exact["body_sha256"] = hashHex(bodyCanon)
	}
	pl := &plan{
		p: p, e: e, svc: e.cfg, tool: toolCall, op: op,
		readOnly: method == "GET" || method == "HEAD",
		details:  details, exact: exact, inputs: inputs,
		autoMatch: func(entry string) bool {
			pat, ok := parseOpPattern(entry)
			return ok && (pat.matches(method, template) || pat.matches(method, decoded))
		},
	}
	pl.run = func(ctx context.Context) (*mcp.CallToolResult, error) {
		return p.runCall(ctx, e, method, full, a, form, hdrs, fieldNames, fieldAbs)
	}
	return pl, nil
}

func mustJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

func summarizeBody(raw json.RawMessage) string {
	var buf bytes.Buffer
	if json.Compact(&buf, raw) != nil {
		return "(invalid)"
	}
	var v any
	if json.Unmarshal(raw, &v) == nil {
		if m, ok := v.(map[string]any); ok {
			var parts []string
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				rawv, _ := json.Marshal(m[k])
				parts = append(parts, k+"="+summarize(k, rawv))
			}
			return clip(strings.Join(parts, ", "), 300)
		}
	}
	return clip(buf.String(), 300)
}

// runCall performs the HTTP request and shapes the reply.
func (p *Provider) runCall(ctx context.Context, e *entry, method, target string, a callArgs, form url.Values, hdrs http.Header, fieldNames []string, fieldAbs map[string]string) (*mcp.CallToolResult, error) {
	ctx, cancel := context.WithTimeout(ctx, callTimeout)
	defer cancel()
	secrets := e.secret.secrets()
	fail := func(format string, args ...any) (*mcp.CallToolResult, error) {
		return provider.ErrorResult("service %s: %s", e.cfg.Name, redactString(fmt.Sprintf(format, args...), secrets)), nil
	}

	var body io.Reader
	ctype := ""
	var uploads []*uploadFile
	switch {
	case len(fieldNames) > 0 || len(form) > 0:
		pr, pw := io.Pipe()
		mw := multipart.NewWriter(pw)
		ctype = mw.FormDataContentType()
		body = pr
		for _, f := range fieldNames {
			fh, err := openUpload(fieldAbs[f])
			if err != nil {
				pr.Close()
				return fail("files.%s: %v", f, err)
			}
			uploads = append(uploads, fh)
		}
		defer func() {
			for _, u := range uploads {
				u.f.Close()
			}
		}()
		go func() { pw.CloseWithError(writeMultipart(mw, form, fieldNames, uploads)) }()
		defer pr.Close()
	case len(bytes.TrimSpace(a.Body)) > 0 && string(bytes.TrimSpace(a.Body)) != "null":
		body = bytes.NewReader(a.Body)
		ctype = "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return fail("%v", err)
	}
	for k, vs := range hdrs {
		req.Header[k] = vs
	}
	if ctype != "" && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", ctype)
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "application/json, */*;q=0.5")
	}
	resp, err := e.http.Do(req)
	if err != nil {
		e.requestProbe()
		return fail("%s", describeErr(unwrapURLError(err)))
	}
	defer resp.Body.Close()
	out, err := p.shapeResponse(e, method, resp)
	if err != nil {
		return fail("%v", err)
	}
	res, err := provider.JSONResult(out.payload)
	if err != nil {
		return nil, err
	}
	res.IsError = resp.StatusCode >= 400
	return redactResult(res, secrets), nil
}

type uploadFile struct {
	f    *os.File
	name string
}

func openUpload(abs string) (*uploadFile, error) {
	f, err := os.Open(abs)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("does not exist")
		}
		return nil, err
	}
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		f.Close()
		return nil, errors.New("not a regular file")
	}
	return &uploadFile{f: f, name: filepath.Base(abs)}, nil
}

func writeMultipart(mw *multipart.Writer, form url.Values, fields []string, uploads []*uploadFile) error {
	keys := make([]string, 0, len(form))
	for k := range form {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		for _, v := range form[k] {
			if err := mw.WriteField(k, v); err != nil {
				return err
			}
		}
	}
	for i, field := range fields {
		u := uploads[i]
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="%s"; filename="%s"`, quoteEscape(field), quoteEscape(u.name)))
		ct := mime.TypeByExtension(strings.ToLower(path.Ext(u.name)))
		if ct == "" {
			ct = "application/octet-stream"
		}
		h.Set("Content-Type", ct)
		part, err := mw.CreatePart(h)
		if err != nil {
			return err
		}
		if _, err := io.Copy(part, u.f); err != nil {
			return err
		}
	}
	return mw.Close()
}

var quoteEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

func quoteEscape(s string) string { return quoteEscaper.Replace(s) }

// unwrapURLError drops the request URL from transport errors.
func unwrapURLError(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

type shaped struct {
	payload map[string]any
}

// shapeResponse turns an HTTP reply into the service_call result payload:
// JSON stays JSON, text stays text, binary becomes an artifact.
func (p *Provider) shapeResponse(e *entry, method string, resp *http.Response) (shaped, error) {
	ct := resp.Header.Get("Content-Type")
	mt, _, _ := mime.ParseMediaType(ct)
	out := map[string]any{"status": resp.StatusCode}
	if mt != "" {
		out["content_type"] = mt
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		out["location"] = loc
	}
	if method == "HEAD" || resp.StatusCode == http.StatusNoContent {
		return shaped{out}, nil
	}

	br := newPeekReader(resp.Body)
	head := br.peek(512)
	kind := classify(mt, head)
	if kind == bodyBinary {
		if len(head) == 0 {
			return shaped{out}, nil
		}
		ext := ""
		if _, params, err := mime.ParseMediaType(resp.Header.Get("Content-Disposition")); err == nil {
			ext = safeExt(params["filename"])
		}
		useMime := mt
		if useMime == "" {
			useMime = sniffBlob(head)
		}
		if useMime == "" {
			useMime = "application/octet-stream"
		}
		sk := &sink{paths: p.paths, source: e.cfg.Name}
		a, err := sk.saveReader(br, useMime, ext)
		if err != nil {
			return shaped{}, err
		}
		out["artifact"] = a
		return shaped{out}, nil
	}

	data, err := io.ReadAll(io.LimitReader(br, maxBuffered+1))
	if err != nil {
		return shaped{}, err
	}
	total := int64(len(data))
	if total > maxBuffered {
		n, _ := io.Copy(io.Discard, io.LimitReader(br, maxDrain))
		total += n
		out["truncated"] = true
		out["bytes"] = total
		if total > maxBuffered+maxDrain {
			out["bytes_at_least"] = true
		}
		out["text"] = cutUTF8(data, maxInlineBytes)
		return shaped{out}, nil
	}
	if kind == bodyJSON {
		var v any
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if err := dec.Decode(&v); err == nil && !dec.More() {
			sk := &sink{paths: p.paths, source: e.cfg.Name}
			nv, changed, serr := sk.scrub(v)
			if serr != nil {
				sk.discard()
				return shaped{}, serr
			}
			enc, _ := json.Marshal(nv)
			if len(enc) <= maxInlineBytes {
				out["json"] = json.RawMessage(enc)
				if changed {
					out["artifacts"] = sk.made
				}
				return shaped{out}, nil
			}
			sk.discard()
			out["truncated"] = true
			out["bytes"] = total
			out["text"] = cutUTF8(data, maxInlineBytes)
			return shaped{out}, nil
		}
		// Declared JSON that does not parse is handed back as text.
	}
	if total > maxInlineBytes {
		out["truncated"] = true
		out["bytes"] = total
		out["text"] = cutUTF8(data, maxInlineBytes)
		return shaped{out}, nil
	}
	out["text"] = string(data)
	return shaped{out}, nil
}

// cutUTF8 returns the first n bytes of b without splitting a character.
func cutUTF8(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	cut := n
	for cut > 0 && b[cut]&0xC0 == 0x80 {
		cut--
	}
	return string(b[:cut])
}

type bodyKind int

const (
	bodyBinary bodyKind = iota
	bodyJSON
	bodyText
)

// classify decides how to treat a reply from its media type and first bytes.
func classify(mt string, head []byte) bodyKind {
	switch {
	case mt == "application/json" || strings.HasSuffix(mt, "+json"):
		return bodyJSON
	case strings.HasPrefix(mt, "text/"), mt == "application/xml", strings.HasSuffix(mt, "+xml"),
		mt == "application/javascript", mt == "application/x-www-form-urlencoded", mt == "application/yaml", mt == "application/x-ndjson":
		return bodyText
	case mt == "" || mt == "application/octet-stream":
		if len(head) == 0 {
			return bodyText
		}
		if sniffBlob(head) == "" && strings.HasPrefix(http.DetectContentType(head), "text/") {
			return bodyText
		}
	}
	return bodyBinary
}

// safeExt returns a filename's extension when it is short and plain.
func safeExt(name string) string {
	ext := strings.ToLower(path.Ext(strings.ReplaceAll(name, `\`, "/")))
	if len(ext) < 2 || len(ext) > 8 {
		return ""
	}
	for _, r := range ext[1:] {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return ""
		}
	}
	return ext
}

// peekReader lets the first bytes of a stream be inspected and still read.
type peekReader struct {
	r    io.Reader
	buf  []byte
	done bool
}

func newPeekReader(r io.Reader) *peekReader { return &peekReader{r: r} }

func (p *peekReader) peek(n int) []byte {
	if !p.done {
		buf := make([]byte, n)
		got, _ := io.ReadFull(p.r, buf)
		p.buf = buf[:got]
		p.done = true
	}
	return p.buf
}

func (p *peekReader) Read(b []byte) (int, error) {
	if len(p.buf) > 0 {
		n := copy(b, p.buf)
		p.buf = p.buf[n:]
		return n, nil
	}
	return p.r.Read(b)
}
