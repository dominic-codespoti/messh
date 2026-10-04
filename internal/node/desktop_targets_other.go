//go:build !windows

package node

import (
	"context"

	"messh/internal/state"
)

// probeDesktopTargets off Windows: host inventory is honestly unavailable —
// wsl.exe is a Windows program, and no substitute inventory exists. Any
// configured-state verdicts come from classify's unknown paths, never from a
// fabricated distro list. A failed load still reaches here as facts with
// InvErr preserved; unconfigured stays quiet.
func probeDesktopTargets(_ context.Context, _ *state.WSLTargetConfig) desktopFacts {
	return desktopFacts{Unavailable: true}
}
