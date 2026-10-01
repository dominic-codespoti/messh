package jobs

import (
	"context"
	"runtime"
	"sync"
	"time"

	"messh/internal/sysinfo"
)

// Resources reports what the device can offer right now. The scheduler asks
// only when a queued job has a claim that needs live numbers.
type Resources interface {
	Snapshot(ctx context.Context) (Snapshot, error)
}

// Snapshot is one reading of the device.
type Snapshot struct {
	CPUThreads int
	MemTotalMB int64
	MemAvailMB int64
	GPUs       []GPUState
}

// GPUState is one GPU. FreeMB is only meaningful when FreeKnown (nvidia-smi
// reports it; the Windows registry fallback does not).
type GPUState struct {
	Index     int    `json:"index"`
	Name      string `json:"name"`
	TotalMB   int64  `json:"vram_total_mb"`
	FreeMB    int64  `json:"vram_free_mb"`
	FreeKnown bool   `json:"vram_free_known"`
}

// sysResources reads sysinfo, caching briefly because a full collection
// launches several probes.
type sysResources struct {
	ttl time.Duration

	mu   sync.Mutex
	at   time.Time
	snap Snapshot
}

func newSysResources() *sysResources { return &sysResources{ttl: 2 * time.Second} }

func (s *sysResources) Snapshot(ctx context.Context) (Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.at.IsZero() && time.Since(s.at) < s.ttl {
		return s.snap, nil
	}
	info := sysinfo.Collect(ctx)
	if err := ctx.Err(); err != nil {
		return Snapshot{}, err
	}
	snap := Snapshot{
		CPUThreads: max(info.CPU.LogicalCores, runtime.NumCPU()),
		MemTotalMB: int64(info.Memory.TotalBytes >> 20),
		MemAvailMB: int64(info.Memory.AvailableBytes >> 20),
	}
	for _, g := range info.GPUs {
		st := GPUState{Index: g.Index, Name: g.Name, TotalMB: int64(g.MemoryTotalMiB)}
		if g.MemoryFreeMiB != nil {
			st.FreeMB, st.FreeKnown = int64(*g.MemoryFreeMiB), true
		}
		snap.GPUs = append(snap.GPUs, st)
	}
	s.at, s.snap = time.Now(), snap
	return snap, nil
}
