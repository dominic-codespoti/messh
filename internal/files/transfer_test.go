package files

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// bothProtocols runs fn against an HTTP/2 and an HTTP/1.1 server: trailers
// are framed differently in each.
func bothProtocols(t *testing.T, fn func(t *testing.T, s *Store, root string, c *Client)) {
	for _, h2 := range []bool{true, false} {
		t.Run(map[bool]string{true: "h2", false: "h1"}[h2], func(t *testing.T) {
			s, root := newTestStore(t)
			mux := http.NewServeMux()
			h := NewHandler(s)
			mux.Handle(Path, h)
			mux.Handle(Path+"/", h)
			srv := httptest.NewUnstartedServer(mux)
			srv.EnableHTTP2 = h2
			srv.StartTLS()
			t.Cleanup(srv.Close)
			c := &Client{HTTP: srv.Client(), Base: srv.URL, IdleTimeout: 5 * time.Second}
			fn(t, s, root, c)
		})
	}
}

func randomBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// bytesSource is a Source over memory that vouches for its digest.
type bytesSource struct {
	r      *bytes.Reader
	digest string
}

func newBytesSource(b []byte) *bytesSource {
	return &bytesSource{r: bytes.NewReader(b), digest: sum(b)}
}
func (s *bytesSource) Read(p []byte) (int, error) { return s.r.Read(p) }
func (s *bytesSource) SHA256() string             { return s.digest }

func readAll(t *testing.T, st Stream) []byte {
	t.Helper()
	b, err := io.ReadAll(st)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestPutAndOpenOverTheWire(t *testing.T) {
	bothProtocols(t, func(t *testing.T, s *Store, root string, c *Client) {
		ctx := t.Context()
		for _, size := range []int{0, 1, 3<<20 + 17} {
			data := randomBytes(t, size)
			ref := fmt.Sprintf("ws/job/f%d.bin", size)
			mt := time.Unix(1700000000, 0)
			got, err := c.Put(ctx, ref, WriteOptions{Size: int64(size), ModTime: mt, Exec: true}, newBytesSource(data))
			if err != nil {
				t.Fatalf("put %d bytes: %v", size, err)
			}
			if got.SHA256 != sum(data) || got.Size != int64(size) {
				t.Fatalf("put result %+v", got)
			}
			st, err := c.Open(ctx, ref)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(readAll(t, st), data) {
				t.Fatalf("downloaded %d bytes differ", size)
			}
			if st.SHA256() != sum(data) || st.Info().Size != int64(size) || !st.Info().ModTime.Equal(mt) {
				t.Fatalf("stream info %+v sha %s", st.Info(), st.SHA256())
			}
			st.Close()
		}
		fi, err := os.Stat(filepath.Join(root, "ws", "job", "f1.bin"))
		if err != nil {
			t.Fatal(err)
		}
		if !fi.ModTime().Equal(time.Unix(1700000000, 0)) {
			t.Fatalf("mtime not preserved: %v", fi.ModTime())
		}
		if got := leftovers(t, root); len(got) != 0 {
			t.Fatalf("temp files left behind: %v", got)
		}
	})
}

func TestServerRejectsWhatItMust(t *testing.T) {
	bothProtocols(t, func(t *testing.T, s *Store, root string, c *Client) {
		ctx := t.Context()
		data := []byte("hello")
		put := func(ref string, src Source, overwrite bool) error {
			_, err := c.Put(ctx, ref, WriteOptions{Size: int64(len(data)), Overwrite: overwrite}, src)
			return err
		}
		if err := put("artifacts/svc/x.wav", newBytesSource(data), false); !errors.Is(err, ErrReadOnly) {
			t.Fatalf("peer write into artifacts = %v, want ErrReadOnly", err)
		}
		if err := put("ws/job/a.txt", newBytesSource(data), false); err != nil {
			t.Fatal(err)
		}
		if err := put("ws/job/a.txt", newBytesSource(data), false); !errors.Is(err, ErrExists) {
			t.Fatalf("second put = %v, want ErrExists", err)
		}
		if err := put("ws/job/a.txt", newBytesSource(data), true); err != nil {
			t.Fatalf("overwrite put: %v", err)
		}
		c.Mkdir(ctx, "ws/job/dir")
		if err := put("ws/job/dir", newBytesSource(data), true); !errors.Is(err, ErrIsDirectory) {
			t.Fatalf("put over a directory = %v, want ErrIsDirectory", err)
		}
		// A sender that vouches for the wrong digest is refused and nothing stays behind.
		bad := newBytesSource(data)
		bad.digest = sum([]byte("different"))
		if err := put("ws/job/bad.txt", bad, false); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("wrong digest = %v, want ErrIntegrity", err)
		}
		if _, err := os.Stat(filepath.Join(root, "ws", "job", "bad.txt")); err == nil {
			t.Fatal("file with a bad digest was published")
		}
		// Announcing a different size than delivered is refused too.
		if _, err := c.Put(ctx, "ws/job/size.txt", WriteOptions{Size: 99}, newBytesSource(data)); err == nil {
			t.Fatal("size mismatch accepted")
		}
		eventually(t, "the receiver to drop rejected uploads", func() bool { return len(leftovers(t, root)) == 0 })
		if _, err := c.Stat(ctx, "ws/job/missing"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("stat missing = %v, want ErrNotFound", err)
		}
		if _, err := c.Open(ctx, "ws/job/dir"); !errors.Is(err, ErrIsDirectory) {
			t.Fatalf("open directory = %v, want ErrIsDirectory", err)
		}
		for _, p := range []string{"/v1/files/ws/../x", "/v1/files/ws/job/", "/v1/files/..%2fx", "/v1/files/ws/a%5Cb"} {
			resp, err := c.HTTP.Get(c.Base + p)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				t.Errorf("GET %s answered 200", p)
			}
		}
	})
}

