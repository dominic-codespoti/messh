//go:build !linux && !windows

package update

import (
	"errors"
	"os"
)

func lockFile(*os.File) error {
	return errors.New("self-update is supported only on Linux and Windows")
}
func unlockFile(*os.File) {}
func privateStageDir(string) (string, error) {
	return "", errors.New("self-update is supported only on Linux and Windows")
}
