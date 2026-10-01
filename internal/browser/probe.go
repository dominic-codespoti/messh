package browser

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"messh/internal/state"
)

// PinnedServerVersion is the Playwright version @playwright/mcp PinnedVersion
// reports in its MCP server info.
const PinnedServerVersion = "1.64.0-alpha-1790635538000"

// Probe is the result of checking that the browser side can work, for
// `messh browser status`. It never opens a browser window: Playwright MCP
// launches the browser on the first tool call, and the probe makes none.
type Probe struct {
	Command        string   // resolved program
	NodeVersion    string   // node --version
	PackageVersion string   // @playwright/mcp version, from --version
	ServerVersion  string   // Playwright version the MCP server reports
	Browser        string   // path of the browser that would be used
	BrowserRunning bool     // extension mode: the owner's browser is running
	Missing        []string // published tools this Playwright MCP does not offer
	Problems       []string // everything that stops the browser tools from working
}

var versionRE = regexp.MustCompile(`\d+\.\d+\.\d+[^\s]*`)

// ProbeSetup checks the runtime, the browser, and that Playwright MCP starts
// and speaks MCP. It runs in a scratch state directory, so a node using the
// real one is not disturbed.
func ProbeSetup(ctx context.Context, cfg Config) Probe {
	cfg = cfg.Normalized()
	var pr Probe
	rt := CheckRuntime(cfg.Command)
	if rt.Err != nil {
		pr.Problems = append(pr.Problems, rt.Err.Error())
		return pr
	}
	pr.Command, pr.NodeVersion = rt.Command, rt.NodeVersion

	if plan, err := planBrowser(cfg); err != nil {
		pr.Problems = append(pr.Problems, err.Error())
	} else if plan.Executable != "" {
		pr.Browser = plan.Executable
	} else {
		pr.Browser = standardChannelPath(plan.Channel)
	}
	if cfg.Mode == ModeExtension {
		pr.BrowserRunning = BrowserRunning(cfg.Channel)
		if !pr.BrowserRunning {
			pr.Problems = append(pr.Problems, fmt.Sprintf("%s is not running; extension mode attaches to the browser you have open", channelLabel(cfg.Channel)))
		}
	}

	// --version doubles as the download of Playwright MCP through npx.
	vctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	out, err := exec.CommandContext(vctx, rt.Command, append(append([]string(nil), cfg.Command[1:]...), "--version")...).CombinedOutput()
	cancel()
	if err != nil {
		pr.Problems = append(pr.Problems, fmt.Sprintf("%s --version failed: %s", strings.Join(cfg.Command, " "), describeOutput(string(out))))
		return pr
	}
	pr.PackageVersion = versionRE.FindString(string(out))
	if pr.PackageVersion != "" && pr.PackageVersion != PinnedVersion {
		pr.Problems = append(pr.Problems, fmt.Sprintf("Playwright MCP %s is installed; messh was written and tested against %s (tool schemas may differ)", pr.PackageVersion, PinnedVersion))
	}

	// Start the server in a scratch directory and speak MCP to it.
	tmp, err := os.MkdirTemp("", "messh-browser-probe-*")
	if err != nil {
		pr.Problems = append(pr.Problems, err.Error())
		return pr
	}
	defer os.RemoveAll(tmp)
	if cfg.Mode != ModeExtension && cfg.ProfileDir != "" {
		cfg.ProfileDir = "" // never touch the real profile from a probe
	}
	probeCtx, stop := context.WithCancel(ctx)
	defer stop()
	u := newUpstream(probeCtx, state.Paths{Root: tmp}, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	u.timeout.start = 3 * time.Minute
	sctx, cancel := context.WithTimeout(ctx, u.timeout.start)
	defer cancel()
	s, err := u.launchProcess(sctx, cfg)
	if err != nil {
		pr.Problems = append(pr.Problems, "Playwright MCP does not start: "+err.Error())
		return pr
	}
	defer stopSession(s)
	pr.ServerVersion = s.version
	if s.version != PinnedServerVersion {
		pr.Problems = append(pr.Problems, fmt.Sprintf("Playwright server %s reported; tested with %s", s.version, PinnedServerVersion))
	}
	for _, name := range allowlisted() {
		if name == "browser_evaluate" && !cfg.AllowScript {
			continue
		}
		if !s.tools[name] {
			pr.Missing = append(pr.Missing, name)
		}
	}
	sort.Strings(pr.Missing)
	if len(pr.Missing) > 0 {
		pr.Problems = append(pr.Problems, "this Playwright MCP lacks tools messh publishes: "+strings.Join(pr.Missing, ", "))
	}
	return pr
}
