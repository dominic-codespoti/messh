package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"messh/internal/browser"
	"messh/internal/control"
	"messh/internal/node"
	"messh/internal/state"
)

func init() {
	add(&Command{
		Name:    "browser setup",
		Summary: "let agents on other devices use a web browser on this one (asks you per website)",
		Help:    "Profile mode gives agents their own browser profile. Extension mode drives your everyday browser through the Playwright extension: install it, then pass its token with --token - (piped, never on the command line).",
		Flags: func(fs *flag.FlagSet) {
			choiceFlag(fs, "mode", "", "agent browser in `MODE`: profile (agent-only profile) or extension (your main browser)", "profile", "extension")
			choiceFlag(fs, "channel", "", "browser channel in `CHANNEL`: chrome or msedge", "chrome", "msedge")
			fs.String("executable", "", "path of a Chromium-based browser (Brave, Chromium) in `PATH` instead of Chrome/Edge")
			fs.Bool("headless", false, "hide the agent browser window (profile mode)")
			fs.String("token", "", "extension mode: `-` reads the Playwright extension token from standard input")
			fs.Bool("no-fetch", false, "do not download Playwright MCP now")
		},
		Output:  `{"enabled","mode","channel","headless","profile","command"} (never the extension token)`,
		Mutates: true,
		Examples: []string{
			"messh browser setup --mode profile",
			"messh browser setup --mode extension --token -",
		},
		Run: func(c *Context) error {
			paths, err := c.Paths()
			if err != nil {
				return err
			}
			return browserSetupRun(c, paths)
		},
	})
	add(&Command{
		Name:    "browser status",
		Summary: "config, Node, Playwright MCP, browser, and whether it starts (opens no window)",
		Output:  `{"configured","enabled","mode","channel","profile","command","problems[]","probe{...}","node":{browser.Status: enabled, mode, channel, headless, running, open_tabs[], ...}}`,
		Examples: []string{
			"messh browser status",
			"messh browser status --json",
		},
		Run: func(c *Context) error {
			paths, err := c.Paths()
			if err != nil {
				return err
			}
			return browserStatusRun(c, paths)
		},
	})
	add(&Command{
		Name:    "browser login",
		Summary: "open the agent browser profile to sign in to your sites (profile mode)",
		Output:  `{"opened","profile"}`,
		Mutates: true,
		Person:  "sign in to sites in the window that opens",
		Examples: []string{
			"messh browser login",
		},
		Run: func(c *Context) error {
			paths, err := c.Paths()
			if err != nil {
				return err
			}
			return browserLoginRun(c, paths)
		},
	})
	add(&Command{
		Name:    "browser stop",
		Summary: "stop the running browser now",
		Output:  `{"stopped"}`,
		Mutates: true,
		Examples: []string{
			"messh browser stop",
		},
		Run: func(c *Context) error {
			paths, err := c.Paths()
			if err != nil {
				return err
			}
			stopped, err := stopBrowser(paths)
			if err != nil {
				return err
			}
			return c.Emit(map[string]any{"stopped": stopped}, func(w io.Writer) {
				if stopped {
					fmt.Fprintln(w, "Stopped the browser.")
				} else {
					fmt.Fprintln(w, "No browser was running.")
				}
			})
		},
	})
	add(&Command{
		Name:    "browser reset-profile",
		Summary: "delete the agent browser profile (its logins and cookies)",
		Flags: func(fs *flag.FlagSet) {
			fs.Bool("yes", false, "delete without asking")
		},
		Output:  `{"profile","deleted"}`,
		Mutates: true,
		Examples: []string{
			"messh browser reset-profile --yes",
		},
		Run: func(c *Context) error {
			paths, err := c.Paths()
			if err != nil {
				return err
			}
			return browserResetRun(c, paths)
		},
	})
	add(&Command{
		Name:    "browser allow",
		Args:    []Arg{{Name: "ORIGIN", Help: "site agents may view without asking, e.g. github.com"}},
		Summary: "sites agents may view without asking (clicking and typing still ask)",
		Output:  `{"origin","list":"allow"}`,
		Mutates: true,
		Examples: []string{
			"messh browser allow github.com",
		},
		Run: func(c *Context) error {
			paths, err := c.Paths()
			if err != nil {
				return err
			}
			return browserListRun(c, paths, "allow", c.Args[0])
		},
	})
	add(&Command{
		Name:    "browser deny",
		Args:    []Arg{{Name: "ORIGIN", Help: "site that is never reachable, e.g. bank.example"}},
		Summary: "sites agents may never reach (no prompt)",
		Output:  `{"origin","list":"deny"}`,
		Mutates: true,
		Examples: []string{
			"messh browser deny bank.example",
		},
		Run: func(c *Context) error {
			paths, err := c.Paths()
			if err != nil {
				return err
			}
			return browserListRun(c, paths, "deny", c.Args[0])
		},
	})
	add(&Command{
		Name:    "browser rm",
		Args:    []Arg{{Name: "ORIGIN", Help: "entry to forget"}},
		Summary: "forget a site from the allow/deny lists",
		Output:  `{"origin","removed":true}`,
		Mutates: true,
		Examples: []string{
			"messh browser rm github.com",
		},
		Run: func(c *Context) error {
			paths, err := c.Paths()
			if err != nil {
				return err
			}
			return browserListRun(c, paths, "rm", c.Args[0])
		},
	})
	add(&Command{
		Name:    "browser ls",
		Summary: "list sites agents may view without asking / never reach",
		Output:  `{"allow_origins[]","deny_origins[]"}`,
		Examples: []string{
			"messh browser ls",
			"messh browser ls --json",
		},
		Run: func(c *Context) error {
			paths, err := c.Paths()
			if err != nil {
				return err
			}
			return browserLsRun(c, paths)
		},
	})
}

