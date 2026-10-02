package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"messh/internal/files"
)

const maxCheckpointSize = 64 << 20

// publishCheckpoint snapshots the configured workspace checkpoint into the
// private job directory. Callers commit the resulting reference before use.
func (p *Provider) publishCheckpoint(j *job) error {
	if j.Recovery == nil {
		return errors.New("checkpoint recovery is not configured")
	}
	ref, err := files.ParseRef(files.RootWorkspaces + "/" + j.Workspace + "/" + j.Recovery.Checkpoint)
	if err != nil {
		return err
	}
	src, err := files.Resolve(p.paths, ref)
	if err != nil {
		return err
	}
	st, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return errors.New("checkpoint is not a regular non-symlink file")
	}
	if st.Size() > maxCheckpointSize {
		return errors.New("checkpoint exceeds 64 MiB limit")
	}
	sum, st, _, err := p.hashes.hashFile(src)
	if err != nil {
		return err
	}
	if st.Size() > maxCheckpointSize {
		return errors.New("checkpoint exceeds 64 MiB limit")
	}
	if sum == j.CheckpointSHA256 && j.CheckpointSnapshot != "" {
		committed := filepath.Join(p.jobDir(j.ID), j.CheckpointSnapshot)
		if info, err := os.Lstat(committed); err == nil && info.Mode().IsRegular() && info.Size() <= maxCheckpointSize {
			if existing, _, _, hashErr := p.hashes.hashFile(committed); hashErr == nil && existing == sum {
				return nil
			}
		}
	}
	name := fmt.Sprintf("checkpoint-%d-%s.bin", j.Attempt+1, sum)
	dst := filepath.Join(p.jobDir(j.ID), name)
	if err := os.MkdirAll(p.jobDir(j.ID), 0o700); err != nil {
		return err
	}
	// Reuse a prior unreferenced publication after a transient job-record write
	// failure instead of copying the same immutable bytes every tick.
	if info, err := os.Lstat(dst); err == nil && info.Mode().IsRegular() && info.Size() <= maxCheckpointSize {
		if existing, _, _, hashErr := p.hashes.hashFile(dst); hashErr == nil && existing == sum {
			j.CheckpointSnapshot, j.CheckpointSHA256 = name, sum
			return nil
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if p.opts.WriteState != nil || runtime.GOOS == "windows" {
		f, err := os.Open(src)
		if err != nil {
			return err
		}
		data, readErr := io.ReadAll(io.LimitReader(f, maxCheckpointSize+1))
		closeErr := f.Close()
		if readErr == nil {
			readErr = closeErr
		}
		if readErr != nil {
			return readErr
		}
		if len(data) > maxCheckpointSize {
			return errors.New("checkpoint exceeds 64 MiB limit")
		}
		actual := sha256.Sum256(data)
		if hex.EncodeToString(actual[:]) != sum {
			return errors.New("checkpoint changed while being published")
		}
		if err := p.writeState(dst, data, 0o600); err != nil {
			return err
		}
	} else if err := streamCheckpointAtomic(src, dst, sum); err != nil {
		return err
	}
	j.CheckpointSnapshot, j.CheckpointSHA256 = name, sum
	return nil
}

// streamCheckpointAtomic copies, bounds, hashes, syncs, and atomically
// publishes without materializing checkpoint contents in memory.
func streamCheckpointAtomic(src, dst, expectedHash string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".checkpoint-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(f, maxCheckpointSize+1))
	if err != nil {
		_ = tmp.Close()
		return err
	}
	if n > maxCheckpointSize {
		_ = tmp.Close()
		return errors.New("checkpoint exceeds 64 MiB limit")
	}
	if hex.EncodeToString(h.Sum(nil)) != expectedHash {
		_ = tmp.Close()
		return errors.New("checkpoint changed while being published")
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return err
	}
	ok = true
	if runtime.GOOS != "windows" {
		if dir, err := os.Open(filepath.Dir(dst)); err == nil {
			syncErr := dir.Sync()
			closeErr := dir.Close()
			if syncErr != nil {
				return syncErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
	}
	return nil
}

// commitCheckpoint publishes bytes and references them only after the
// authoritative job record commits. Identical committed checkpoints require
// no repeated disk write or record transition.
func (p *Provider) commitCheckpointLocked(j *job) error {
	oldName, oldHash := j.CheckpointSnapshot, j.CheckpointSHA256
	if err := p.publishCheckpoint(j); err != nil {
		return err
	}
	if oldName == j.CheckpointSnapshot && oldHash == j.CheckpointSHA256 {
		return nil
	}
	if err := p.touchLocked(j); err != nil {
		j.CheckpointSnapshot, j.CheckpointSHA256 = oldName, oldHash
		return err
	}
	return nil
}

// restoreCheckpoint verifies the committed snapshot and restores it to the
// workspace path requested by the original submission before launch.
func (p *Provider) restoreCheckpoint(j *job) error {
	if filepath.Base(j.CheckpointSnapshot) != j.CheckpointSnapshot || j.CheckpointSnapshot == "" {
		return errors.New("invalid checkpoint snapshot path")
	}
	snapshot := filepath.Join(p.jobDir(j.ID), j.CheckpointSnapshot)
	st, err := os.Lstat(snapshot)
	if err != nil {
		return fmt.Errorf("checkpoint snapshot unavailable: %w", err)
	}
	if !st.Mode().IsRegular() || st.Size() > maxCheckpointSize {
		return errors.New("checkpoint snapshot unavailable or exceeds 64 MiB limit")
	}
	sum, _, _, err := p.hashes.hashFile(snapshot)
	if err != nil {
		return fmt.Errorf("checkpoint snapshot unavailable: %w", err)
	}
	if sum != j.CheckpointSHA256 {
		return errors.New("checkpoint snapshot hash mismatch")
	}
	ref, err := files.ParseRef(files.RootWorkspaces + "/" + j.Workspace + "/" + j.Recovery.Checkpoint)
	if err != nil {
		return err
	}
	dst, err := files.Resolve(p.paths, ref)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	if p.opts.WriteState != nil || runtime.GOOS == "windows" {
		f, err := os.Open(snapshot)
		if err != nil {
			return err
		}
		data, readErr := io.ReadAll(io.LimitReader(f, maxCheckpointSize+1))
		closeErr := f.Close()
		if readErr == nil {
			readErr = closeErr
		}
		if readErr != nil {
			return readErr
		}
		if len(data) > maxCheckpointSize {
			return errors.New("checkpoint snapshot exceeds 64 MiB limit")
		}
		actual := sha256.Sum256(data)
		if hex.EncodeToString(actual[:]) != j.CheckpointSHA256 {
			return errors.New("checkpoint snapshot hash mismatch")
		}
		return p.writeState(dst, data, 0o600)
	}
	return streamCheckpointAtomic(snapshot, dst, j.CheckpointSHA256)
}
