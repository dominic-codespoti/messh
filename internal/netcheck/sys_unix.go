//go:build !windows

package netcheck

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// probeTimeout bounds each firewall tool call.
const probeTimeout = 3 * time.Second

// run returns the combined output of a command (tools print errors such as
// "You need to be root" on either stream).
func run(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = time.Second
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return out, fmt.Errorf("%s timed out after %s", filepath.Base(name), probeTimeout)
	}
	return out, err
}

// findTool looks on PATH, then in the sbin directories a normal user's PATH
// often lacks.
func findTool(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	for _, dir := range []string{"/usr/sbin", "/sbin", "/usr/local/sbin"} {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return p
		}
	}
	return ""
}

// ProbeLinux reads ufw, firewalld and nftables without root; whatever needs
// root is reported as unreadable instead.
func ProbeLinux(ctx context.Context, ifaces []Iface) LinuxFirewall {
	var f LinuxFirewall
	if ufw := findTool("ufw"); ufw != "" {
		f.UFWInstalled = true
		out, err := run(ctx, ufw, "status", "verbose")
		if st, ok := ParseUFW(string(out)); ok {
			f.UFW, f.UFWActive = &st, st.Active
		} else {
			f.UFWNote = firstLine(out, err)
			if conf, err := os.ReadFile("/etc/ufw/ufw.conf"); err == nil && ufwConfEnabled(string(conf)) {
				f.UFWActive = true
			}
		}
	}
	if fc := findTool("firewall-cmd"); fc != "" {
		if out, _ := run(ctx, fc, "--state"); strings.TrimSpace(string(out)) == "running" {
			f.FirewalldRunning = true
			for _, zone := range firewalldZones(ctx, fc, ifaces) {
				out, err := run(ctx, fc, "--list-all", "--zone="+zone)
				z, ok := ParseFirewalld(string(out))
				if !ok {
					f.Firewalld, f.FirewalldNote = nil, firstLine(out, err)
					break
				}
				f.Firewalld = append(f.Firewalld, z)
			}
		}
	}
	if nft := findTool("nft"); nft != "" {
		f.NftInstalled = true
		out, err := run(ctx, nft, "list", "ruleset")
		if err == nil {
			f.NftRead, f.Nft = true, ParseNft(string(out))
		} else {
			f.NftNote = firstLine(out, err)
		}
	}
	return f
}

// firewalldZones returns the zones of the LAN interfaces (an interface in no
// zone uses the default zone).
func firewalldZones(ctx context.Context, fc string, ifaces []Iface) []string {
	var zones []string
	add := func(z string) {
		if z = strings.TrimSpace(z); z != "" && !strings.ContainsAny(z, " \t\n") && !slices.Contains(zones, z) {
			zones = append(zones, z)
		}
	}
	needDefault := len(ifaces) == 0
	for _, i := range ifaces {
		out, err := run(ctx, fc, "--get-zone-of-interface="+i.Name)
		if err != nil {
			needDefault = true
			continue
		}
		add(string(out))
	}
	if needDefault {
		if out, err := run(ctx, fc, "--get-default-zone"); err == nil {
			add(string(out))
		}
	}
	return zones
}

func ufwConfEnabled(conf string) bool {
	for line := range strings.Lines(conf) {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok && k == "ENABLED" && strings.EqualFold(strings.Trim(v, `"' `), "yes") {
			return true
		}
	}
	return false
}

func firstLine(out []byte, err error) string {
	for line := range strings.Lines(string(out)) {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	if err != nil {
		return err.Error()
	}
	return "no output"
}

func osChecks(ctx context.Context, ifaces []Iface, _ string, _ Ports) []Check {
	return LinuxChecks(ProbeLinux(ctx, ifaces), ifaces)
}

// processPath returns the executable of a running process.
func processPath(pid int) (string, error) {
	return os.Readlink("/proc/" + strconv.Itoa(pid) + "/exe")
}