const extensionURL = "https://chromewebstore.google.com/detail/playwright-extension/mmlmfjhmonkocbjadbfplnigmagldckm"

func loadBrowserConfig(paths state.Paths) (browser.Config, error) {
	return browser.LoadConfig(paths.BrowserFile())
}

type browserSetupJSON struct {
	Enabled  bool     `json:"enabled"`
	Mode     string   `json:"mode"`
	Channel  string   `json:"channel"`
	Headless bool     `json:"headless"`
	Profile  string   `json:"profile,omitempty"`
	Command  []string `json:"command,omitempty"`
}

func browserSetupRun(c *Context, paths state.Paths) error {
	cfg, err := loadBrowserConfig(paths)
	if err != nil {
		return fmt.Errorf("%w (fix or remove the file, then run setup again)", err)
	}
	cfg.Enabled = true
	if c.String("mode") != "" {
		cfg.Mode = c.String("mode")
	}
	if cfg.Mode == "" {
		cfg.Mode = browser.ModeProfile
	}
	if c.String("channel") != "" {
		cfg.Channel = c.String("channel")
	}
	if c.String("executable") != "" {
		abs, err := filepath.Abs(c.String("executable"))
		if err != nil {
			return err
		}
		cfg.Executable = abs
	}
	if c.Set("headless") {
		cfg.Headless = c.Bool("headless")
	}
	if cfg.Mode == browser.ModeExtension && c.Set("headless") && c.Bool("headless") {
		return errors.New("--headless cannot be used with --mode extension: it drives your visible browser")
	}
	if cfg.Mode == browser.ModeExtension {
		cfg.Headless = false
	}
	if tok := c.String("token"); tok == "-" {
		token, err := c.ReadPiped("the extension token")
		if err != nil {
			return err
		}
		cfg.ExtensionToken = token
	} else if tok != "" {
		return errors.New(`pass --token - and pipe the token on standard input (a token on the command line ends up in your shell history)`)
	}
	if cfg.Channel == "" {
		cfg.Channel = pickChannel()
	}
	if problems := cfg.Validate(); len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}

	rt := browser.CheckRuntime(cfg.Command)
	if rt.Err != nil {
		return rt.Err
	}
	say := func(format string, a ...any) {
		if !c.JSON {
			fmt.Fprintf(c.Stdout, format+"\n", a...)
		}
	}
	say("Node.js %s, %s", orUnknown(rt.NodeVersion), rt.Command)
	if _, err := browser.PlanFor(cfg); err != nil {
		return err
	}
	if !c.Bool("no-fetch") {
		say("Fetching Playwright MCP %s (once; this can take a minute)...", browser.PinnedVersion)
		args := append(append([]string(nil), cfg.Normalized().Command[1:]...), "--version")
		out, err := exec.Command(rt.Command, args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("could not run %s: %s", strings.Join(cfg.Normalized().Command, " "), strings.TrimSpace(string(out)))
		}
		say("%s", strings.TrimSpace(string(out)))
	}
	if err := browser.SaveConfig(paths.BrowserFile(), cfg); err != nil {
		return err
	}
	say("Wrote %s (owner-only). A running node picks it up within a couple of seconds; no restart needed.\n", paths.BrowserFile())

	n := cfg.Normalized()
	res := browserSetupJSON{Enabled: true, Mode: n.Mode, Channel: n.Channel, Headless: n.Headless, Command: []string(n.Command)}
	var next string
	if n.Mode == browser.ModeProfile {
		res.Profile = browser.AgentProfileDir(paths, n)
		next = fmt.Sprintf(`Mode: agent profile (%s). Agents get their own browser profile at
  %s
that shares nothing with your everyday browser.

Next:
  messh browser login      sign in to the sites agents should use, once (optional)
  messh browser status     check that everything starts
`, channelName(n.Channel), res.Profile)
	} else {
		res.Profile = n.ProfileDir
		next = fmt.Sprintf(`Mode: your main browser (%s), through the Playwright extension. Agents see the tabs you
share and your logged-in sessions there.

Next:
  1. In %s, install the "Playwright Extension": %s
     (install it in the browser profile you are signed in with; if you use several
     profiles, set "profile_dir" in browser.json to that profile's folder name, the last
     part of "Profile Path" at chrome://version, e.g. "Profile 1")
  2. Keep that browser running; agents attach to the running instance.
  3. Without a token, every new agent session opens a page in your browser where you
     approve the connection and pick the tab to share. To skip it, open the extension's
     status page, copy its PLAYWRIGHT_MCP_EXTENSION_TOKEN, and run
         messh browser setup --mode extension --token -
     (pipe the token on standard input). With a token, only messh's per-website approval remains.
  4. messh browser status
`, channelName(n.Channel), channelName(n.Channel), extensionURL)
	}
	return c.Emit(res, func(w io.Writer) {
		fmt.Fprint(w, next)
		fmt.Fprint(w, `
Every website an agent touches asks you first on this device (messh approvals / the prompt):
view, then interact; choose "Always allow" per site, or list sites with
  messh browser allow github.com      view without asking (clicking and typing still ask)
  messh browser deny bank.example     never reachable
This is your approval, not a sandbox: a page you approve can still send what is on it anywhere.
Register messh in omp under a name other than browser or playwright (for example "messh");
omp silently drops MCP servers with those names while its built-in browser is on.
`)
	})
}

