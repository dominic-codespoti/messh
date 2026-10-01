package browser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"messh/internal/state"
)

func TestCommandAcceptsStringOrArray(t *testing.T) {
	var c Config
	if err := json.Unmarshal([]byte(`{"command": "npx -y @playwright/mcp@0.0.83"}`), &c); err != nil {
		t.Fatal(err)
	}
	if want := (Command{"npx", "-y", "@playwright/mcp@0.0.83"}); !reflect.DeepEqual(c.Command, want) {
		t.Errorf("string form = %v", c.Command)
	}
	if err := json.Unmarshal([]byte(`{"command": ["C:\\Program Files\\nodejs\\node.exe", "cli.js"]}`), &c); err != nil {
		t.Fatal(err)
	}
	if want := (Command{`C:\Program Files\nodejs\node.exe`, "cli.js"}); !reflect.DeepEqual(c.Command, want) {
		t.Errorf("array form = %v", c.Command)
	}
	got, err := SplitCommand(`"C:\Program Files\nodejs\npx.cmd" -y   "@playwright/mcp@latest"`)
	if err != nil || !reflect.DeepEqual(got, []string{`C:\Program Files\nodejs\npx.cmd`, "-y", "@playwright/mcp@latest"}) {
		t.Errorf("quoted split = %v, %v", got, err)
	}
	if _, err := SplitCommand(`npx "unterminated`); err == nil {
		t.Error("unterminated quote accepted")
	}
	if err := json.Unmarshal([]byte(`{"command": 5}`), &c); err == nil {
		t.Error("a number is not a command")
	}
	if d := (Config{}).Normalized().Command; !reflect.DeepEqual(d, DefaultCommand()) || d[2] != "@playwright/mcp@"+PinnedVersion {
		t.Errorf("default command = %v", d)
	}
}

func TestConfigDefaultsAndValidation(t *testing.T) {
	n := Config{Enabled: true}.Normalized()
	if n.Mode != ModeProfile || n.Channel != "chrome" || n.Headless || n.AllowScript {
		t.Errorf("defaults = %+v", n)
	}
	if (Config{}).Idle() != DefaultIdleTimeout || (Config{IdleTimeout: "0"}).Idle() != 0 || (Config{IdleTimeout: "90s"}).Idle() != 90*time.Second {
		t.Error("idle parsing")
	}
	for name, c := range map[string]Config{
		"mode":       {Mode: "stealth"},
		"channel":    {Channel: "firefox"},
		"idle":       {IdleTimeout: "soon"},
		"headless":   {Mode: ModeExtension, Headless: true},
		"allow list": {AllowOrigins: []string{"*"}},
		"deny list":  {DenyOrigins: []string{"https://x.test/path"}},
	} {
		if len(c.Validate()) == 0 {
			t.Errorf("%s: no problem reported", name)
		}
	}
	if c := (Config{Enabled: true, AllowOrigins: []string{"github.com"}}); !c.Active() {
		t.Error("valid config inactive")
	}
	if c := (Config{Enabled: true, Mode: "x"}); c.Active() {
		t.Error("invalid config active")
	}
}

func TestConfigFile(t *testing.T) {
	paths := state.Paths{Root: t.TempDir()}
	if c, err := LoadConfig(paths.BrowserFile()); err != nil || c.Enabled {
		t.Errorf("missing file: %+v %v", c, err)
	}
	in := Config{Enabled: true, Mode: ModeExtension, Channel: "msedge", ProfileDir: "Profile 1", DenyOrigins: []string{"bank.test"}, ExtensionToken: "t"}
	if err := SaveConfig(paths.BrowserFile(), in); err != nil {
		t.Fatal(err)
	}
	out, err := LoadConfig(paths.BrowserFile())
	if err != nil || !reflect.DeepEqual(in, out) {
		t.Errorf("round trip: %+v %v", out, err)
	}
	if runtime.GOOS != "windows" {
		fi, _ := os.Stat(paths.BrowserFile())
		if fi.Mode().Perm() != 0o600 {
			t.Errorf("mode %v", fi.Mode().Perm())
		}
	}
	if left, _ := filepath.Glob(filepath.Join(paths.Root, ".browser.json.*")); len(left) != 0 {
		t.Errorf("temp files left: %v", left)
	}
	if got := AgentProfileDir(paths, Config{}); got != filepath.Join(paths.Root, "browser", "profile") {
		t.Errorf("profile dir = %s", got)
	}
	if got := AgentProfileDir(paths, Config{ProfileDir: "/custom"}); got != "/custom" {
		t.Errorf("custom profile dir = %s", got)
	}
	// In extension mode profile_dir names a browser profile folder, not a path.
	if got := AgentProfileDir(paths, Config{Mode: ModeExtension, ProfileDir: "Profile 1"}); strings.Contains(got, "Profile 1") {
		t.Errorf("extension profile name used as a path: %s", got)
	}
}

func TestResultCleaningKeepsOnlySites(t *testing.T) {
	text := "### Ran Playwright code\n```js\nawait page.click();\n```\n" +
		"### Open tabs\n- 0: (current) [Secret Title](https://a.test/private?x=1)\n- 1: [Other](https://b.test/y)\n" +
		"### Page\n- Page URL: https://a.test/private?x=1\n- Page Title: Secret Title\n" +
		"### Events\n- New console entries: out/console-1.log#L1\n" +
		"### Snapshot\n- [Snapshot](out/page-1.yml)"
	c := &resultCleaner{paths: state.Paths{Root: t.TempDir()}, outDir: filepath.Join(t.TempDir(), "work", "out")}
	got := c.clean(text)
	for _, banned := range []string{"Ran Playwright", "Other", "/y", "console-1", "page-1.yml"} {
		if strings.Contains(got, banned) {
			t.Errorf("%q survived cleaning:\n%s", banned, got)
		}
	}
	for _, want := range []string{"- 0: (current) https://a.test", "- 1: https://b.test", "Page URL: https://a.test/private?x=1", "browser_snapshot"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing from:\n%s", want, got)
		}
	}
	// Paths outside the output folder are never read.
	if _, ok := c.insideOut(filepath.Join("..", "..", "secret.txt")); ok {
		t.Error("a path outside the output folder was accepted")
	}
	if _, ok := c.insideOut(os.Args[0]); ok {
		t.Error("an absolute path outside the output folder was accepted")
	}
}
