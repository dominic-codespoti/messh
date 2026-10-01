package files

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"messh/internal/state"
)

// Limits on what peers and agents may create. Reads are not limited by
// depth: jobs may produce deep trees and those must stay copyable out.
const (
	MaxWriteRefLen   = 200    // total characters of a ref being created
	MaxWriteDepth    = 16     // segments, including the root
	MaxListEntries   = 5000   // entries in a single directory listing
	MaxTreeEntries   = 100000 // entries in a recursive walk
	DefaultReserve   = 2 << 30
	DefaultReservePc = 5

	tempPrefix = ".messh-"
	tempSuffix = ".part"
)

// Sentinel errors. The HTTP layer carries them across the mesh so callers can
// test with errors.Is on either side.
var (
	ErrNotFound     = fs.ErrNotExist
	ErrExists       = errors.New("already exists")
	ErrIsDirectory  = errors.New("is a directory")
	ErrNotDirectory = errors.New("is not a directory")
	ErrReadOnly     = errors.New("read-only location: files can only be written under ws/")
	ErrRoot         = errors.New("is a root and cannot be changed")
	ErrNotRegular   = errors.New("is not a regular file or directory")
	ErrIntegrity    = errors.New("integrity check failed")
	ErrNoSpace      = errors.New("not enough free disk space")
	ErrInvalid      = errors.New("invalid request")
)

// Info describes one file or directory.
type Info struct {
	Ref     string    `json:"ref"`
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
	IsDir   bool      `json:"is_dir"`
	Exec    bool      `json:"exec,omitempty"`
	SHA256  string    `json:"sha256,omitempty"`
}

