package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	defaultIdleTimeout = 60 * time.Second
	defaultRetries     = 3
	finalizeTimeout    = 5 * time.Minute
)

var errStalled = errors.New("no data moved for too long; the other device or the network stalled")

// Client is the Endpoint for a paired peer's file space. HTTP must already
// pin the peer's certificate (see the node's pinned transport).
type Client struct {
	HTTP *http.Client
	Base string // "https://host:port"
	// IdleTimeout aborts a request when no bytes move for this long
	// (default 60s); it does not limit total transfer time.
	IdleTimeout time.Duration
	// Retries is how often an interrupted download resumes with a Range
	// request before giving up (default 3).
	Retries     int
	CallerAgent string
}

func (c *Client) setCaller(req *http.Request) {
	if c.CallerAgent != "" {
		req.Header.Set(HeaderAgent, c.CallerAgent)
	}
}

func (c *Client) idle() time.Duration {
	if c.IdleTimeout > 0 {
		return c.IdleTimeout
	}
	return defaultIdleTimeout
}

func (c *Client) refURL(ref string) string {
	u := url.URL{Path: Path + "/" + ref}
	return c.Base + u.EscapedPath()
}

func (c *Client) queryURL(q url.Values) string { return c.Base + Path + "?" + q.Encode() }

// remoteError is an error reported by the peer; it unwraps to the matching
// sentinel so errors.Is works as it does locally.
type remoteError struct {
	msg      string
	sentinel error
	code     string
}

func (e *remoteError) Error() string     { return e.msg }
func (e *remoteError) Unwrap() error     { return e.sentinel }
func (e *remoteError) ErrorCode() string { return e.code }

func decodeError(resp *http.Response) error {
	var body []byte
	var eb errBody
	contentType, _, _ := strings.Cut(resp.Header.Get("Content-Type"), ";")
	if strings.EqualFold(strings.TrimSpace(contentType), "application/json") {
		// Decode the error without waiting for response EOF. A rejected upload
		// must close its response so the server can stop draining its request.
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&eb)
	} else {
		body, _ = io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = json.Unmarshal(body, &eb)
	}
	e := &remoteError{msg: eb.Error, code: eb.Code}
	if e.msg == "" {
		e.msg = fmt.Sprintf("the other device answered %s", resp.Status)
		if t := strings.TrimSpace(string(body)); t != "" && len(t) < 200 {
			e.msg += ": " + t
		}
	}
	for _, c := range errCodes {
		if c.code == eb.Code {
			e.sentinel = c.err
		}
	}
	switch eb.Code {
	case "no_matching_grant", "grant_expired", "grant_revoked":
		e.sentinel = ErrCapabilityDenied
	}
	return e
}

func (c *Client) getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	return c.doJSON(req, v, nil)
}

func (c *Client) doJSON(req *http.Request, v any, cancelUpload context.CancelCauseFunc) error {
	c.setCaller(req)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() {
		// HTTP/2 response Close waits for the request writer. Cancel an upload
		// first so a blocked source cannot delay returning its rejection.
		if cancelUpload != nil {
			cancelUpload(nil)
		}
		resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusOK {
		return decodeError(resp)
	}
	if v == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(v)
}

func (c *Client) Stat(ctx context.Context, ref string) (Info, error) {
	return c.stat(ctx, ref, false)
}

// StatHash is Stat with the file's SHA-256, computed by the peer.
func (c *Client) StatHash(ctx context.Context, ref string) (Info, error) {
	return c.stat(ctx, ref, true)
}

func (c *Client) stat(ctx context.Context, ref string, hash bool) (Info, error) {
	q := url.Values{"ref": {ref}, "stat": {"1"}}
	if hash {
		q.Set("hash", "1")
	}
	var l Listing
	err := c.getJSON(ctx, c.queryURL(q), &l)
	return l.Info, err
}

// List lists a directory; the empty ref lists the roots.
func (c *Client) List(ctx context.Context, ref string) (Listing, error) {
	var l Listing
	err := c.getJSON(ctx, c.queryURL(url.Values{"ref": {ref}}), &l)
	return l, err
}

func (c *Client) Walk(ctx context.Context, ref string) (Listing, error) {
	var l Listing
	err := c.getJSON(ctx, c.queryURL(url.Values{"ref": {ref}, "recursive": {"1"}}), &l)
	return l, err
}

func (c *Client) Mkdir(ctx context.Context, ref string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.refURL(ref), nil)
	if err != nil {
		return err
	}
	return c.doJSON(req, nil, nil)
}

