package approval

import (
	"os"

	"golang.org/x/sys/unix"
)

// isTerminal reports whether f is a terminal; /dev/null is a character
// device too, so the mode bits are not enough.
func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	return err == nil
}
