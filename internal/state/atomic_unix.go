//go:build !windows

package state

import (
	"os"
)

func replaceFile(tmp, path string) error { return os.Rename(tmp, path) }

func syncDirectory(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
