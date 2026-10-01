package catalog

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/files"
	"messh/internal/state"
)

const (
	// maxInlineText bounds text returned to an agent; longer output is cut
	// with a marker so one reply cannot flood a model's context.
	maxInlineText = 256 << 10
	// minBlobString is the shortest base64 string considered for extraction
	// when its content has a recognisable media signature.
	minBlobString = 1 << 10
	// minOpaqueBlobString extracts large base64 strings of unknown type too.
	minOpaqueBlobString = 64 << 10
	// maxArtifactBytes bounds one captured file.
	maxArtifactBytes = 1 << 30
)

// Artifact describes a file messh captured from a service reply.
type Artifact struct {
	Ref   string `json:"ref"`
	Mime  string `json:"mime"`
	Bytes int64  `json:"bytes"`
}

// sink writes captured payloads into artifacts/<source>/ and remembers them.
type sink struct {
	paths  state.Paths
	source string
	made   []Artifact
}

var mimeExt = map[string]string{
	"audio/wav": ".wav", "audio/x-wav": ".wav", "audio/wave": ".wav", "audio/vnd.wave": ".wav",
	"audio/mpeg": ".mp3", "audio/mp3": ".mp3", "audio/ogg": ".ogg", "audio/opus": ".opus",
	"audio/flac": ".flac", "audio/x-flac": ".flac", "audio/webm": ".webm",
	"audio/mp4": ".m4a", "audio/aac": ".aac", "audio/x-m4a": ".m4a",
	"image/png": ".png", "image/jpeg": ".jpg", "image/gif": ".gif", "image/webp": ".webp",
	"image/svg+xml": ".svg", "image/bmp": ".bmp",
	"video/mp4": ".mp4", "video/webm": ".webm",
	"application/pdf": ".pdf", "application/zip": ".zip", "application/gzip": ".gz",
	"application/x-tar": ".tar", "application/octet-stream": ".bin",
}

// extFor picks a file extension from a media type, falling back to .bin.
func extFor(mime string) string {
	m, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(mime)), ";")
	if e, ok := mimeExt[strings.TrimSpace(m)]; ok {
		return e
	}
	return ".bin"
}

// sniffBlob recognises common audio, image, video and archive signatures.
func sniffBlob(b []byte) string {
	has := func(off int, s string) bool { return len(b) >= off+len(s) && string(b[off:off+len(s)]) == s }
	switch {
	case has(0, "RIFF") && has(8, "WAVE"):
		return "audio/wav"
	case has(0, "RIFF") && has(8, "WEBP"):
		return "image/webp"
	case has(0, "ID3"), len(b) > 2 && b[0] == 0xFF && b[1]&0xE0 == 0xE0 && b[1]&0x06 != 0:
		return "audio/mpeg"
	case has(0, "fLaC"):
		return "audio/flac"
	case has(0, "OggS"):
		return "audio/ogg"
	case has(0, "\x89PNG\r\n\x1a\n"):
		return "image/png"
	case has(0, "\xff\xd8\xff"):
		return "image/jpeg"
	case has(0, "GIF87a"), has(0, "GIF89a"):
		return "image/gif"
	case has(0, "%PDF-"):
		return "application/pdf"
	case has(0, "PK\x03\x04"):
		return "application/zip"
	case has(4, "ftypM4A"):
		return "audio/mp4"
	case has(4, "ftyp"):
		return "video/mp4"
	case has(0, "\x1a\x45\xdf\xa3"):
		return "video/webm"
	}
	return ""
}

// saveBytes stores data as a new artifact. mime may be empty; it is sniffed.
func (s *sink) saveBytes(data []byte, mime string) (Artifact, error) {
	if mime == "" || mime == "application/octet-stream" {
		if m := sniffBlob(data); m != "" {
			mime = m
		}
	}
	if mime == "" {
		mime = "application/octet-stream"
	}
	return s.saveReader(bytes.NewReader(data), mime, "")
}

