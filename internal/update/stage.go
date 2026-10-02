package update

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const maxExpandedSize int64 = 512 << 20

// Staged owns a private directory beside the installed executable. Keep it until
// Commit or Rollback completes; Close removes archives and abandoned candidates.
type Staged struct {
	Path string
	dir  string
}

func (s *Staged) Close() error {
	if s == nil || s.dir == "" {
		return nil
	}
	return os.RemoveAll(s.dir)
}

func (c *Client) DownloadStage(ctx context.Context, release Release, executable string) (_ *Staged, err error) {
	executable, err = executablePath(executable)
	if err != nil {
		return nil, err
	}
	asset := release.Asset
	if release.archiveURL == "" || asset.Size <= 0 || asset.Size > maxArchiveSize || !digestPattern.MatchString(asset.SHA256) {
		return nil, errors.New("release must come from a successful update check")
	}
	dir, err := privateStageDir(filepath.Dir(executable))
	if err != nil {
		return nil, err
	}
	stage := &Staged{Path: filepath.Join(dir, filepath.Base(executable)), dir: dir}
	defer func() {
		if err != nil {
			err = errors.Join(err, stage.Close())
		}
	}()
	archive, err := os.OpenFile(filepath.Join(dir, "artifact"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	defer archive.Close()
	resp, err := c.request(ctx, release.archiveURL, false)
	if err != nil {
		return nil, err
	}
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(archive, hash), io.LimitReader(resp.Body, asset.Size+1))
	closeErr := resp.Body.Close()
	if err := errors.Join(copyErr, closeErr); err != nil {
		return nil, err
	}
	if size != asset.Size || hex.EncodeToString(hash.Sum(nil)) != asset.SHA256 {
		return nil, errors.New("archive size or checksum mismatch")
	}
	output, err := os.OpenFile(stage.Path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	if asset.OS == "windows" {
		err = extractZIP(archive, size, asset.Executable, output)
	} else {
		if _, err = archive.Seek(0, io.SeekStart); err == nil {
			err = extractTarGZ(archive, asset.Executable, output)
		}
	}
	if err == nil {
		err = output.Sync()
	}
	err = errors.Join(err, output.Close())
	if err != nil {
		return nil, fmt.Errorf("extract update: %w", err)
	}
	if err = os.Chmod(stage.Path, 0700); err != nil {
		return nil, err
	}
	return stage, nil
}

func safeMember(name, expected string, directory bool) bool {
	if strings.ContainsAny(name, "\\:\x00") || strings.HasPrefix(name, "/") {
		return false
	}
	clean := name
	if directory {
		clean = strings.TrimSuffix(clean, "/")
	}
	if clean == "" || path.Clean(clean) != clean || clean == "." || clean == ".." {
		return false
	}
	root := strings.SplitN(expected, "/", 2)[0]
	return clean == root && directory || strings.HasPrefix(clean, root+"/")
}

func extractZIP(file *os.File, size int64, expected string, output io.Writer) error {
	reader, err := zip.NewReader(file, size)
	if err != nil {
		return err
	}
	if len(reader.File) > 4096 {
		return errors.New("too many archive members")
	}
	seen := make(map[string]bool)
	found := false
	var total uint64
	for _, member := range reader.File {
		directory := member.FileInfo().IsDir()
		if !safeMember(member.Name, expected, directory) || seen[strings.TrimSuffix(member.Name, "/")] || (!directory && !member.Mode().IsRegular()) || (directory && member.Mode()&os.ModeSymlink != 0) {
			return errors.New("unsafe or duplicate ZIP member")
		}
		seen[strings.TrimSuffix(member.Name, "/")] = true
		if member.UncompressedSize64 > uint64(maxExpandedSize)-total {
			return errors.New("expanded archive exceeds size limit")
		}
		total += member.UncompressedSize64
		if directory {
			if member.UncompressedSize64 != 0 {
				return errors.New("nonempty ZIP directory")
			}
			continue
		}
		input, err := member.Open()
		if err != nil {
			return err
		}
		var destination io.Writer = io.Discard
		if member.Name == expected {
			if member.UncompressedSize64 == 0 || member.UncompressedSize64 > uint64(maxArchiveSize) {
				input.Close()
				return errors.New("invalid executable size")
			}
			found, destination = true, output
		}
		n, err := io.Copy(destination, io.LimitReader(input, int64(member.UncompressedSize64)+1))
		err = errors.Join(err, input.Close())
		if err != nil {
			return err
		}
		if uint64(n) != member.UncompressedSize64 {
			return errors.New("ZIP member size mismatch")
		}
	}
	if !found {
		return errors.New("expected executable is missing from archive")
	}
	return nil
}

func extractTarGZ(input io.Reader, expected string, output io.Writer) error {
	compressed, err := gzip.NewReader(input)
	if err != nil {
		return err
	}
	defer compressed.Close()
	bounded := &io.LimitedReader{R: compressed, N: maxExpandedSize + 1}
	reader := tar.NewReader(bounded)
	seen := make(map[string]bool)
	found := false
	for count := 0; ; count++ {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if count >= 4096 {
			return errors.New("too many archive members")
		}
		directory := header.Typeflag == tar.TypeDir
		if !safeMember(header.Name, expected, directory) || seen[strings.TrimSuffix(header.Name, "/")] || (header.Typeflag != tar.TypeReg && !directory) {
			return errors.New("unsafe or duplicate TAR member")
		}
		seen[strings.TrimSuffix(header.Name, "/")] = true
		if header.Size < 0 || header.Size > maxExpandedSize || directory && header.Size != 0 {
			return errors.New("invalid TAR member size")
		}
		var destination io.Writer = io.Discard
		if header.Name == expected && !directory {
			if header.Size == 0 || header.Size > maxArchiveSize {
				return errors.New("invalid executable size")
			}
			found, destination = true, output
		}
		if _, err := io.Copy(destination, reader); err != nil {
			return err
		}
	}
	if _, err := io.Copy(io.Discard, bounded); err != nil {
		return err
	}
	if bounded.N <= 0 {
		return errors.New("expanded archive exceeds size limit")
	}
	if !found {
		return errors.New("expected executable is missing from archive")
	}
	return nil
}
