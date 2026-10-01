// Package sysinfo collects a snapshot of the local device's capabilities.
package sysinfo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Info struct {
	Hostname    string    `json:"hostname"`
	OS          string    `json:"os"`
	OSVersion   string    `json:"os_version"`
	Arch        string    `json:"arch"`
	Machine     string    `json:"machine,omitempty"`
	CPU         CPU       `json:"cpu"`
	Memory      Memory    `json:"memory"`
	GPUs        []GPU     `json:"gpus"`
	Runtimes    []Runtime `json:"runtimes"`
	Browsers    []Browser `json:"browsers"`
	Presence    Presence  `json:"presence"`
	CollectedAt time.Time `json:"collected_at"`
	Errors      []string  `json:"errors,omitempty"`
}

type CPU struct {
	Model        string `json:"model"`
	LogicalCores int    `json:"logical_cores"`
}

type Memory struct {
	TotalBytes     uint64 `json:"total_bytes"`
	AvailableBytes uint64 `json:"available_bytes"`
}

type GPU struct {
	Index              int    `json:"index"`
	Name               string `json:"name"`
	Vendor             string `json:"vendor"`
	MemoryTotalMiB     int    `json:"memory_total_mib"`
	MemoryUsedMiB      *int   `json:"memory_used_mib,omitempty"`
	MemoryFreeMiB      *int   `json:"memory_free_mib,omitempty"`
	UtilizationPercent *int   `json:"utilization_percent,omitempty"`
	Driver             string `json:"driver,omitempty"`
}

type Runtime struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Path    string `json:"path"`
}

type Browser struct {
	Name    string `json:"name"`
	Engine  string `json:"engine"`
	Path    string `json:"path"`
	Version string `json:"version,omitempty"`
}

type Presence struct {
	IdleSeconds *int64 `json:"idle_seconds,omitempty"`
	Locked      *bool  `json:"locked,omitempty"`
	Source      string `json:"source"`
}

const cmdTimeout = 3 * time.Second

// errSink collects non-fatal probe errors concurrently.
type errSink struct {
	mu   sync.Mutex
	errs []string
}

func (e *errSink) add(probe string, err error) {
	if err == nil {
		return
	}
	e.mu.Lock()
	e.errs = append(e.errs, probe+": "+err.Error())
	e.mu.Unlock()
}

// Collect gathers everything concurrently. It never fails: probe errors go into Info.Errors.
// Each external command gets a 3s timeout; Collect as a whole respects ctx.
func Collect(ctx context.Context) Info {
	info := Info{
		OS:          runtime.GOOS,
		Arch:        runtime.GOARCH,
		GPUs:        []GPU{},
		Runtimes:    []Runtime{},
		Browsers:    []Browser{},
		Presence:    Presence{Source: "unavailable"},
		CollectedAt: time.Now().UTC(),
	}
	es := &errSink{}
	if h, err := os.Hostname(); err == nil {
		info.Hostname = h
	} else {
		es.add("hostname", err)
	}

	var (
		wg       sync.WaitGroup
		plat     platformInfo
		nvidia   []GPU
		runtimes []Runtime
		browsers []Browser
		presence Presence
	)
	wg.Add(5)
	go func() { defer wg.Done(); plat = collectPlatform(ctx, es) }()
	go func() { defer wg.Done(); nvidia = probeNvidia(ctx, es) }()
	go func() { defer wg.Done(); runtimes = probeRuntimes(ctx, es) }()
	go func() { defer wg.Done(); browsers = probeBrowsers(ctx, es) }()
	go func() { defer wg.Done(); presence = probePresence(ctx, es) }()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		es.add("collect", ctx.Err())
		<-done // commands are bound to ctx, so this returns promptly
	}

	info.OSVersion = plat.osVersion
	info.Machine = plat.machine
	info.CPU = CPU{Model: plat.cpuModel, LogicalCores: runtime.NumCPU()}
	info.Memory = plat.memory
	if len(nvidia) > 0 {
		info.GPUs = append(info.GPUs, nvidia...)
	} else {
		for i, g := range plat.gpus {
			g.Index = i
			info.GPUs = append(info.GPUs, g)
		}
	}
	info.Runtimes = append(info.Runtimes, runtimes...)
	info.Browsers = append(info.Browsers, browsers...)
	info.Presence = presence
	sort.Strings(es.errs)
	info.Errors = es.errs
	return info
}

