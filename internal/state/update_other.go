//go:build !linux && !windows

package state

import (
	"errors"
	"os"
)

func lockUpdateStartupFile(*os.File) error {
	return errors.New("startup update transactions are supported only on Linux and Windows")
}
