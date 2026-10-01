package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Limits on a single copy call, as a sanity check against pointing an agent
// at the wrong directory.
const (
	MaxCopyFiles = 50000
	MaxCopyBytes = 512 << 30

	defaultCopyWorkers = 4
	maxConflictsShown  = 5
)

// Source is a stream of file content that can vouch for its own SHA-256.
type Source interface {
	io.Reader
	// SHA256 returns the hex digest of the whole content. It is only valid
	// after Read has returned io.EOF, and is empty if the source cannot
	// vouch for the content.
	SHA256() string
}

// Stream is a Source read from an Endpoint.
type Stream interface {
	Source
	io.Closer
	Info() Info
}

// Endpoint is one device's file space, local or reached over the mesh. All
// refs are unqualified ("ws/job/out.png").
type Endpoint interface {
	Stat(ctx context.Context, ref string) (Info, error)
	// Walk lists everything below a directory, parents first.
	Walk(ctx context.Context, ref string) (Listing, error)
	Mkdir(ctx context.Context, ref string) error
	Open(ctx context.Context, ref string) (Stream, error)
	// Put writes src to ref atomically, verifying size and, when src vouches
	// for one, its SHA-256. The returned Info carries the digest the
	// receiver computed.
	Put(ctx context.Context, ref string, opt WriteOptions, src Source) (Info, error)
}

// Local is the Endpoint backed by this device's Store.
type Local struct{ Store *Store }

func (l Local) Stat(_ context.Context, ref string) (Info, error) { return l.Store.Stat(ref, false) }
func (l Local) Walk(_ context.Context, ref string) (Listing, error) {
	return l.Store.Walk(ref)
}
func (l Local) Mkdir(_ context.Context, ref string) error { return l.Store.Mkdir(ref) }

func (l Local) Open(_ context.Context, ref string) (Stream, error) {
	f, info, err := l.Store.OpenFile(ref)
	if err != nil {
		return nil, err
	}
	return &localStream{f: f, info: info, h: sha256.New()}, nil
}

func (l Local) Put(ctx context.Context, ref string, opt WriteOptions, src Source) (Info, error) {
	u, err := l.Store.BeginWrite(ref, opt)
	if err != nil {
		return Info{}, err
	}
	if _, err := io.Copy(u, ctxReader{ctx, src}); err != nil {
		u.Abort()
		return Info{}, err
	}
	return u.Commit(src.SHA256())
}

// localStream reads a local file, hashing as it goes and never delivering
// more or fewer bytes than the size it announced.
type localStream struct {
	f    *os.File
	info Info
	h    hash.Hash
	n    int64
	sum  string
}

func (s *localStream) Info() Info { return s.info }

func (s *localStream) Read(p []byte) (int, error) {
	remain := s.info.Size - s.n
	if remain == 0 {
		s.sum = hex.EncodeToString(s.h.Sum(nil))
		return 0, io.EOF
	}
	if int64(len(p)) > remain {
		p = p[:remain]
	}
	n, err := s.f.Read(p)
	s.h.Write(p[:n])
	s.n += int64(n)
	if err == io.EOF {
		if s.n < s.info.Size {
			return n, fmt.Errorf("%s: %w: the file shrank while it was being read", s.info.Ref, ErrIntegrity)
		}
		err = nil
	}
	return n, err
}

func (s *localStream) SHA256() string { return s.sum }
func (s *localStream) Close() error   { return s.f.Close() }

// ctxReader makes a copy loop notice cancellation between reads.
type ctxReader struct {
	ctx context.Context
	Source
}

func (r ctxReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, context.Cause(r.ctx)
	}
	return r.Source.Read(p)
}

// CopyOptions tune Copy.
type CopyOptions struct {
	Overwrite bool
	// SameDevice is set when src and dst are the same file space, so copying
	// a file onto itself or a directory into itself can be refused.
	SameDevice  bool
	Concurrency int   // files copied at once; default 4
	MaxFiles    int   // default MaxCopyFiles
	MaxBytes    int64 // default MaxCopyBytes
}

// CopyResult summarises a finished (or partly finished) copy.
type CopyResult struct {
	To       string // the ref written (differs from the requested one when it ended with "/")
	Files    int
	Dirs     int
	Bytes    int64
	Skipped  int // source entries left out: symlinks, devices, unsupported names
	Duration time.Duration
	SHA256   string // digest of the file, for single-file copies
}