// Listing is a directory listing, or the stat of a single entry.
type Listing struct {
	Info      Info   `json:"info"`
	Entries   []Info `json:"entries,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
	// Skipped counts entries left out because their names cannot be
	// expressed as a ref or because they are not regular files/directories
	// (symlinks, devices).
	Skipped int `json:"skipped,omitempty"`
}

// Store performs sandboxed file operations on the two shared roots.
type Store struct {
	paths state.Paths

	// FreeSpace reports free and total bytes of the filesystem holding dir.
	// Tests replace it.
	FreeSpace func(dir string) (free, total uint64, err error)
	// A write must leave min(ReserveBytes, ReservePercent% of the disk) free.
	ReserveBytes   uint64
	ReservePercent uint64
}

// NewStore creates the roots if needed.
func NewStore(p state.Paths) (*Store, error) {
	for _, root := range []string{RootWorkspaces, RootArtifacts} {
		if err := os.MkdirAll(filepath.Join(p.Root, root), 0o700); err != nil {
			return nil, err
		}
	}
	return &Store{paths: p, FreeSpace: diskSpace, ReserveBytes: DefaultReserve, ReservePercent: DefaultReservePc}, nil
}

var windowsReserved = regexp.MustCompile(`(?i)^(con|prn|aux|nul|com[1-9]|lpt[1-9])$`)

// parse validates s as a ref and rejects names this store never serves:
// its own temp files and names Windows treats as devices.
func (s *Store) parse(raw string) (Ref, error) {
	ref, err := ParseRef(raw)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	for _, seg := range strings.Split(string(ref), "/") {
		stem, _, _ := strings.Cut(seg, ".")
		if strings.HasPrefix(seg, tempPrefix) || windowsReserved.MatchString(strings.TrimRight(stem, " ")) {
			return "", fmt.Errorf("%w: %q is a reserved name", ErrInvalid, seg)
		}
	}
	return ref, nil
}

func (s *Store) resolve(raw string) (Ref, string, error) {
	ref, err := s.parse(raw)
	if err != nil {
		return "", "", err
	}
	abs, err := s.checkPath(ref)
	if err != nil {
		return "", "", err
	}
	return ref, abs, nil
}

// checkPath maps ref to an absolute path; Resolve already refuses links,
// junctions and special files anywhere below the root.
func (s *Store) checkPath(ref Ref) (string, error) {
	abs, err := Resolve(s.paths, ref)
	if errors.Is(err, ErrNotRegular) {
		return "", err
	}
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	return abs, nil
}

// ioErr rewrites OS errors so they never carry absolute paths of this device.
func ioErr(ref Ref, err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s: %w", ref, ErrNotFound)
	}
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return fmt.Errorf("%s: %s: %w", ref, pe.Op, pe.Err)
	}
	return fmt.Errorf("%s: %w", ref, err)
}

// plain reports whether m is a regular file or a real directory; symlinks,
// junctions, devices and the like are never served.
func plain(m fs.FileMode) bool { return m&fs.ModeType&^fs.ModeDir == 0 }

func infoOf(ref Ref, fi fs.FileInfo) Info {
	in := Info{Ref: string(ref), Name: path.Base(string(ref)), ModTime: fi.ModTime().UTC(), IsDir: fi.IsDir()}
	if !in.IsDir {
		in.Size = fi.Size()
		in.Exec = fi.Mode()&0o111 != 0
	}
	return in
}

func rootInfo(name string) Info {
	return Info{Ref: name, Name: name, IsDir: true}
}

// Stat describes ref; the empty ref is the virtual directory holding the two
// roots. With hash, the SHA-256 of a regular file is computed.
func (s *Store) Stat(raw string, withHash bool) (Info, error) {
	if raw == "" {
		return Info{IsDir: true}, nil
	}
	ref, abs, err := s.resolve(raw)
	if err != nil {
		return Info{}, err
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return Info{}, ioErr(ref, err)
	}
	if !plain(fi.Mode()) {
		return Info{}, fmt.Errorf("%s %w", ref, ErrNotRegular)
	}
	in := infoOf(ref, fi)
	if withHash && !in.IsDir {
		f, fi, err := openChecked(ref, abs)
		if err != nil {
			return Info{}, err
		}
		defer f.Close()
		h := sha256.New()
		if _, err := io.Copy(h, io.LimitReader(f, fi.Size())); err != nil {
			return Info{}, ioErr(ref, err)
		}
		in = infoOf(ref, fi)
		in.SHA256 = hex.EncodeToString(h.Sum(nil))
	}
	return in, nil
}

// openChecked opens abs for reading after confirming it is a regular file
// and still the same file that was inspected (a job could swap in a symlink
// between the check and the open).
func openChecked(ref Ref, abs string) (*os.File, fs.FileInfo, error) {
	before, err := os.Lstat(abs)
	if err != nil {
		return nil, nil, ioErr(ref, err)
	}
	if before.IsDir() {
		return nil, nil, fmt.Errorf("%s: %w", ref, ErrIsDirectory)
	}
	if !before.Mode().IsRegular() {
		return nil, nil, fmt.Errorf("%s %w", ref, ErrNotRegular)
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, nil, ioErr(ref, err)
	}
	after, err := f.Stat()
	if err != nil || !os.SameFile(before, after) || !after.Mode().IsRegular() {
		f.Close()
		return nil, nil, fmt.Errorf("%s %w", ref, ErrNotRegular)
	}
	return f, after, nil
}

// OpenFile opens a regular file for reading.
func (s *Store) OpenFile(raw string) (*os.File, Info, error) {
	ref, abs, err := s.resolve(raw)
	if err != nil {
		return nil, Info{}, err
	}
	f, fi, err := openChecked(ref, abs)
	if err != nil {
		return nil, Info{}, err
	}
	return f, infoOf(ref, fi), nil
}

// List lists one directory (the empty ref lists the roots). A regular file
// lists as itself.
func (s *Store) List(raw string) (Listing, error) {
	if raw == "" {
		return Listing{Entries: []Info{rootInfo(RootWorkspaces), rootInfo(RootArtifacts)}, Info: Info{IsDir: true}}, nil
	}
	ref, abs, err := s.resolve(raw)
	if err != nil {
		return Listing{}, err
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return Listing{}, ioErr(ref, err)
	}
	if !plain(fi.Mode()) {
		return Listing{}, fmt.Errorf("%s %w", ref, ErrNotRegular)
	}
	l := Listing{Info: infoOf(ref, fi)}
	if !fi.IsDir() {
		l.Entries = []Info{l.Info}
		return l, nil
	}
	des, err := os.ReadDir(abs)
	if err != nil {
		return Listing{}, ioErr(ref, err)
	}
	for _, de := range des {
		child, ok := s.childRef(ref, de)
		if !ok {
			if !strings.HasPrefix(de.Name(), tempPrefix) {
				l.Skipped++
			}
			continue
		}
		if len(l.Entries) >= MaxListEntries {
			l.Truncated = true
			break
		}
		fi, err := de.Info()
		if err != nil {
			l.Skipped++
			continue
		}
		l.Entries = append(l.Entries, infoOf(child, fi))
	}
	return l, nil
}

// childRef returns the ref of a directory entry if it can be served.
func (s *Store) childRef(parent Ref, de fs.DirEntry) (Ref, bool) {
	if !plain(de.Type()) {
		return "", false
	}
	c, err := s.parse(string(parent) + "/" + de.Name())
	if err != nil {
		return "", false
	}
	return c, true
}

// Walk lists everything below a directory, parents before children, up to
// MaxTreeEntries. Info is the directory itself.
func (s *Store) Walk(raw string) (Listing, error) {
	if raw == "" {
		return Listing{}, fmt.Errorf("%w: a ref is required", ErrInvalid)
	}
	ref, abs, err := s.resolve(raw)
	if err != nil {
		return Listing{}, err
	}
	fi, err := os.Lstat(abs)
	if err != nil {
		return Listing{}, ioErr(ref, err)
	}
	if !plain(fi.Mode()) {
		return Listing{}, fmt.Errorf("%s %w", ref, ErrNotRegular)
	}
	l := Listing{Info: infoOf(ref, fi)}
	if !fi.IsDir() {
		return l, nil
	}
	refs := map[string]Ref{abs: ref}
	err = filepath.WalkDir(abs, func(p string, de fs.DirEntry, err error) error {
		if p == abs {
			return err
		}
		if err != nil {
			l.Skipped++
			return nil
		}
		parent := refs[filepath.Dir(p)]
		child, ok := s.childRef(parent, de)
		if !ok {
			if !strings.HasPrefix(de.Name(), tempPrefix) {
				l.Skipped++
			}
			if de.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if len(l.Entries) >= MaxTreeEntries {
			l.Truncated = true
			return filepath.SkipAll
		}
		fi, err := de.Info()
		if err != nil {
			l.Skipped++
			return nil
		}
		if de.IsDir() {
			refs[p] = child
		}
		l.Entries = append(l.Entries, infoOf(child, fi))
		return nil
	})
	if err != nil {
		return Listing{}, ioErr(ref, err)
	}
	return l, nil
}

// ValidateWriteRef checks that raw names a place that may be created: under
// ws/, deep enough not to be a root or a bare workspace name where a file is
// meant, and within the length and depth limits. It does not touch the disk.
func ValidateWriteRef(raw string, file bool) error {
	ref, err := ParseRef(raw)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if ref.Root() != RootWorkspaces {
		return fmt.Errorf("%s: %w", ref, ErrReadOnly)
	}
	depth := strings.Count(string(ref), "/") + 1
	switch {
	case depth == 1:
		return fmt.Errorf("%s: %w", ref, ErrRoot)
	case file && depth < 3:
		return fmt.Errorf("%w: %s must be inside a workspace directory, e.g. ws/<workspace>/<file>", ErrInvalid, ref)
	case len(ref) > MaxWriteRefLen || depth > MaxWriteDepth:
		return fmt.Errorf("%w: %s is too long or deep (max %d characters, %d levels)", ErrInvalid, ref, MaxWriteRefLen, MaxWriteDepth)
	}
	return nil
}

func (s *Store) writable(raw string, file bool) (Ref, string, error) {
	ref, abs, err := s.resolve(raw)
	if err != nil {
		return "", "", err
	}
	if err := ValidateWriteRef(string(ref), file); err != nil {
		return "", "", err
	}
	return ref, abs, nil
}

// Mkdir creates a directory and its parents under ws/. An existing
// directory is not an error.
func (s *Store) Mkdir(raw string) error {
	ref, abs, err := s.writable(raw, false)
	if err != nil {
		return err
	}
	if fi, err := os.Lstat(abs); err == nil {
		if fi.IsDir() && plain(fi.Mode()) {
			return nil
		}
		if !fi.IsDir() {
			return fmt.Errorf("%s: %w", ref, ErrExists)
		}
		return fmt.Errorf("%s %w", ref, ErrNotRegular)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return ioErr(ref, err)
	}
	return nil
}

// Delete removes a file or directory tree. The roots cannot be deleted.
// Symlinks and junctions found inside a tree are removed, never followed.
func (s *Store) Delete(raw string) error {
	ref, err := s.parse(raw)
	if err != nil {
		return err
	}
	if !strings.Contains(string(ref), "/") {
		return fmt.Errorf("%s: %w", ref, ErrRoot)
	}
	// Resolve the parent, not the entry: a symlink entry pointing outside
	// the roots must still be removable (only the link goes).
	parent, err := s.checkPath(Ref(path.Dir(string(ref))))
	if err != nil {
		return err
	}
	abs := filepath.Join(parent, path.Base(string(ref)))
	if _, err := os.Lstat(abs); err != nil {
		return ioErr(ref, err)
	}
	if err := removeTree(abs); err != nil {
		return ioErr(ref, err)
	}
	return nil
}

func removeTree(p string) error {
	fi, err := os.Lstat(p)
	if err != nil {
		return err
	}
	if fi.IsDir() && plain(fi.Mode()) {
		des, err := os.ReadDir(p)
		if err != nil {
			return err
		}
		for _, de := range des {
			if err := removeTree(filepath.Join(p, de.Name())); err != nil {
				return err
			}
		}
	}
	return os.Remove(p)
}

// WriteOptions describe an incoming file.
type WriteOptions struct {
	Size      int64 // exact number of bytes the sender will deliver
	Overwrite bool
	Exec      bool
	ModTime   time.Time // zero keeps the time of writing
}

// Upload is a file being received. Bytes go to a temporary file beside the
// destination; Commit verifies them and moves the file into place.
type Upload struct {
	s    *Store
	ref  Ref
	dst  string
	opt  WriteOptions
	tmp  *os.File
	hash hash.Hash
	n    int64
	done bool
}

// BeginWrite validates the destination, checks free space, and opens the
// temporary file. The caller must call Commit or Abort.
func (s *Store) BeginWrite(raw string, opt WriteOptions) (*Upload, error) {
	ref, dst, err := s.writable(raw, true)
	if err != nil {
		return nil, err
	}
	if opt.Size < 0 {
		return nil, fmt.Errorf("%w: file size is required", ErrInvalid)
	}
	if err := checkTarget(ref, dst, opt.Overwrite); err != nil {
		return nil, err
	}
	dir := filepath.Dir(dst)
	if err := s.checkSpace(ref, nearestExisting(dir), uint64(opt.Size)); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, ioErr(ref, err)
	}
	// MkdirAll created plain directories only, but recheck: the path may
	// have changed under us.
	if _, err := s.checkPath(Ref(path.Dir(string(ref)))); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(dir, tempPrefix+"*"+tempSuffix)
	if err != nil {
		return nil, ioErr(ref, err)
	}
	return &Upload{s: s, ref: ref, dst: dst, opt: opt, tmp: tmp, hash: sha256.New()}, nil
}

func checkTarget(ref Ref, dst string, overwrite bool) error {
	fi, err := os.Lstat(dst)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return ioErr(ref, err)
	case fi.IsDir():
		return fmt.Errorf("%s: %w", ref, ErrIsDirectory)
	case !fi.Mode().IsRegular():
		return fmt.Errorf("%s %w", ref, ErrNotRegular)
	case !overwrite:
		return fmt.Errorf("%s: %w (pass overwrite to replace it)", ref, ErrExists)
	}
	return nil
}

func nearestExisting(dir string) string {
	for {
		if _, err := os.Stat(dir); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return dir
		}
		dir = parent
	}
}

func (s *Store) checkSpace(ref Ref, dir string, size uint64) error {
	probe := s.FreeSpace
	if probe == nil {
		probe = diskSpace
	}
	free, total, err := probe(dir)
	if err != nil {
		return fmt.Errorf("%s: cannot determine free disk space: %w", ref, err)
	}
	reserve := s.ReserveBytes
	if s.ReservePercent > 0 && total/100*s.ReservePercent < reserve {
		reserve = total / 100 * s.ReservePercent
	}
	if free < size || free-size < reserve {
		return fmt.Errorf("%s: %w: need %s plus a %s reserve, %s free", ref, ErrNoSpace, humanBytes(size), humanBytes(reserve), humanBytes(free))
	}
	return nil
}

func humanBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Write receives bytes, refusing more than the announced size.
func (u *Upload) Write(p []byte) (int, error) {
	if u.done {
		return 0, errors.New("upload finished")
	}
	if u.n+int64(len(p)) > u.opt.Size {
		return 0, fmt.Errorf("%s: %w: more data than the announced %d bytes", u.ref, ErrIntegrity, u.opt.Size)
	}
	n, err := u.tmp.Write(p)
	u.hash.Write(p[:n])
	u.n += int64(n)
	return n, err
}

// Abort discards the temporary file. It is safe to call after Commit.
func (u *Upload) Abort() {
	if u.done {
		return
	}
	u.done = true
	u.tmp.Close()
	os.Remove(u.tmp.Name())
}

// Commit verifies size and (when expect is not empty) SHA-256, then moves
// the file into place and returns its Info with the SHA256 filled in. On any
// failure the temporary file is removed.
func (u *Upload) Commit(expect string) (Info, error) {
	if u.done {
		return Info{}, errors.New("upload finished")
	}
	defer u.Abort()
	if u.n != u.opt.Size {
		return Info{}, fmt.Errorf("%s: %w: received %d of %d bytes", u.ref, ErrIntegrity, u.n, u.opt.Size)
	}
	sum := hex.EncodeToString(u.hash.Sum(nil))
	if expect != "" && !strings.EqualFold(expect, sum) {
		return Info{}, fmt.Errorf("%s: %w: SHA-256 of the received bytes is %s, the sender announced %s", u.ref, ErrIntegrity, sum, expect)
	}
	if err := u.tmp.Sync(); err != nil {
		return Info{}, ioErr(u.ref, err)
	}
	if err := u.tmp.Close(); err != nil {
		return Info{}, ioErr(u.ref, err)
	}
	name := u.tmp.Name()
	mode := os.FileMode(0o600)
	if u.opt.Exec {
		mode = 0o700
	}
	if err := os.Chmod(name, mode); err != nil {
		return Info{}, ioErr(u.ref, err)
	}
	mt := u.opt.ModTime
	if mt.IsZero() || mt.After(time.Now().Add(24*time.Hour)) {
		mt = time.Now()
	}
	if err := os.Chtimes(name, mt, mt); err != nil {
		return Info{}, ioErr(u.ref, err)
	}
	if err := checkTarget(u.ref, u.dst, u.opt.Overwrite); err != nil {
		return Info{}, err
	}
	if err := publish(name, u.dst, u.opt.Overwrite); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return Info{}, fmt.Errorf("%s: %w (pass overwrite to replace it)", u.ref, ErrExists)
		}
		return Info{}, ioErr(u.ref, err)
	}
	fi, err := os.Lstat(u.dst)
	if err != nil {
		return Info{}, ioErr(u.ref, err)
	}
	in := infoOf(u.ref, fi)
	in.SHA256 = sum
	return in, nil
}

// publish moves tmp to dst. Without overwrite a hard link is used so a file
// that appeared since the check is never replaced; filesystems without hard
// links fall back to the (slightly racy) check done by the caller.
func publish(tmp, dst string, overwrite bool) error {
	if overwrite {
		return os.Rename(tmp, dst)
	}
	err := os.Link(tmp, dst)
	if err == nil {
		os.Remove(tmp)
		return nil
	}
	if errors.Is(err, fs.ErrExist) {
		return err
	}
	return os.Rename(tmp, dst)
}

// Sweep removes temporary files abandoned by a crash. Files younger than
// maxAge may belong to a transfer in progress and are left alone.
func (s *Store) Sweep(ctx context.Context, maxAge time.Duration) {
	cutoff := time.Now().Add(-maxAge)
	for _, root := range []string{RootWorkspaces, RootArtifacts} {
		base := filepath.Join(s.paths.Root, root)
		filepath.WalkDir(base, func(p string, de fs.DirEntry, err error) error {
			if ctx.Err() != nil {
				return filepath.SkipAll
			}
			if err != nil || de.IsDir() {
				return nil
			}
			name := de.Name()
			if strings.HasPrefix(name, tempPrefix) && strings.HasSuffix(name, tempSuffix) {
				if fi, err := de.Info(); err == nil && fi.ModTime().Before(cutoff) {
					os.Remove(p)
				}
			}
			return nil
		})
	}
}