// Put uploads src. The SHA-256 travels as a trailer, so the sender never
// has to read the data twice; the receiver rejects the file if its own digest
// differs.
func (c *Client) Put(ctx context.Context, ref string, opt WriteOptions, src Source) (Info, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	idle := c.idle()
	timer := time.AfterFunc(idle, func() { cancel(errStalled) })
	defer timer.Stop()

	body := &putBody{src: src, size: opt.Size, h: sha256.New(), timer: timer, idle: idle, trailer: http.Header{HeaderSHA256: nil}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.refURL(ref), body)
	if err != nil {
		return Info{}, err
	}
	req.ContentLength = -1 // trailers require chunked/streamed framing
	req.Trailer = body.trailer
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set(HeaderSize, strconv.FormatInt(opt.Size, 10))
	if opt.Overwrite {
		req.Header.Set(HeaderOverwrite, "1")
	}
	if opt.Exec {
		req.Header.Set(HeaderExec, "1")
	}
	if !opt.ModTime.IsZero() {
		req.Header.Set(HeaderMtime, strconv.FormatInt(opt.ModTime.UnixNano(), 10))
	}
	var info Info
	err = c.doJSON(req, &info, cancel)
	switch {
	case err == nil:
		return info, nil
	case body.srcErr != nil:
		return Info{}, body.srcErr
	case errors.Is(context.Cause(ctx), errStalled):
		return Info{}, errStalled
	}
	return Info{}, err
}

// putBody streams a Source as a request body and fills in the digest
// trailer when the source ends.
type putBody struct {
	src     Source
	size    int64
	h       hash.Hash
	sent    int64
	timer   *time.Timer
	idle    time.Duration
	trailer http.Header
	srcErr  error
}

func (b *putBody) Read(p []byte) (int, error) {
	n, err := b.src.Read(p)
	b.h.Write(p[:n])
	b.sent += int64(n)
	b.timer.Reset(b.idle)
	switch {
	case err == io.EOF:
		if b.sent != b.size {
			b.srcErr = fmt.Errorf("%w: the source delivered %d bytes but announced %d", ErrIntegrity, b.sent, b.size)
			return n, b.srcErr
		}
		// Prefer the digest the source vouches for (e.g. the origin device's
		// own) over one computed here, so corruption anywhere on the way is
		// caught by the receiver.
		sum := b.src.SHA256()
		if sum == "" {
			sum = hex.EncodeToString(b.h.Sum(nil))
		}
		b.trailer.Set(HeaderSHA256, sum)
		// The receiver now flushes the file to disk, which can take long on
		// slow media; the idle limit must not fire during that.
		b.timer.Reset(finalizeTimeout)
	case err != nil:
		b.srcErr = err
	}
	return n, err
}

func (b *putBody) Close() error { return nil }

// Open starts a download. An interrupted transfer resumes with a Range
// request (guarded by the file's ETag) and the digest keeps accumulating, so
// the end-to-end check still covers every byte.
func (c *Client) Open(ctx context.Context, ref string) (Stream, error) {
	s := &getStream{c: c, parent: ctx, ref: ref, h: sha256.New()}
	if err := s.connect(); err != nil {
		return nil, err
	}
	return s, nil
}

type getStream struct {
	c      *Client
	parent context.Context
	ref    string
	info   Info
	etag   string
	h      hash.Hash
	n      int64

	resp    *http.Response
	actx    context.Context
	cancel  context.CancelCauseFunc
	timer   *time.Timer
	retries int
	resumed bool
	err     error
	sum     string
}

func (s *getStream) Info() Info     { return s.info }
func (s *getStream) SHA256() string { return s.sum }

func (s *getStream) release() {
	if s.resp == nil {
		return
	}
	s.timer.Stop()
	s.resp.Body.Close()
	s.cancel(nil)
	s.resp = nil
}

func (s *getStream) Close() error {
	s.release()
	return nil
}