type copyJob struct{ from, to string }

// Copy copies from (a file or directory on src) to to (on dst). If to ends
// with "/" the source is placed inside it under its own name; otherwise to is
// the exact destination. Directories merge into an existing one. Without
// Overwrite, any destination file that already exists is an error before a
// single byte moves. Every file is hashed end to end and written atomically,
// so a failure leaves complete files or nothing, never partial ones.
func Copy(ctx context.Context, src, dst Endpoint, from, to string, opt CopyOptions) (CopyResult, error) {
	start := time.Now()
	res, err := doCopy(ctx, src, dst, from, to, opt)
	res.Duration = time.Since(start)
	return res, err
}

func doCopy(ctx context.Context, src, dst Endpoint, from, to string, opt CopyOptions) (CopyResult, error) {
	var res CopyResult
	if opt.MaxFiles <= 0 {
		opt.MaxFiles = MaxCopyFiles
	}
	if opt.MaxBytes <= 0 {
		opt.MaxBytes = MaxCopyBytes
	}
	if opt.Concurrency <= 0 {
		opt.Concurrency = defaultCopyWorkers
	}
	into := strings.HasSuffix(to, "/")
	fromRef, err := ParseRef(from)
	if err != nil {
		return res, fmt.Errorf("source: %w", err)
	}
	toRef, err := ParseRef(to)
	if err != nil {
		return res, fmt.Errorf("destination: %w", err)
	}
	if toRef.Root() != RootWorkspaces {
		return res, fmt.Errorf("destination %s: %w", toRef, ErrReadOnly)
	}
	if into {
		if toRef, err = toRef.Join(path.Base(string(fromRef))); err != nil {
			return res, fmt.Errorf("destination: %w", err)
		}
	}
	res.To = string(toRef)

	si, err := src.Stat(ctx, string(fromRef))
	if err != nil {
		return res, fmt.Errorf("source: %w", err)
	}
	if opt.SameDevice && (toRef == fromRef || (si.IsDir && strings.HasPrefix(string(toRef)+"/", string(fromRef)+"/"))) {
		return res, fmt.Errorf("cannot copy %s onto or into itself", fromRef)
	}
	if err := ValidateWriteRef(string(toRef), !si.IsDir); err != nil {
		return res, fmt.Errorf("destination: %w", err)
	}
	di, derr := dst.Stat(ctx, string(toRef))
	exists := derr == nil
	if derr != nil && !errors.Is(derr, ErrNotFound) {
		return res, fmt.Errorf("destination: %w", derr)
	}

	var jobs []copyJob
	var emptyDirs []string
	if !si.IsDir {
		switch {
		case exists && di.IsDir:
			return res, fmt.Errorf("destination %s is a directory: end it with / to copy the file into it", toRef)
		case exists && !opt.Overwrite:
			return res, fmt.Errorf("destination %s: %w (pass overwrite=true to replace it)", toRef, ErrExists)
		}
		jobs = []copyJob{{string(fromRef), string(toRef)}}
	} else {
		if exists && !di.IsDir {
			return res, fmt.Errorf("destination %s exists and is a file, not a directory", toRef)
		}
		tree, err := src.Walk(ctx, string(fromRef))
		if err != nil {
			return res, fmt.Errorf("source: %w", err)
		}
		if tree.Truncated || len(tree.Entries) > opt.MaxFiles {
			return res, fmt.Errorf("source %s has more than %d entries; copy subdirectories separately", fromRef, opt.MaxFiles)
		}
		res.Skipped = tree.Skipped
		var total int64
		for _, e := range tree.Entries {
			total += e.Size
		}
		if total > opt.MaxBytes {
			return res, fmt.Errorf("source %s holds %s, more than the %s limit of one copy; copy parts separately", fromRef, humanBytes(uint64(total)), humanBytes(uint64(opt.MaxBytes)))
		}
		existing := map[string]Info{}
		if exists {
			dt, err := dst.Walk(ctx, string(toRef))
			if err != nil {
				return res, fmt.Errorf("destination: %w", err)
			}
			for _, e := range dt.Entries {
				existing[strings.TrimPrefix(e.Ref, string(toRef)+"/")] = e
			}
		}
		var conflicts []string
		for i, e := range tree.Entries {
			rel := strings.TrimPrefix(e.Ref, string(fromRef)+"/")
			target := string(toRef) + "/" + rel
			if err := ValidateWriteRef(target, !e.IsDir); err != nil {
				return res, fmt.Errorf("destination: %w", err)
			}
			if old, ok := existing[rel]; ok && (old.IsDir != e.IsDir || (!e.IsDir && !opt.Overwrite)) {
				conflicts = append(conflicts, rel)
				continue
			}
			if e.IsDir {
				res.Dirs++
				// Directories with contents appear when their files are
				// written; only empty ones need creating.
				if i+1 == len(tree.Entries) || !strings.HasPrefix(tree.Entries[i+1].Ref, e.Ref+"/") {
					emptyDirs = append(emptyDirs, target)
				}
				continue
			}
			jobs = append(jobs, copyJob{e.Ref, target})
		}
		if len(conflicts) > 0 {
			list := strings.Join(conflicts[:min(len(conflicts), maxConflictsShown)], ", ")
			if len(conflicts) > maxConflictsShown {
				list += ", ..."
			}
			return res, fmt.Errorf("%d destination entries already exist or clash in type (%s) under %s; pass overwrite=true to replace files",
				len(conflicts), list, toRef)
		}
		res.Dirs++ // the destination directory itself
		if err := dst.Mkdir(ctx, string(toRef)); err != nil {
			return res, fmt.Errorf("destination: %w", err)
		}
		for _, d := range emptyDirs {
			if err := dst.Mkdir(ctx, d); err != nil {
				return res, fmt.Errorf("destination: %w", err)
			}
		}
	}

	files, bytes, sum, err := runJobs(ctx, src, dst, jobs, opt)
	res.Files, res.Bytes = files, bytes
	if len(jobs) == 1 && !si.IsDir {
		res.SHA256 = sum
	}
	if err != nil {
		return res, fmt.Errorf("copy stopped after %d of %d files (%s): %w", files, len(jobs), humanBytes(uint64(bytes)), err)
	}
	return res, nil
}