func TestGetIsRefusedForLinks(t *testing.T) {
	bothProtocols(t, func(t *testing.T, s *Store, root string, c *Client) {
		outside := filepath.Join(root, "outside")
		os.MkdirAll(outside, 0o700)
		os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0o600)
		os.MkdirAll(filepath.Join(root, "ws", "job"), 0o700)
		mkDirLink(t, filepath.Join(root, "ws", "job", "escape"), outside)
		if _, err := c.Open(t.Context(), "ws/job/escape/secret"); err == nil {
			t.Fatal("download followed a link out of ws")
		}
		if _, err := c.Stat(t.Context(), "ws/job/escape/secret"); err == nil {
			t.Fatal("stat followed a link out of ws")
		}
	})
}

// tamper wraps a client transport to mutate traffic.
type tamper struct {
	rt   http.RoundTripper
	resp func(*http.Request, *http.Response)
	req  func(*http.Request)
}

func (t tamper) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.req != nil {
		t.req(r)
	}
	resp, err := t.rt.RoundTrip(r)
	if err == nil && t.resp != nil {
		t.resp(r, resp)
	}
	return resp, err
}

type cutReader struct {
	io.ReadCloser
	left int
}

func (r *cutReader) Read(p []byte) (int, error) {
	if r.left <= 0 {
		return 0, io.ErrUnexpectedEOF
	}
	if len(p) > r.left {
		p = p[:r.left]
	}
	n, err := r.ReadCloser.Read(p)
	r.left -= n
	return n, err
}

type flipReader struct {
	io.ReadCloser
	at, pos int
}

func (r *flipReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	if r.at >= r.pos && r.at < r.pos+n {
		p[r.at-r.pos] ^= 0xff
	}
	r.pos += n
	return n, err
}

func TestDownloadResumesAfterInterruption(t *testing.T) {
	bothProtocols(t, func(t *testing.T, s *Store, root string, c *Client) {
		data := randomBytes(t, 2<<20)
		if _, err := put(t, s, "ws/job/big.bin", data, false); err != nil {
			t.Fatal(err)
		}
		var gets, ranged atomic.Int32
		c.HTTP = &http.Client{Transport: tamper{rt: c.HTTP.Transport, resp: func(r *http.Request, resp *http.Response) {
			if r.Method != http.MethodGet || !strings.HasPrefix(r.URL.Path, Path+"/") {
				return
			}
			if r.Header.Get("Range") != "" {
				ranged.Add(1)
				return
			}
			if gets.Add(1) == 1 {
				resp.Body = &cutReader{ReadCloser: resp.Body, left: 700_000}
			}
		}}}
		st, err := c.Open(t.Context(), "ws/job/big.bin")
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if got := readAll(t, st); !bytes.Equal(got, data) {
			t.Fatal("resumed download differs from the file")
		}
		if ranged.Load() == 0 {
			t.Fatal("the download did not resume with a Range request")
		}
		if st.SHA256() != sum(data) {
			t.Fatalf("digest after resume = %s", st.SHA256())
		}
	})
}