type platformInfo struct {
	osVersion string
	machine   string
	cpuModel  string
	memory    Memory
	gpus      []GPU // non-NVIDIA fallback adapters
}

// runCmd runs a command with the per-command timeout and returns stdout.
func runCmd(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, cmdTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	hideWindow(cmd)
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		if ctx.Err() != nil {
			return out.String(), fmt.Errorf("timed out")
		}
		return out.String() + stderr.String(), err
	}
	if out.Len() == 0 {
		return stderr.String(), nil
	}
	return out.String(), nil
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if len(l) > 120 {
				l = l[:120]
			}
			return l
		}
	}
	return ""
}

func probeNvidia(ctx context.Context, es *errSink) []GPU {
	exe, err := exec.LookPath("nvidia-smi")
	if err != nil {
		return nil
	}
	out, err := runCmd(ctx, exe,
		"--query-gpu=index,name,memory.total,memory.used,memory.free,utilization.gpu,driver_version",
		"--format=csv,noheader,nounits")
	if err != nil {
		es.add("nvidia-smi", err)
		return nil
	}
	var gpus []GPU
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Split(line, ",")
		if len(f) < 7 {
			es.add("nvidia-smi", fmt.Errorf("unexpected line %q", line))
			continue
		}
		for i := range f {
			f[i] = strings.TrimSpace(f[i])
		}
		g := GPU{Name: f[1], Vendor: "nvidia", Driver: na(f[6])}
		if p := parseIntPtr(f[0]); p != nil {
			g.Index = *p
		}
		if p := parseIntPtr(f[2]); p != nil {
			g.MemoryTotalMiB = *p
		}
		g.MemoryUsedMiB = parseIntPtr(f[3])
		g.MemoryFreeMiB = parseIntPtr(f[4])
		g.UtilizationPercent = parseIntPtr(f[5])
		gpus = append(gpus, g)
	}
	return gpus
}

func na(s string) string {
	if strings.HasPrefix(s, "[") || s == "N/A" {
		return ""
	}
	return s
}

func parseIntPtr(s string) *int {
	n, err := strconv.Atoi(na(s))
	if err != nil {
		return nil
	}
	return &n
}

type runtimeCandidate struct {
	name string
	exes []string
	args []string
}

var runtimeCandidates = []runtimeCandidate{
	{"python", []string{"python3", "python"}, []string{"--version"}},
	{"uv", []string{"uv"}, []string{"--version"}},
	{"node", []string{"node"}, []string{"--version"}},
	{"bun", []string{"bun"}, []string{"--version"}},
	{"go", []string{"go"}, []string{"version"}},
	{"git", []string{"git"}, []string{"--version"}},
	{"docker", []string{"docker"}, []string{"--version"}},
	{"ollama", []string{"ollama"}, []string{"--version"}},
	{"nvcc", []string{"nvcc"}, []string{"--version"}},
	{"ffmpeg", []string{"ffmpeg"}, []string{"-version"}},
}

func probeRuntimes(ctx context.Context, es *errSink) []Runtime {
	results := make([]*Runtime, len(runtimeCandidates))
	var wg sync.WaitGroup
	for i, c := range runtimeCandidates {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = probeRuntime(ctx, c, es)
		}()
	}
	wg.Wait()
	var out []Runtime
	for _, r := range results {
		if r != nil {
			out = append(out, *r)
		}
	}
	return out
}

func probeRuntime(ctx context.Context, c runtimeCandidate, es *errSink) *Runtime {
	for _, exe := range c.exes {
		path, err := exec.LookPath(exe)
		if err != nil {
			continue
		}
		out, err := runCmd(ctx, path, c.args...)
		if c.name == "python" && (err != nil || isStoreStub(path, out)) {
			continue // Microsoft Store alias or broken interpreter: try next
		}
		if err != nil {
			es.add(c.name, err)
			return nil
		}
		v := firstLine(out)
		if c.name == "nvcc" {
			// nvcc prints a banner; the release line is the useful one.
			for _, l := range strings.Split(out, "\n") {
				if strings.Contains(l, "release") {
					v = firstLine(l)
				}
			}
		}
		return &Runtime{Name: c.name, Version: v, Path: path}
	}
	return nil
}

func isStoreStub(path, out string) bool {
	return strings.Contains(out, "Python was not found") ||
		strings.Contains(strings.ToLower(path), `\microsoft\windowsapps\`) && strings.TrimSpace(out) == ""
}

var errNoSession = errors.New("no session")