// runJobs copies every job with a bounded worker pool and stops at the
// first failure; files that finished stay (each is atomic).
func runJobs(ctx context.Context, src, dst Endpoint, jobs []copyJob, opt CopyOptions) (files int, bytes int64, sum string, err error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	var (
		doneFiles, doneBytes atomic.Int64
		mu                   sync.Mutex
		firstErr             error
		lastSum              string
		wg                   sync.WaitGroup
	)
	queue := make(chan copyJob)
	for range min(opt.Concurrency, len(jobs)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range queue {
				n, s, err := copyFile(ctx, src, dst, j, opt.Overwrite)
				mu.Lock()
				if err != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("%s: %w", j.from, err)
						cancel(firstErr)
					}
				} else {
					doneFiles.Add(1)
					doneBytes.Add(n)
					lastSum = s
				}
				mu.Unlock()
			}
		}()
	}
feed:
	for _, j := range jobs {
		select {
		case queue <- j:
		case <-ctx.Done():
			break feed
		}
	}
	close(queue)
	wg.Wait()
	if firstErr == nil && ctx.Err() != nil {
		firstErr = context.Cause(ctx)
	}
	return int(doneFiles.Load()), doneBytes.Load(), lastSum, firstErr
}

func copyFile(ctx context.Context, src, dst Endpoint, j copyJob, overwrite bool) (int64, string, error) {
	st, err := src.Open(ctx, j.from)
	if err != nil {
		return 0, "", err
	}
	defer st.Close()
	info := st.Info()
	got, err := dst.Put(ctx, j.to, WriteOptions{Size: info.Size, Overwrite: overwrite, Exec: info.Exec, ModTime: info.ModTime}, st)
	if err != nil {
		return 0, "", err
	}
	if s := st.SHA256(); s != "" && got.SHA256 != "" && s != got.SHA256 {
		return 0, "", fmt.Errorf("%w: source digest %s, destination digest %s", ErrIntegrity, s, got.SHA256)
	}
	return info.Size, got.SHA256, nil
}
