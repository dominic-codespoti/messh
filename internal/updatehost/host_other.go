//go:build !linux && !windows

package updatehost

import (
	"context"
	"messh/internal/state"
)

func discoverPlatform(context.Context, string, string, state.RunInfo) (*Manager, error) {
	return nil, manual("automatic launcher control is supported only on Linux and Windows")
}
