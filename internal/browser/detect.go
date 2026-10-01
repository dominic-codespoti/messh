package browser

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Installed is a browser found on this device.
type Installed struct {
	Family string // "chrome", "msedge", "chromium", "brave"
	Path   string
}

type candidate struct {
	family string
	paths  []string // absolute paths tried in order
	names  []string // executable names looked up on PATH
}

func candidates() []candidate {
	if runtime.GOOS == "windows" {
		pf, pf86, local := os.Getenv("ProgramFiles"), os.Getenv("ProgramFiles(x86)"), os.Getenv("LocalAppData")
		join := func(bases []string, rel string) []string {
			var out []string
			for _, b := range bases {
				if b != "" {
					out = append(out, filepath.Join(b, rel))
				}
			}
			return out
		}
		all := []string{pf, pf86, local}
		return []candidate{
			{family: "chrome", paths: join(all, `Google\Chrome\Application\chrome.exe`)},
			{family: "msedge", paths: join([]string{pf86, pf}, `Microsoft\Edge\Application\msedge.exe`)},
			{family: "chromium", paths: join(all, `Chromium\Application\chrome.exe`)},
			{family: "brave", paths: join(all, `BraveSoftware\Brave-Browser\Application\brave.exe`)},
		}
	}
	return []candidate{
		{family: "chrome", paths: []string{"/opt/google/chrome/chrome"}, names: []string{"google-chrome-stable", "google-chrome", "chrome"}},
		{family: "msedge", paths: []string{"/opt/microsoft/msedge/msedge"}, names: []string{"microsoft-edge-stable", "microsoft-edge", "msedge"}},
		{family: "chromium", names: []string{"chromium", "chromium-browser"}},
		{family: "brave", names: []string{"brave-browser", "brave", "brave-browser-stable"}},
	}
}

// FindBrowsers lists the Chromium-based browsers installed here, one per family.
func FindBrowsers() []Installed {
	var out []Installed
	for _, c := range candidates() {
		if p := c.find(); p != "" {
			out = append(out, Installed{Family: c.family, Path: p})
		}
	}
	return out
}

func (c candidate) find() string {
	for _, p := range c.paths {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	for _, n := range c.names {
		if p, err := exec.LookPath(n); err == nil {
			return p
		}
	}
	return ""
}

// BrowserPath finds the executable for a channel ("chrome" or "msedge").
func BrowserPath(channel string) string {
	for _, c := range candidates() {
		if c.family == channel {
			return c.find()
		}
	}
	return ""
}

// standardChannelPath is where Playwright itself looks for a channel; when the
// browser lives elsewhere (found on PATH) the executable is passed explicitly.
func standardChannelPath(channel string) string {
	for _, c := range candidates() {
		if c.family != channel {
			continue
		}
		for _, p := range c.paths {
			if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
				return p
			}
		}
	}
	return ""
}

// ProcessNames are the executable names whose presence means the owner's
// browser is running (extension mode attaches to a running browser).
func ProcessNames(channel string) []string {
	switch {
	case channel == "msedge" && runtime.GOOS == "windows":
		return []string{"msedge.exe"}
	case channel == "msedge":
		return []string{"msedge", "microsoft-edge", "microsoft-edge-stable"}
	case runtime.GOOS == "windows":
		return []string{"chrome.exe"}
	}
	return []string{"chrome", "google-chrome", "google-chrome-stable"}
}

// BrowserRunning reports whether the owner's browser for channel is running.
func BrowserRunning(channel string) bool { return browserRunning(ProcessNames(channel)...) }

// Runtime describes the Node.js side.
type Runtime struct {
	Command     string // resolved program
	NodeVersion string
	Err         error
}

// CheckRuntime resolves the command's program and reports the Node version.
func CheckRuntime(cmd Command) Runtime {
	if len(cmd) == 0 {
		cmd = DefaultCommand()
	}
	path, err := exec.LookPath(cmd[0])
	if err != nil {
		return Runtime{Err: missingNode(cmd[0])}
	}
	r := Runtime{Command: path}
	if node, err := exec.LookPath("node"); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, node, "--version").Output(); err == nil {
			r.NodeVersion = strings.TrimSpace(string(out))
		}
	}
	return r
}

func missingNode(prog string) error {
	if prog == "npx" || prog == "npm" || prog == "node" {
		return fmt.Errorf("%s was not found on PATH: install Node.js 18 or newer from https://nodejs.org (Playwright MCP runs on it), then restart the messh node so it sees the new PATH", prog)
	}
	return fmt.Errorf("%q was not found on PATH: fix \"command\" in browser.json", prog)
}

// ErrNoBrowser is returned when no usable browser binary exists.
var ErrNoBrowser = errors.New("no browser found")

// launchPlan resolves which browser the upstream should start.
type launchPlan struct {
	Channel    string // value for --browser
	Executable string // value for --executable-path, when non-empty
}

func planBrowser(c Config) (launchPlan, error) {
	c = c.Normalized()
	if c.Executable != "" {
		if fi, err := os.Stat(c.Executable); err != nil || fi.IsDir() {
			return launchPlan{}, fmt.Errorf("%w: executable %q in browser.json does not exist", ErrNoBrowser, c.Executable)
		}
		return launchPlan{Channel: c.Channel, Executable: c.Executable}, nil
	}
	if p := standardChannelPath(c.Channel); p != "" {
		return launchPlan{Channel: c.Channel}, nil
	}
	if p := BrowserPath(c.Channel); p != "" {
		return launchPlan{Channel: c.Channel, Executable: p}, nil
	}
	found := FindBrowsers()
	base := channelLabel(c.Channel) + " is not installed on this device"
	if len(found) == 0 {
		return launchPlan{}, fmt.Errorf("%w: %s; install Google Chrome or Microsoft Edge, then run: messh browser setup", ErrNoBrowser, base)
	}
	var names []string
	for _, f := range found {
		names = append(names, f.Family+" ("+f.Path+")")
	}
	return launchPlan{}, fmt.Errorf("%w: %s; found %s. Switch with: messh browser setup --channel <chrome|msedge>, or set \"executable\" in browser.json", ErrNoBrowser, base, strings.Join(names, ", "))
}

func channelLabel(ch string) string {
	switch ch {
	case "msedge":
		return "Microsoft Edge"
	case "chrome":
		return "Google Chrome"
	}
	return ch
}

// Plan is the browser the upstream will launch.
type Plan = launchPlan

// PlanFor resolves which browser binary and channel c selects, or explains
// what is missing.
func PlanFor(c Config) (Plan, error) { return planBrowser(c) }
