package files

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Wire format of the mesh file endpoints. Mount NewHandler at both
// Path and Path+"/" behind peer authentication.
//
//	GET  /v1/files?ref=R[&stat=1|&recursive=1][&hash=1]  list or stat (no ref: the roots)
//	GET  /v1/files/R                                     download; Range supported; the
//	     full response ends with an X-Messh-Sha256 trailer
//	PUT  /v1/files/R                                     upload to ws/ only; X-Messh-Size is
//	     required and X-Messh-Sha256 is sent as a header (if known up front) or a trailer
//	POST /v1/files/R                                     mkdir -p under ws/
const (
	Path = "/v1/files"

	HeaderSize      = "X-Messh-Size"
	HeaderSHA256    = "X-Messh-Sha256"
	HeaderOverwrite = "X-Messh-Overwrite" // "1" replaces an existing file
	HeaderExec      = "X-Messh-Exec"      // "1" marks the file executable
	HeaderMtime     = "X-Messh-Mtime"     // modification time, unix nanoseconds
	HeaderAgent     = "X-Messh-Agent"

	copyBufSize = 256 << 10
)

// errBody is the JSON error payload. Code lets the client rebuild the
// sentinel error so callers can use errors.Is across the mesh.
type errBody struct {
	Error string `json:"error"`
	Code  string `json:"code,omitempty"`
}

var errCodes = []struct {
	err    error
	code   string
	status int
}{
	{ErrNotFound, "not_found", http.StatusNotFound},
	{ErrExists, "exists", http.StatusConflict},
	{ErrIsDirectory, "is_directory", http.StatusConflict},
	{ErrNotDirectory, "not_directory", http.StatusConflict},
	{ErrCapabilityDenied, "capability_denied", http.StatusForbidden},
	{ErrReadOnly, "read_only", http.StatusForbidden},
	{ErrRoot, "root", http.StatusForbidden},
	{ErrNotRegular, "not_regular", http.StatusUnprocessableEntity},
	{ErrIntegrity, "integrity", http.StatusUnprocessableEntity},
	{ErrNoSpace, "no_space", http.StatusInsufficientStorage},
	{ErrInvalid, "invalid", http.StatusBadRequest},
}

type handler struct{ s *Store }

// NewHandler serves the file endpoints for s.
func NewHandler(s *Store) http.Handler { return &handler{s} }

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == Path {
		if r.Method != http.MethodGet {
			writeErr(w, r, http.StatusMethodNotAllowed, "", errors.New("method not allowed"))
			return
		}
		h.list(w, r)
		return
	}
	raw, ok := strings.CutPrefix(r.URL.Path, Path+"/")
	if !ok {
		writeErr(w, r, http.StatusNotFound, "", errors.New("not found"))
		return
	}
	// Only canonical refs are served so there is exactly one name per file.
	if ref, err := ParseRef(raw); err != nil || string(ref) != raw {
		writeErr(w, r, http.StatusBadRequest, "invalid", fmt.Errorf("%w: %q is not a canonical file reference", ErrInvalid, raw))
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.download(w, r, raw)
	case http.MethodPut:
		h.upload(w, r, raw)
	case http.MethodPost:
		if err := h.s.Mkdir(raw); err != nil {
			writeFail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"ref": raw})
	default:
		writeErr(w, r, http.StatusMethodNotAllowed, "", errors.New("method not allowed"))
	}
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	ref := q.Get("ref")
	var (
		l   Listing
		err error
	)
	switch {
	case q.Get("stat") == "1":
		l.Info, err = h.s.Stat(ref, q.Get("hash") == "1")
	case q.Get("recursive") == "1":
		l, err = h.s.Walk(ref)
	default:
		l, err = h.s.List(ref)
	}
	if err != nil {
		writeFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, l)
}

func (h *handler) download(w http.ResponseWriter, r *http.Request, ref string) {
	f, info, err := h.s.OpenFile(ref)
	if err != nil {
		writeFail(w, r, err)
		return
	}
	defer f.Close()
	hd := w.Header()
	hd.Set("Content-Type", "application/octet-stream")
	hd.Set("Accept-Ranges", "bytes")
	hd.Set("ETag", fmt.Sprintf(`"%x-%x"`, info.Size, info.ModTime.UnixNano()))
	hd.Set(HeaderSize, strconv.FormatInt(info.Size, 10))
	hd.Set(HeaderMtime, strconv.FormatInt(info.ModTime.UnixNano(), 10))
	if info.Exec {
		hd.Set(HeaderExec, "1")
	}
	if r.Header.Get("Range") != "" {
		// Resumed transfers carry no trailer; the client verifies them
		// with a hash request once complete.
		http.ServeContent(w, r, "", info.ModTime, f)
		return
	}
	hd.Set("Last-Modified", info.ModTime.UTC().Format(http.TimeFormat))
	hd.Set("Trailer", HeaderSHA256)
	w.WriteHeader(http.StatusOK)
	sum := sha256.New()
	n, err := io.CopyBuffer(io.MultiWriter(w, sum), io.LimitReader(f, info.Size), make([]byte, copyBufSize))
	if err != nil || n != info.Size {
		// Cutting the response off is the only way to tell the client the
		// body is incomplete (the file shrank, or the client went away).
		panic(http.ErrAbortHandler)
	}
	hd.Set(HeaderSHA256, hex.EncodeToString(sum.Sum(nil)))
}

func (h *handler) upload(w http.ResponseWriter, r *http.Request, ref string) {
	size, err := strconv.ParseInt(r.Header.Get(HeaderSize), 10, 64)
	if err != nil || size < 0 {
		writeFail(w, r, fmt.Errorf("%w: %s header is required", ErrInvalid, HeaderSize))
		return
	}
	opt := WriteOptions{
		Size:      size,
		Overwrite: r.Header.Get(HeaderOverwrite) == "1",
		Exec:      r.Header.Get(HeaderExec) == "1",
	}
	if ns, err := strconv.ParseInt(r.Header.Get(HeaderMtime), 10, 64); err == nil {
		opt.ModTime = time.Unix(0, ns)
	}
	u, err := h.s.BeginWrite(ref, opt)
	if err != nil {
		writeFail(w, r, err)
		return
	}
	defer u.Abort()
	if _, err := io.CopyBuffer(u, r.Body, make([]byte, copyBufSize)); err != nil {
		writeFail(w, r, err)
		return
	}
	// A trailer is only populated once the body has been read to EOF.
	expect := r.Header.Get(HeaderSHA256)
	if expect == "" {
		expect = r.Trailer.Get(HeaderSHA256)
	}
	if expect == "" {
		writeFail(w, r, fmt.Errorf("%w: the sender must announce the SHA-256 of the file", ErrInvalid))
		return
	}
	info, err := u.Commit(expect)
	if err != nil {
		writeFail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func writeFail(w http.ResponseWriter, r *http.Request, err error) {
	for _, c := range errCodes {
		if errors.Is(err, c.err) {
			writeErr(w, r, c.status, c.code, err)
			return
		}
	}
	writeErr(w, r, http.StatusInternalServerError, "", err)
}

func writeErr(w http.ResponseWriter, r *http.Request, status int, code string, err error) {
	writeJSON(w, status, errBody{Error: err.Error(), Code: code})
	if r.ProtoMajor != 2 || r.Method != http.MethodPut {
		return
	}
	// Keep the stream alive until queued request trailers arrive or the client
	// cancels it. Otherwise Go's HTTP/2 server can treat late trailer HEADERS as
	// an out-of-order new stream and shut down the entire connection.
	if err := http.NewResponseController(w).Flush(); err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, r.Body)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