func (s *getStream) connect() error {
	s.release()
	idle := s.c.idle()
	actx, cancel := context.WithCancelCause(s.parent)
	timer := time.AfterFunc(idle, func() { cancel(errStalled) })
	fail := func(err error) error {
		timer.Stop()
		cancel(nil)
		return err
	}
	req, err := http.NewRequestWithContext(actx, http.MethodGet, s.c.refURL(s.ref), nil)
	if err != nil {
		return fail(err)
	}
	if s.n > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", s.n))
		req.Header.Set("If-Range", s.etag)
	}
	s.c.setCaller(req)
	resp, err := s.c.HTTP.Do(req)
	if err != nil {
		if errors.Is(context.Cause(actx), errStalled) {
			err = errStalled
		}
		return fail(err)
	}
	switch {
	case s.n == 0 && resp.StatusCode == http.StatusOK:
		size, err := strconv.ParseInt(resp.Header.Get(HeaderSize), 10, 64)
		if err != nil {
			resp.Body.Close()
			return fail(errors.New("the other device did not announce the file size"))
		}
		s.info = Info{Ref: s.ref, Name: path.Base(s.ref), Size: size, Exec: resp.Header.Get(HeaderExec) == "1"}
		if ns, err := strconv.ParseInt(resp.Header.Get(HeaderMtime), 10, 64); err == nil {
			s.info.ModTime = time.Unix(0, ns).UTC()
		}
		s.etag = resp.Header.Get("ETag")
	case s.n > 0 && resp.StatusCode == http.StatusPartialContent && strings.HasPrefix(resp.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-", s.n)):
		s.resumed = true
	case s.n > 0 && resp.StatusCode == http.StatusOK:
		resp.Body.Close()
		return fail(fmt.Errorf("%s changed on the other device while it was being copied", s.ref))
	default:
		err := decodeError(resp)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent {
			err = errors.New("unexpected response to a resumed download")
		}
		return fail(err)
	}
	s.resp, s.actx, s.cancel, s.timer = resp, actx, cancel, timer
	return nil
}

// Read returns data until the verified end; every terminal outcome, error or
// io.EOF, is sticky.
func (s *getStream) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	n, err := s.read(p)
	if err != nil {
		s.err = err
	}
	return n, err
}

func (s *getStream) read(p []byte) (int, error) {
	for {
		n, err := s.resp.Body.Read(p)
		if n > 0 {
			s.h.Write(p[:n])
			s.n += int64(n)
			s.timer.Reset(s.c.idle())
			return n, nil // a sticky error is returned again by the next Read
		}
		if err == nil {
			continue
		}
		if err == io.EOF && s.n == s.info.Size {
			return 0, s.finish()
		}
		if errors.Is(context.Cause(s.actx), errStalled) {
			err = errStalled
		}
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		retries := s.c.Retries
		if retries <= 0 {
			retries = defaultRetries
		}
		if s.retries >= retries || s.parent.Err() != nil || s.n >= s.info.Size {
			return 0, fmt.Errorf("reading %s: %w", s.ref, err)
		}
		s.retries++
		select {
		case <-time.After(time.Duration(s.retries) * 500 * time.Millisecond):
		case <-s.parent.Done():
			return 0, context.Cause(s.parent)
		}
		if cerr := s.connect(); cerr != nil {
			return 0, fmt.Errorf("reading %s (resuming at byte %d after %v): %w", s.ref, s.n, err, cerr)
		}
	}
}

// finish checks the digest once all bytes have arrived. A complete, never
// interrupted download is checked against the trailer; a resumed one (which
// has no trailer) against a hash computed by the peer.
func (s *getStream) finish() error {
	trailer := s.resp.Trailer.Get(HeaderSHA256)
	s.release()
	local := hex.EncodeToString(s.h.Sum(nil))
	remote := trailer
	if s.resumed || remote == "" {
		in, err := s.c.StatHash(s.parent, s.ref)
		if err != nil {
			return fmt.Errorf("verifying %s: %w", s.ref, err)
		}
		if in.Size != s.info.Size {
			return fmt.Errorf("%s changed on the other device while it was being copied", s.ref)
		}
		remote = in.SHA256
	}
	if !strings.EqualFold(local, remote) {
		return fmt.Errorf("%s: %w: received bytes hash to %s but the other device says %s", s.ref, ErrIntegrity, local, remote)
	}
	s.sum = remote
	return io.EOF
}
