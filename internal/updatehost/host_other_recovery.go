//go:build !linux && !windows

package updatehost

import (
	"context"
	"messh/internal/state"
)

// RecoverInterrupted cannot manage launchers on unsupported platforms.
func RecoverInterrupted(context.Context, state.Paths) error {
	return manual("automatic launcher recovery is supported only on Linux and Windows")
}