// saveReader streams r into a new artifact (ext overrides the mime-derived one).
func (s *sink) saveReader(r io.Reader, mime, ext string) (Artifact, error) {
	if ext == "" {
		ext = extFor(mime)
	}
	ref, f, err := files.NewArtifact(s.paths, s.source, ext)
	if err != nil {
		return Artifact{}, fmt.Errorf("create artifact: %w", err)
	}
	abs, _ := files.Resolve(s.paths, ref)
	n, err := io.Copy(f, io.LimitReader(r, maxArtifactBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > maxArtifactBytes {
		err = fmt.Errorf("payload exceeds %d bytes", int64(maxArtifactBytes))
	}
	if err != nil {
		os.Remove(abs)
		return Artifact{}, fmt.Errorf("write artifact: %w", err)
	}
	a := Artifact{Ref: string(ref), Mime: mime, Bytes: n}
	s.made = append(s.made, a)
	return a, nil
}

// decodeBlobString recognises a base64 payload (optionally a data: URI) that
// is large enough to be worth capturing and returns the bytes and media type.
func decodeBlobString(str string) ([]byte, string, bool) {
	mime := ""
	if rest, ok := strings.CutPrefix(str, "data:"); ok {
		head, payload, ok := strings.Cut(rest, ",")
		if !ok || !strings.HasSuffix(head, ";base64") {
			return nil, "", false
		}
		mime, str = strings.TrimSuffix(head, ";base64"), payload
		if strings.HasPrefix(mime, "text/") {
			return nil, "", false
		}
	}
	if len(str) < minBlobString {
		return nil, "", false
	}
	var raw []byte
	var err error
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if raw, err = enc.DecodeString(str); err == nil {
			break
		}
	}
	if err != nil {
		return nil, "", false
	}
	if m := sniffBlob(raw); m != "" {
		if mime == "" || mime == "application/octet-stream" {
			mime = m
		}
		return raw, mime, true
	}
	if mime != "" || (len(str) >= minOpaqueBlobString && !utf8.Valid(raw)) {
		return raw, mime, true
	}
	return nil, "", false
}

// scrub walks decoded JSON and replaces base64 media strings with artifact refs.
func (s *sink) scrub(v any) (any, bool, error) {
	switch t := v.(type) {
	case string:
		raw, mime, ok := decodeBlobString(t)
		if !ok {
			return v, false, nil
		}
		a, err := s.saveBytes(raw, mime)
		if err != nil {
			return v, false, err
		}
		return a.Ref, true, nil
	case map[string]any:
		changed := false
		for k, e := range t {
			ne, c, err := s.scrub(e)
			if err != nil {
				return v, false, err
			}
			if c {
				t[k], changed = ne, true
			}
		}
		return t, changed, nil
	case []any:
		changed := false
		for i, e := range t {
			ne, c, err := s.scrub(e)
			if err != nil {
				return v, false, err
			}
			if c {
				t[i], changed = ne, true
			}
		}
		return t, changed, nil
	}
	return v, false, nil
}

// scrubText applies scrub to text that is JSON or one bare blob string.
func (s *sink) scrubText(text string) (string, bool, error) {
	trim := strings.TrimSpace(text)
	if len(trim) < minBlobString {
		return text, false, nil
	}
	if trim[0] == '{' || trim[0] == '[' {
		dec := json.NewDecoder(strings.NewReader(trim))
		dec.UseNumber()
		var v any
		if err := dec.Decode(&v); err != nil || dec.More() {
			return text, false, nil
		}
		nv, changed, err := s.scrub(v)
		if err != nil || !changed {
			return text, false, err
		}
		out, err := json.Marshal(nv)
		return string(out), true, err
	}
	nv, changed, err := s.scrub(trim)
	if err != nil || !changed {
		return text, false, err
	}
	return nv.(string), true, nil
}

// limitText cuts s to maxInlineText bytes (on a rune boundary) with a marker.
func limitText(s string) string {
	if len(s) <= maxInlineText {
		return s
	}
	cut := maxInlineText
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return fmt.Sprintf("%s\n[truncated: showing %d of %d bytes]", s[:cut], cut, len(s))
}

// convert rewrites an upstream MCP result: binary content and base64 media
// inside text/structured JSON become artifact refs; oversized text is cut.
func (s *sink) convert(res *mcp.CallToolResult) (out *mcp.CallToolResult, err error) {
	defer func() {
		if err != nil {
			s.discard()
		}
	}()
	return s.convertInner(res)
}

func (s *sink) convertInner(res *mcp.CallToolResult) (*mcp.CallToolResult, error) {
	out := &mcp.CallToolResult{IsError: res.IsError}
	for _, c := range res.Content {
		switch t := c.(type) {
		case *mcp.TextContent:
			text, _, err := s.scrubText(t.Text)
			if err != nil {
				return nil, err
			}
			out.Content = append(out.Content, &mcp.TextContent{Text: limitText(text)})
		case *mcp.ImageContent:
			if err := s.addBlob(out, t.Data, t.MIMEType); err != nil {
				return nil, err
			}
		case *mcp.AudioContent:
			if err := s.addBlob(out, t.Data, t.MIMEType); err != nil {
				return nil, err
			}
		case *mcp.EmbeddedResource:
			if t.Resource != nil && len(t.Resource.Blob) > 0 {
				if err := s.addBlob(out, t.Resource.Blob, t.Resource.MIMEType); err != nil {
					return nil, err
				}
			} else if t.Resource != nil {
				text, _, err := s.scrubText(t.Resource.Text)
				if err != nil {
					return nil, err
				}
				out.Content = append(out.Content, &mcp.TextContent{Text: limitText(text)})
			}
		case *mcp.ResourceLink:
			// The URI names a place on the upstream, not on this device; show only what it is.
			out.Content = append(out.Content, &mcp.TextContent{Text: fmt.Sprintf("[resource link %q %s]", t.Name, t.MIMEType)})
		default:
			out.Content = append(out.Content, c)
		}
	}
	if res.StructuredContent != nil {
		raw, err := json.Marshal(res.StructuredContent)
		if err == nil {
			text, _, serr := s.scrubText(string(raw))
			if serr != nil {
				return nil, serr
			}
			if len(text) > maxInlineText {
				// Structured copy is redundant with the (truncated) text content.
			} else {
				out.StructuredContent = json.RawMessage(text)
			}
		}
	}
	if len(s.made) > 0 {
		list, _ := json.Marshal(map[string]any{"artifacts": s.made})
		out.Content = append(out.Content, &mcp.TextContent{Text: string(list)})
	}
	return out, nil
}

func (s *sink) addBlob(out *mcp.CallToolResult, data []byte, mime string) error {
	a, err := s.saveBytes(data, mime)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(map[string]any{"artifact": a})
	out.Content = append(out.Content, &mcp.TextContent{Text: string(b)})
	return nil
}

// discard removes everything captured so far (used when a call fails midway).
func (s *sink) discard() {
	for _, a := range s.made {
		if abs, err := files.Resolve(s.paths, files.Ref(a.Ref)); err == nil {
			os.Remove(abs)
		}
	}
	s.made = nil
}
