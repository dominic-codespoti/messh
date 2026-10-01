//go:build linux

package sysinfo

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func hideWindow(*exec.Cmd) {}

func collectPlatform(_ context.Context, es *errSink) platformInfo {
	var p platformInfo

	if kv, err := readKV("/etc/os-release", "="); err == nil {
		p.osVersion = strings.Trim(kv["PRETTY_NAME"], `"'`)
		if p.osVersion == "" {
			p.osVersion = strings.Trim(kv["NAME"], `"'`)
		}
	} else if !os.IsNotExist(err) {
		es.add("os_version", err)
	}

	if b, err := os.ReadFile("/proc/device-tree/model"); err == nil {
		p.machine = strings.TrimSpace(strings.TrimRight(string(b), "\x00"))
	}
	if p.machine == "" {
		vendor := readTrim("/sys/class/dmi/id/sys_vendor")
		product := readTrim("/sys/class/dmi/id/product_name")
		p.machine = strings.TrimSpace(vendor + " " + product)
	}

	if f, err := os.Open("/proc/cpuinfo"); err == nil {
		vals := map[string]string{}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			k, v, ok := strings.Cut(sc.Text(), ":")
			if !ok {
				continue
			}
			k = strings.TrimSpace(k)
			if _, dup := vals[k]; !dup {
				vals[k] = strings.TrimSpace(v)
			}
		}
		f.Close()
		for _, k := range []string{"model name", "Model", "Hardware"} {
			if v := vals[k]; v != "" {
				p.cpuModel = v
				break
			}
		}
	} else {
		es.add("cpuinfo", err)
	}
	if p.cpuModel == "" && runtime.GOARCH == "arm64" {
		p.cpuModel = p.machine
	}

	if kv, err := readKV("/proc/meminfo", ":"); err == nil {
		p.memory = Memory{TotalBytes: kB(kv["MemTotal"]), AvailableBytes: kB(kv["MemAvailable"])}
	} else {
		es.add("meminfo", err)
	}
	return p
}

func readTrim(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func readKV(path, sep string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	kv := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		if k, v, ok := strings.Cut(line, sep); ok {
			kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return kv, nil
}

func kB(s string) uint64 {
	n, _ := strconv.ParseUint(strings.TrimSpace(strings.TrimSuffix(s, "kB")), 10, 64)
	return n * 1024
}

var linuxBrowsers = []struct {
	name, engine string
	exes         []string
}{
	{"Google Chrome", "chromium", []string{"google-chrome-stable", "google-chrome"}},
	{"Microsoft Edge", "chromium", []string{"microsoft-edge-stable", "microsoft-edge"}},
	{"Chromium", "chromium", []string{"chromium", "chromium-browser"}},
	{"Brave", "chromium", []string{"brave", "brave-browser"}},
	{"Firefox", "gecko", []string{"firefox"}},
}

func probeBrowsers(ctx context.Context, _ *errSink) []Browser {
	res := make([]*Browser, len(linuxBrowsers))
	done := make(chan struct{}, len(linuxBrowsers))
	for i, lb := range linuxBrowsers {
		go func() {
			defer func() { done <- struct{}{} }()
			for _, exe := range lb.exes {
				path, err := exec.LookPath(exe)
				if err != nil {
					continue
				}
				b := &Browser{Name: lb.name, Engine: lb.engine, Path: path}
				if out, err := runCmd(ctx, path, "--version"); err == nil {
					b.Version = firstLine(out)
				}
				res[i] = b
				return
			}
		}()
	}
	for range linuxBrowsers {
		<-done
	}
	var out []Browser
	for _, b := range res {
		if b != nil {
			out = append(out, *b)
		}
	}
	return out
}

func probePresence(ctx context.Context, _ *errSink) Presence {
	unavailable := Presence{Source: "unavailable"}
	loginctl, err := exec.LookPath("loginctl")
	if err != nil {
		return unavailable
	}
	id := os.Getenv("XDG_SESSION_ID")
	if id == "" {
		if id, err = activeSession(ctx, loginctl); err != nil {
			return unavailable
		}
	}
	out, err := runCmd(ctx, loginctl, "show-session", id, "-p", "IdleHint", "-p", "IdleSinceHint", "-p", "LockedHint")
	if err != nil {
		return unavailable
	}
	kv := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			kv[k] = v
		}
	}
	p := Presence{Source: "logind"}
	switch kv["IdleHint"] {
	case "yes":
		if us, err := strconv.ParseInt(kv["IdleSinceHint"], 10, 64); err == nil && us > 0 {
			s := int64(time.Since(time.UnixMicro(us)).Seconds())
			if s < 0 {
				s = 0
			}
			p.IdleSeconds = &s
		}
	case "no":
		var s int64
		p.IdleSeconds = &s
	}
	switch kv["LockedHint"] {
	case "yes":
		v := true
		p.Locked = &v
	case "no":
		v := false
		p.Locked = &v
	}
	if p.IdleSeconds == nil && p.Locked == nil {
		return unavailable
	}
	return p
}

// activeSession picks the current user's session, preferring one with a seat
// (graphical) when several exist. Columns: SESSION UID USER SEAT [TTY ...].
func activeSession(ctx context.Context, loginctl string) (string, error) {
	out, err := runCmd(ctx, loginctl, "list-sessions", "--no-legend")
	if err != nil {
		return "", err
	}
	uid := strconv.Itoa(os.Getuid())
	var first string
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || f[1] != uid {
			continue
		}
		if len(f) >= 4 && strings.HasPrefix(f[3], "seat") {
			return f[0], nil
		}
		if first == "" {
			first = f[0]
		}
	}
	if first == "" {
		return "", errNoSession
	}
	return first, nil
}
