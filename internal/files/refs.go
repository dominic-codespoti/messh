// Package files names and resolves the directories messh shares between
// devices. Only two roots exist, both inside the state directory:
//
//	ws/<workspace>/...        working files for jobs (copied in, produced by jobs)
//	artifacts/<source>/...    outputs messh captured, e.g. audio from a service call
//
// A Ref is a slash-separated path starting with one of those roots. A
// qualified ref prefixes the device: "desktop:artifacts/voicestudio/9f2c.wav".
package files

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"messh/internal/state"
)

const (
	RootWorkspaces = "ws"
	RootArtifacts  = "artifacts"
)

// Ref is a validated, clean path inside a shared root.
type Ref string

var segmentRE = regexp.MustCompile(`^[A-Za-z0-9._ ()\[\]@+=,-]+$`)

// ParseRef validates s and returns its canonical form. It rejects absolute
// paths, "..", empty segments after cleaning, unknown roots, and characters
// that are unsafe on Windows or Linux.
func ParseRef(s string) (Ref, error) {
	if s == "" {
		return "", errors.New("empty file reference")
	}
	if strings.ContainsAny(s, "\\:\x00") {
		return "", fmt.Errorf("file reference %q: use forward slashes and no ':' (qualified refs are DEVICE:root/path)", s)
	}
	if strings.HasPrefix(s, "/") {
		return "", fmt.Errorf("file reference %q must be relative, e.g. ws/<workspace>/file", s)
	}
	clean := path.Clean(s)
	parts := strings.Split(clean, "/")
	for _, p := range parts {
		if p == ".." || p == "." || !segmentRE.MatchString(p) || strings.HasSuffix(p, ".") || strings.HasSuffix(p, " ") {
			return "", fmt.Errorf("file reference %q has an invalid segment %q", s, p)
		}
	}
	if parts[0] != RootWorkspaces && parts[0] != RootArtifacts {
		return "", fmt.Errorf("file reference %q must start with %s/ or %s/", s, RootWorkspaces, RootArtifacts)
	}
	return Ref(clean), nil
}

// Root returns the ref's root ("ws" or "artifacts").
func (r Ref) Root() string {
	root, _, _ := strings.Cut(string(r), "/")
	return root
}

// Join appends slash-separated elements to r and validates the result.
func (r Ref) Join(elem ...string) (Ref, error) {
	return ParseRef(path.Join(append([]string{string(r)}, elem...)...))
}

// Resolve maps r to an absolute path under the state directory. It refuses a
// path that passes through a symlink, a Windows junction, or any other
// special file below the root: filepath.EvalSymlinks does not resolve
// junctions, and a job can create either kind inside ws/, so every existing
// component is checked with Lstat instead. The root itself may be a link
// (the owner may have moved it to another drive).
func Resolve(p state.Paths, r Ref) (string, error) {
	if _, err := ParseRef(string(r)); err != nil {
		return "", err
	}
	base := filepath.Join(p.Root, r.Root())
	abs := filepath.Join(p.Root, filepath.FromSlash(string(r)))
	rel, err := filepath.Rel(base, abs)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("file reference %q escapes %s", r, base)
	}
	cur := base
	for _, seg := range strings.Split(rel, string(filepath.Separator)) {
		if seg == "." {
			continue
		}
		cur = filepath.Join(cur, seg)
		fi, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			break // nothing below this point exists yet
		}
		if err != nil {
			return "", err
		}
		if !plain(fi.Mode()) {
			return "", fmt.Errorf("%s passes through a link or special file, which is never followed (%w)", r, ErrNotRegular)
		}
	}
	return abs, nil
}

// SplitQualified splits "device:ref" into its parts. An unqualified ref
// returns an empty device, meaning the local device.
func SplitQualified(s string) (device, ref string) {
	if d, r, ok := strings.Cut(s, ":"); ok {
		return d, r
	}
	return "", s
}

// NewArtifact creates an empty artifact file artifacts/<source>/<random><ext>
// and returns its ref and open handle. ext includes the dot, e.g. ".wav".
func NewArtifact(p state.Paths, source, ext string) (Ref, *os.File, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", nil, err
	}
	ref, err := ParseRef(RootArtifacts + "/" + source + "/" + hex.EncodeToString(b[:]) + ext)
	if err != nil {
		return "", nil, err
	}
	abs, err := Resolve(p, ref)
	if err != nil {
		return "", nil, err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		return "", nil, err
	}
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", nil, err
	}
	return ref, f, nil
}