func TestCorruptionInFlightIsDetected(t *testing.T) {
	bothProtocols(t, func(t *testing.T, s *Store, root string, c *Client) {
		data := randomBytes(t, 1<<20)
		if _, err := put(t, s, "ws/job/a.bin", data, false); err != nil {
			t.Fatal(err)
		}
		rt := c.HTTP.Transport

		// Corrupt the download: the stream must end with an integrity error, not io.EOF.
		c.HTTP = &http.Client{Transport: tamper{rt: rt, resp: func(r *http.Request, resp *http.Response) {
			if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, Path+"/") {
				resp.Body = &flipReader{ReadCloser: resp.Body, at: 12345}
			}
		}}}
		st, err := c.Open(t.Context(), "ws/job/a.bin")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadAll(st); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("corrupted download ended with %v, want ErrIntegrity", err)
		}
		st.Close()

		// Corrupt the upload: the receiver must refuse and publish nothing.
		c.HTTP = &http.Client{Transport: tamper{rt: rt, req: func(r *http.Request) {
			if r.Method == http.MethodPut {
				r.Body = &flipReader{ReadCloser: r.Body, at: 777}
			}
		}}}
		_, err = c.Put(t.Context(), "ws/job/b.bin", WriteOptions{Size: int64(len(data))}, newBytesSource(data))
		if !errors.Is(err, ErrIntegrity) {
			t.Fatalf("corrupted upload = %v, want ErrIntegrity", err)
		}
		if _, err := os.Stat(filepath.Join(root, "ws", "job", "b.bin")); err == nil {
			t.Fatal("corrupted upload was published")
		}
		eventually(t, "the receiver to drop rejected uploads", func() bool { return len(leftovers(t, root)) == 0 })
	})
}

// blockingSource delivers some bytes and then waits for ctx.
type blockingSource struct {
	ctx  context.Context
	sent bool
}

func (b *blockingSource) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, "partial data"), nil
	}
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (b *blockingSource) SHA256() string { return "" }