func channelName(ch string) string {
	if ch == "msedge" {
		return "Microsoft Edge"
	}
	return "Google Chrome"
}

func orUnknown(s string) string {
	if s == "" {
		return "(version unknown)"
	}
	return s
}

// pickChannel prefers Chrome, then Edge, then leaves the default.
func pickChannel() string {
	have := map[string]bool{}
	for _, b := range browser.FindBrowsers() {
		have[b.Family] = true
	}
	switch {
	case have["chrome"]:
		return "chrome"
	case have["msedge"]:
		return "msedge"
	}
	return "chrome"
}

type browserStatusJSON struct {
	ConfigFile   string          `json:"config_file"`
	Configured   bool            `json:"configured"`
	Enabled      bool            `json:"enabled"`
	Mode         string          `json:"mode,omitempty"`
	Channel      string          `json:"channel,omitempty"`
	Profile      string          `json:"profile,omitempty"`
	Headless     bool            `json:"headless,omitempty"`
	Idle         string          `json:"idle_timeout,omitempty"`
	Command      []string        `json:"command,omitempty"`
	AllowOrigins []string        `json:"allow_origins,omitempty"`
	DenyOrigins  []string        `json:"deny_origins,omitempty"`
	Problems     []string        `json:"problems,omitempty"`
	Probe        *browser.Probe  `json:"probe,omitempty"`
	Node         *browser.Status `json:"node,omitempty"`
}