func TestHTTP2RejectedUploadDoesNotWaitForSourceEOF(t *testing.T) {
	store, root := newTestStore(t)
	mux := http.NewServeMux()
	mux.Handle(Path, NewHandler(store))
	mux.Handle(Path+"/", NewHandler(store))
	server := httptest.NewUnstartedServer(mux)
	server.EnableHTTP2 = true
	server.StartTLS()
	t.Cleanup(server.Close)
	client := &Client{HTTP: server.Client(), Base: server.URL, IdleTimeout: 5 * time.Second}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	sourceCtx, releaseSource := context.WithCancel(t.Context())
	defer releaseSource()

	// The error response must stop the stream even though its source never
	// reaches EOF. Waiting for response EOF would deadlock with the server's
	// request drain until the idle timeout masks ErrReadOnly.
	_, err := client.Put(ctx, "artifacts/svc/blocked.bin", WriteOptions{Size: 1 << 20}, &blockingSource{ctx: sourceCtx})
	releaseSource()
	if !errors.Is(err, ErrReadOnly) {
		t.Fatalf("rejected blocked upload = %v, want ErrReadOnly", err)
	}

	// An upload following the rejection must still preserve its contents.
	data := []byte("after rejection")
	if _, err := client.Put(ctx, "ws/job/next.txt", WriteOptions{Size: int64(len(data))}, newBytesSource(data)); err != nil {
		t.Fatal(err)
	}
	if got := fileText(t, root, "ws/job/next.txt"); got != string(data) {
		t.Fatalf("next upload contents = %q, want %q", got, data)
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func TestCancelledUploadCleansUpAndStalledDownloadGivesUp(t *testing.T) {
	bothProtocols(t, func(t *testing.T, s *Store, root string, c *Client) {
		ctx, cancel := context.WithCancel(t.Context())
		go func() { time.Sleep(200 * time.Millisecond); cancel() }()
		_, err := c.Put(ctx, "ws/job/cancel.bin", WriteOptions{Size: 1 << 20}, &blockingSource{ctx: ctx})
		if err == nil {
			t.Fatal("cancelled upload reported success")
		}
		eventually(t, "the receiver to drop the cancelled upload", func() bool { return len(leftovers(t, root)) == 0 })

		// A download whose bytes stop flowing is abandoned after the idle limit.
		if _, err := put(t, s, "ws/job/stall.bin", randomBytes(t, 1<<16), false); err != nil {
			t.Fatal(err)
		}
		c.IdleTimeout, c.Retries = 300*time.Millisecond, 1
		c.HTTP = &http.Client{Transport: tamper{rt: c.HTTP.Transport, resp: func(r *http.Request, resp *http.Response) {
			if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "stall.bin") {
				resp.Body = &stallReader{ReadCloser: resp.Body, ctx: r.Context()}
			}
		}}}
		start := time.Now()
		st, err := c.Open(t.Context(), "ws/job/stall.bin")
		if err != nil {
			t.Fatal(err)
		}
		defer st.Close()
		if _, err := io.ReadAll(st); !errors.Is(err, errStalled) {
			t.Fatalf("stalled download = %v, want errStalled", err)
		}
		if time.Since(start) > 5*time.Second {
			t.Fatalf("stall detected after %v", time.Since(start))
		}
	})
}

// stallReader delivers nothing until its request is cancelled.
type stallReader struct {
	io.ReadCloser
	ctx context.Context
}

func (r *stallReader) Read([]byte) (int, error) {
	<-r.ctx.Done()
	return 0, r.ctx.Err()
}

func writeTree(t *testing.T, s *Store, files map[string]string) {
	t.Helper()
	for ref, content := range files {
		if _, err := put(t, s, ref, []byte(content), false); err != nil {
			t.Fatal(err)
		}
	}
}

func fileText(t *testing.T, root, ref string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(ref)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCopyDirectoryToRemote(t *testing.T) {
	bothProtocols(t, func(t *testing.T, s *Store, root string, c *Client) {
		srcStore, srcRoot := newTestStore(t)
		writeTree(t, srcStore, map[string]string{
			"ws/proj/a.txt":         "alpha",
			"ws/proj/sub/b.txt":     "beta",
			"ws/proj/sub/deep/c.md": "gamma",
		})
		srcStore.Mkdir("ws/proj/empty/nested")
		local := Local{srcStore}
		ctx := t.Context()

		res, err := Copy(ctx, local, c, "ws/proj", "ws/copy", CopyOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if res.Files != 3 || res.Bytes != 14 || res.To != "ws/copy" || res.Dirs < 4 {
			t.Fatalf("result %+v", res)
		}
		if fileText(t, root, "ws/copy/sub/deep/c.md") != "gamma" || fileText(t, root, "ws/copy/a.txt") != "alpha" {
			t.Fatal("copied content differs")
		}
		if fi, err := os.Stat(filepath.Join(root, "ws", "copy", "empty", "nested")); err != nil || !fi.IsDir() {
			t.Fatalf("empty directory not copied: %v", err)
		}

		// Existing files make the whole copy fail before anything moves.
		os.WriteFile(filepath.Join(srcRoot, "ws", "proj", "new.txt"), []byte("new"), 0o600)
		if _, err := Copy(ctx, local, c, "ws/proj", "ws/copy", CopyOptions{}); !errors.Is(err, ErrExists) && (err == nil || !strings.Contains(err.Error(), "already exist")) {
			t.Fatalf("merge onto existing files = %v", err)
		}
		if _, err := os.Stat(filepath.Join(root, "ws", "copy", "new.txt")); err == nil {
			t.Fatal("a file was copied although the preflight found conflicts")
		}
		res, err = Copy(ctx, local, c, "ws/proj", "ws/copy", CopyOptions{Overwrite: true})
		if err != nil || res.Files != 4 {
			t.Fatalf("overwrite merge = %+v, %v", res, err)
		}

		// A trailing slash puts the source inside the destination.
		res, err = Copy(ctx, local, c, "ws/proj/a.txt", "ws/inbox/", CopyOptions{})
		if err != nil || res.To != "ws/inbox/a.txt" {
			t.Fatalf("copy into dir = %+v, %v", res, err)
		}
		if res.SHA256 != sum([]byte("alpha")) {
			t.Fatalf("single file digest %q", res.SHA256)
		}
		if _, err := Copy(ctx, local, c, "ws/proj/a.txt", "ws/inbox", CopyOptions{}); err == nil || !strings.Contains(err.Error(), "directory") {
			t.Fatalf("file onto a directory without slash = %v", err)
		}
		if _, err := Copy(ctx, local, c, "ws/proj/a.txt", "artifacts/x/a.txt", CopyOptions{}); !errors.Is(err, ErrReadOnly) {
			t.Fatalf("copy into artifacts = %v, want ErrReadOnly", err)
		}
		if _, err := Copy(ctx, local, c, "ws/proj/nothing", "ws/x/y", CopyOptions{}); !errors.Is(err, ErrNotFound) {
			t.Fatalf("copy of a missing source = %v, want ErrNotFound", err)
		}

		// Remote to local, and the file really arrives.
		res, err = Copy(ctx, c, local, "ws/copy/sub", "ws/back/", CopyOptions{})
		if err != nil || res.Files != 2 || res.To != "ws/back/sub" {
			t.Fatalf("remote to local = %+v, %v", res, err)
		}
		if fileText(t, srcRoot, "ws/back/sub/deep/c.md") != "gamma" {
			t.Fatal("remote to local content differs")
		}
		eventually(t, "the receiver to drop rejected uploads", func() bool { return len(leftovers(t, root)) == 0 })
	})
}

func TestCopyOntoItselfAndLimits(t *testing.T) {
	s, _ := newTestStore(t)
	writeTree(t, s, map[string]string{"ws/a/f.txt": "x", "ws/a/g.txt": "y"})
	l := Local{s}
	ctx := t.Context()
	if _, err := Copy(ctx, l, l, "ws/a/f.txt", "ws/a/f.txt", CopyOptions{SameDevice: true, Overwrite: true}); err == nil {
		t.Fatal("file copied onto itself")
	}
	if _, err := Copy(ctx, l, l, "ws/a", "ws/a/inner", CopyOptions{SameDevice: true}); err == nil {
		t.Fatal("directory copied into itself")
	}
	if _, err := Copy(ctx, l, l, "ws/a", "ws/b", CopyOptions{SameDevice: true, MaxFiles: 1}); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("file count limit = %v", err)
	}
	if _, err := Copy(ctx, l, l, "ws/a", "ws/b", CopyOptions{SameDevice: true, MaxBytes: 1}); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("size limit = %v", err)
	}
	if res, err := Copy(ctx, l, l, "ws/a", "ws/b", CopyOptions{SameDevice: true}); err != nil || res.Files != 2 {
		t.Fatalf("local copy = %+v, %v", res, err)
	}
	// Deep destinations that no peer would accept are refused before moving anything.
	deep := "ws/z/" + strings.Repeat("d/", MaxWriteDepth)
	if _, err := Copy(ctx, l, l, "ws/a/f.txt", deep+"f.txt", CopyOptions{SameDevice: true}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("too deep destination = %v, want ErrInvalid", err)
	}
}

func TestCopyStopsOnCancel(t *testing.T) {
	s, root := newTestStore(t)
	writeTree(t, s, map[string]string{"ws/a/f.txt": "x"})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Copy(ctx, Local{s}, Local{s}, "ws/a/f.txt", "ws/b/f.txt", CopyOptions{SameDevice: true}); err == nil {
		t.Fatal("cancelled copy reported success")
	}
	if _, err := os.Stat(filepath.Join(root, "ws", "b", "f.txt")); err == nil {
		t.Fatal("cancelled copy produced a file")
	}
	if got := leftovers(t, root); len(got) != 0 {
		t.Fatalf("temp files left behind: %v", got)
	}
}