func browserStatusRun(c *Context, paths state.Paths) error {
	cfg, err := loadBrowserConfig(paths)
	if err != nil {
		return err
	}
	res := browserStatusJSON{ConfigFile: paths.BrowserFile()}
	if _, err := os.Stat(paths.BrowserFile()); err != nil {
		return c.Emit(res, func(w io.Writer) {
			fmt.Fprintf(w, "Config: %s\n", paths.BrowserFile())
			fmt.Fprintln(w, "  not configured: run `messh browser setup`. No browser tools are published.")
		})
	}
	res.Configured = true
	n := cfg.Normalized()
	res.Enabled = cfg.Enabled
	res.Mode, res.Channel, res.Headless = n.Mode, n.Channel, n.Headless
	res.Command = []string(n.Command)
	res.AllowOrigins = append([]string{}, cfg.AllowOrigins...)
	res.DenyOrigins = append([]string{}, cfg.DenyOrigins...)
	res.Problems = append([]string{}, cfg.Validate()...)
	if n.Mode == browser.ModeProfile {
		res.Profile = browser.AgentProfileDir(paths, n)
	} else if n.ProfileDir != "" {
		res.Profile = n.ProfileDir
	}
	if idle := n.Idle(); idle == 0 {
		res.Idle = "never"
	} else {
		res.Idle = idle.String()
	}
	if !cfg.Enabled {
		return c.Emit(res, func(w io.Writer) {
			printBrowserConfig(w, paths, cfg, n)
			fmt.Fprintln(w, "\nDisabled: no browser tools are published. `messh browser setup` enables it.")
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	pr := browser.ProbeSetup(ctx, cfg)
	res.Probe = &pr
	if cl, err := control.Dial(paths); err == nil {
		var st browser.Status
		if err := cl.Do(context.Background(), http.MethodGet, "/v1/browser", nil, &st); err == nil {
			res.Node = &st
		}
	}
	return c.Emit(res, func(w io.Writer) {
		printBrowserConfig(w, paths, cfg, n)
		fmt.Fprintln(w, "\nChecking (starts Playwright MCP without opening a browser window)...")
		if pr.Command != "" {
			fmt.Fprintf(w, "  Node.js:      %s (%s)\n", orUnknown(pr.NodeVersion), pr.Command)
		}
		if pr.PackageVersion != "" {
			fmt.Fprintf(w, "  Playwright MCP: %s (tested with %s), server reports Playwright %s\n", pr.PackageVersion, browser.PinnedVersion, orUnknown(pr.ServerVersion))
		}
		if pr.Browser != "" {
			fmt.Fprintf(w, "  Browser:      %s\n", pr.Browser)
		}
		if n.Mode == browser.ModeExtension {
			fmt.Fprintf(w, "  Browser running: %v\n", pr.BrowserRunning)
		}
		if len(pr.Problems) == 0 {
			fmt.Fprintln(w, "  Playwright MCP starts and offers every tool messh publishes.")
		}
		for _, p := range pr.Problems {
			fmt.Fprintln(w, "  PROBLEM:", p)
		}
		if res.Node != nil {
			st := res.Node
			fmt.Fprintf(w, "\nRunning node: browser %s", map[bool]string{true: "is running", false: "is not running (starts on the first agent call)"}[st.Running])
			if st.Running && len(st.Tabs) > 0 {
				fmt.Fprintf(w, "; tabs: %s", strings.Join(st.Tabs, ", "))
			}
			fmt.Fprintln(w)
			if st.LastError != "" {
				fmt.Fprintln(w, "  last error:", st.LastError)
			}
		} else {
			fmt.Fprintln(w, "\nNo node is running here; start one with `messh node`.")
		}
	})
}

func printBrowserConfig(w io.Writer, paths state.Paths, cfg browser.Config, n browser.Config) {
	fmt.Fprintf(w, "Config: %s\n", paths.BrowserFile())
	fmt.Fprintf(w, "  enabled:      %v\n  mode:         %s\n  channel:      %s\n", cfg.Enabled, n.Mode, n.Channel)
	if n.Mode == browser.ModeProfile {
		fmt.Fprintf(w, "  profile:      %s\n  headless:     %v\n", browser.AgentProfileDir(paths, n), n.Headless)
	} else if n.ProfileDir != "" {
		fmt.Fprintf(w, "  profile dir:  %s\n", n.ProfileDir)
	}
	if n.Mode == browser.ModeExtension {
		fmt.Fprintf(w, "  extension token: %s\n", map[bool]string{true: "set (connections are automatic)", false: "not set (you approve each session in the browser)"}[cfg.ExtensionToken != ""])
	}
	idle := n.Idle().String()
	if n.Idle() == 0 {
		idle = "never"
	}
	fmt.Fprintf(w, "  idle stop:    %s\n  page scripts: %v (allow_script)\n  command:      %s\n", idle, cfg.AllowScript, strings.Join(n.Command, " "))
	for _, p := range cfg.Validate() {
		fmt.Fprintln(w, "  PROBLEM:", p)
	}
	printLists(w, cfg)
}

func printLists(w io.Writer, cfg browser.Config) {
	if len(cfg.AllowOrigins) > 0 {
		fmt.Fprintln(w, "  view without asking (allow_origins):", strings.Join(cfg.AllowOrigins, ", "))
	}
	if len(cfg.DenyOrigins) > 0 {
		fmt.Fprintln(w, "  never reachable (deny_origins):     ", strings.Join(cfg.DenyOrigins, ", "))
	}
}

// stopBrowser asks the running node to stop the browser. No node: nothing to stop.
func stopBrowser(paths state.Paths) (bool, error) {
	cl, err := control.Dial(paths)
	if err != nil {
		return false, nil
	}
	var out node.BrowserStopped
	if err := cl.Do(context.Background(), http.MethodPost, "/v1/browser/stop", nil, &out); err != nil {
		return false, err
	}
	return out.Stopped, nil
}

func browserLoginRun(c *Context, paths state.Paths) error {
	cfg, err := loadBrowserConfig(paths)
	if err != nil {
		return err
	}
	n := cfg.Normalized()
	if n.Mode != browser.ModeProfile {
		return errors.New("login is for the agent profile (mode profile); in extension mode you are already signed in through your own browser")
	}
	plan, err := browser.PlanFor(cfg)
	if err != nil {
		return err
	}
	exe := plan.Executable
	if exe == "" {
		exe = browser.BrowserPath(plan.Channel)
	}
	stopped, err := stopBrowser(paths)
	if err != nil {
		return err
	}
	say := func(format string, a ...any) {
		if !c.JSON {
			fmt.Fprintf(c.Stdout, format+"\n", a...)
		}
	}
	if stopped {
		say("Stopped the agent browser so its profile can be opened.")
		time.Sleep(time.Second) // let the profile lock go
	}
	profile := browser.AgentProfileDir(paths, n)
	if err := os.MkdirAll(profile, 0o700); err != nil {
		return err
	}
	// A normal browser window on the agent profile: no automation flags, so
	// sites see an ordinary browser while you sign in. It stays open after
	// this command returns; agents cannot use the profile while it is open.
	cmd := exec.Command(exe, "--user-data-dir="+profile, "--no-first-run", "--no-default-browser-check")
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", exe, err)
	}
	return c.Emit(map[string]any{"opened": true, "profile": profile}, func(w io.Writer) {
		fmt.Fprintf(w, `Opened %s with the agent profile:
  %s
Sign in to the sites agents should be able to use, then close the browser window.
Agents cannot use the profile while this window is open.
`, channelName(n.Channel), profile)
	})
}

func browserResetRun(c *Context, paths state.Paths) error {
	cfg, err := loadBrowserConfig(paths)
	if err != nil {
		return err
	}
	n := cfg.Normalized()
	profile := browser.AgentProfileDir(paths, n)
	if !c.Bool("yes") {
		return usageErrorf("would delete the agent browser profile %s (all its sign-ins, cookies and history); pass --yes to confirm", profile)
	}
	if n.Mode != browser.ModeProfile {
		return errors.New("reset-profile deletes the agent profile; in extension mode messh keeps no profile")
	}
	if _, err := os.Stat(profile); errors.Is(err, os.ErrNotExist) {
		return c.Emit(map[string]any{"profile": profile, "deleted": false}, func(w io.Writer) {
			fmt.Fprintln(w, "There is no agent profile to delete:", profile)
		})
	}
	if _, err := stopBrowser(paths); err != nil {
		return err
	}
	time.Sleep(time.Second)
	if err := os.RemoveAll(profile); err != nil {
		return fmt.Errorf("%w (is the browser still open? close it and retry)", err)
	}
	return c.Emit(map[string]any{"profile": profile, "deleted": true}, func(w io.Writer) {
		fmt.Fprintln(w, "Deleted. The next agent session starts with an empty profile.")
	})
}

// browserListRun edits allow_origins / deny_origins.
func browserListRun(c *Context, paths state.Paths, verb, origin string) error {
	cfg, err := loadBrowserConfig(paths)
	if err != nil {
		return err
	}
	list := ""
	switch verb {
	case "allow":
		if _, err := browser.ParsePattern(origin, false); err != nil {
			return err
		}
		if browser.MatchAny(cfg.DenyOrigins, true, mustOrigin(origin)) {
			return fmt.Errorf("%s is covered by deny_origins; remove that first with: messh browser rm <entry>", origin)
		}
		cfg.AllowOrigins = addUnique(cfg.AllowOrigins, origin)
		list = "allow"
	case "deny":
		if _, err := browser.ParsePattern(origin, true); err != nil {
			return err
		}
		cfg.DenyOrigins = addUnique(cfg.DenyOrigins, origin)
		cfg.AllowOrigins = slices.DeleteFunc(cfg.AllowOrigins, func(s string) bool { return strings.EqualFold(s, origin) })
		list = "deny"
	case "rm":
		before := len(cfg.AllowOrigins) + len(cfg.DenyOrigins)
		match := func(s string) bool { return strings.EqualFold(s, origin) }
		cfg.AllowOrigins = slices.DeleteFunc(cfg.AllowOrigins, match)
		cfg.DenyOrigins = slices.DeleteFunc(cfg.DenyOrigins, match)
		if len(cfg.AllowOrigins)+len(cfg.DenyOrigins) == before {
			return fmt.Errorf("%s is in neither list (messh browser ls)", origin)
		}
	}
	if err := browser.SaveConfig(paths.BrowserFile(), cfg); err != nil {
		return err
	}
	if verb == "rm" {
		return c.Emit(map[string]any{"origin": origin, "removed": true}, func(w io.Writer) {
			fmt.Fprintf(w, "Removed %s.\n", origin)
			if !cfg.Enabled {
				fmt.Fprintln(w, "Note: the browser is not enabled yet; run `messh browser setup`.")
			}
		})
	}
	return c.Emit(map[string]any{"origin": origin, "list": list}, func(w io.Writer) {
		if verb == "allow" {
			fmt.Fprintf(w, "Agents may now view %s without asking (clicking and typing there still ask).\n", origin)
		} else {
			fmt.Fprintf(w, "%s is now unreachable for agents.\n", origin)
		}
		if !cfg.Enabled {
			fmt.Fprintln(w, "Note: the browser is not enabled yet; run `messh browser setup`.")
		}
	})
}

// mustOrigin turns a list entry into a representative origin for a conflict
// check; entries that are not plain hosts check nothing.
func mustOrigin(entry string) browser.Origin {
	e := entry
	if !strings.Contains(e, "://") {
		e = "https://" + e
	}
	if strings.Contains(e, "*") {
		e = strings.Replace(e, "*.", "www.", 1)
	}
	o, _, err := browser.ParseURL(e)
	if err != nil {
		return browser.Origin{}
	}
	return o
}

func addUnique(list []string, v string) []string {
	for _, s := range list {
		if strings.EqualFold(s, v) {
			return list
		}
	}
	return append(list, v)
}

func browserLsRun(c *Context, paths state.Paths) error {
	cfg, err := loadBrowserConfig(paths)
	if err != nil {
		return err
	}
	res := map[string]any{
		"allow_origins": append([]string{}, cfg.AllowOrigins...),
		"deny_origins":  append([]string{}, cfg.DenyOrigins...),
	}
	return c.Emit(res, func(w io.Writer) {
		if len(cfg.AllowOrigins) == 0 && len(cfg.DenyOrigins) == 0 {
			fmt.Fprintln(w, "No sites listed. Agents ask you for every website.")
			return
		}
		for _, s := range cfg.AllowOrigins {
			fmt.Fprintf(w, "allow  %s\n", s)
		}
		for _, s := range cfg.DenyOrigins {
			fmt.Fprintf(w, "deny   %s\n", s)
		}
	})
}
