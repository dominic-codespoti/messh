package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"messh/internal/buildinfo"
)

// addContractFake registers a temporary command for one test and removes it
// afterwards, so contract tests never depend on (or disturb) real commands.
func addContractFake(t *testing.T, c *Command) {
	t.Helper()
	if _, dup := registry[c.Name]; dup {
		t.Fatalf("fake command %q collides with a real command", c.Name)
	}
	add(c)
	t.Cleanup(func() { delete(registry, c.Name) })
}

func contractRunErr(t *testing.T, stderr *bytes.Buffer) (kind, hint string) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(stderr.Bytes(), &doc); err != nil {
		t.Fatalf("stderr is not JSON: %v\n%s", err, stderr.String())
	}
	errObj, ok := doc["error"].(map[string]any)
	if !ok {
		t.Fatalf("stderr JSON has no error object: %s", stderr.String())
	}
	kind, _ = errObj["kind"].(string)
	hint, _ = errObj["hint"].(string)
	return kind, hint
}

func TestVersionReportsBuildWithoutNodeOrState(t *testing.T) {
	previousVersion, previousCommit, previousChannel, previousBuild := buildinfo.Version, buildinfo.Commit, buildinfo.Channel, buildinfo.Build
	t.Cleanup(func() {
		buildinfo.Version, buildinfo.Commit, buildinfo.Channel, buildinfo.Build = previousVersion, previousCommit, previousChannel, previousBuild
	})
	want := buildinfo.Info{
		Version: "0.1.0-main.9007199254740993+0123456789ab",
		Commit:  "0123456789abcdef0123456789abcdef01234567",
		Channel: "main",
		Build:   9007199254740993,
	}
	buildinfo.Version, buildinfo.Commit, buildinfo.Channel, buildinfo.Build = want.Version, want.Commit, want.Channel, "9007199254740993"
	statePath := filepath.Join(t.TempDir(), "absent-state")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"version", "--json", "--state", statePath}, strings.NewReader(""), false, &stdout, &stderr); code != exitOK {
		t.Fatalf("version exit = %d: %s", code, stderr.String())
	}
	var got buildinfo.Info
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("version JSON does not contain a flat build identity with numeric build: %v", err)
	}
	if got != want {
		t.Fatalf("version identity = %+v, want %+v", got, want)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"describe", "--json", "--state", statePath}, strings.NewReader(""), false, &stdout, &stderr); code != exitOK {
		t.Fatalf("describe exit = %d: %s", code, stderr.String())
	}
	var doc ocDoc
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Info.Version != want.Version {
		t.Fatalf("describe version = %q, want %q", doc.Info.Version, want.Version)
	}
	if _, err := os.Stat(statePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("metadata commands touched state: %v", err)
	}
}

func TestContractUnknownCommand(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"bogus-command-xyz", "--json"}, strings.NewReader(""), false, &stdout, &stderr)
	if code != exitUsage {
		t.Fatalf("unknown command exit = %d, want %d", code, exitUsage)
	}
	if kind, _ := contractRunErr(t, &stderr); kind != "usage" {
		t.Fatalf("error kind = %q, want %q", kind, "usage")
	}
}

func TestContractMissingArg(t *testing.T) {
	addContractFake(t, &Command{
		Name:     "contract req",
		Summary:  "fake command with a required arg",
		Examples: []string{"messh contract req NAME"},
		Args:     []Arg{{Name: "NAME", Help: "a name"}},
		Run:      func(c *Context) error { return nil },
	})
	var stdout, stderr bytes.Buffer
	if code := run([]string{"contract", "req"}, strings.NewReader(""), false, &stdout, &stderr); code != exitUsage {
		t.Fatalf("missing arg exit = %d, want %d", code, exitUsage)
	}
}

func TestPairCodeRequired(t *testing.T) {
	for _, verb := range []string{"approve", "confirm"} {
		for _, flags := range [][]string{nil, {"--code", ""}, {"--code", "   "}} {
			t.Run(verb+fmt.Sprint(flags), func(t *testing.T) {
				var stdout, stderr bytes.Buffer
				args := append([]string{"pair", verb, "request-id", "--state", t.TempDir(), "--json"}, flags...)
				if code := run(args, strings.NewReader(""), false, &stdout, &stderr); code != exitUsage {
					t.Fatalf("exit = %d, want usage before looking for a node: %s", code, stderr.String())
				}
				if kind, _ := contractRunErr(t, &stderr); kind != "usage" {
					t.Fatalf("kind = %q, want usage", kind)
				}
			})
		}
	}
}

func TestContractFlagPositions(t *testing.T) {
	var gotArgs []string
	var gotOpt string
	addContractFake(t, &Command{
		Name:     "contract flags",
		Summary:  "fake command with one flag and one arg",
		Examples: []string{"messh contract flags one --opt x"},
		Args:     []Arg{{Name: "FIRST", Help: "first positional"}},
		Flags:    func(fs *flag.FlagSet) { fs.String("opt", "", "an `OPT`") },
		Run: func(c *Context) error {
			gotArgs = append([]string(nil), c.Args...)
			gotOpt = c.String("opt")
			return nil
		},
	})
	for _, args := range [][]string{
		{"contract", "flags", "--opt", "x", "one"},
		{"contract", "flags", "one", "--opt", "x"},
		{"contract", "flags", "one", "--opt=x"},
		{"contract", "flags", "--opt=x", "one"},
	} {
		gotArgs, gotOpt = nil, ""
		var stdout, stderr bytes.Buffer
		if code := run(args, strings.NewReader(""), false, &stdout, &stderr); code != exitOK {
			t.Fatalf("run(%q) exit = %d, want 0 (stderr: %s)", args, code, stderr.String())
		}
		if len(gotArgs) != 1 || gotArgs[0] != "one" || gotOpt != "x" {
			t.Fatalf("run(%q) parsed args=%q opt=%q, want [one] x", args, gotArgs, gotOpt)
		}
	}
}

func TestContractJSONOutput(t *testing.T) {
	addContractFake(t, &Command{
		Name:     "contract js",
		Summary:  "fake command that emits",
		Examples: []string{"messh contract js --json"},
		Run: func(c *Context) error {
			return c.Emit(map[string]string{"hello": "world"}, func(w io.Writer) {
				fmt.Fprintln(w, "hello world")
			})
		},
	})
	var stdout, stderr bytes.Buffer
	if code := run([]string{"contract", "js", "--json"}, strings.NewReader(""), false, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	var v map[string]string
	if err := json.Unmarshal(stdout.Bytes(), &v); err != nil {
		t.Fatalf("--json output is not valid JSON: %v\n%s", err, stdout.String())
	}
	if v["hello"] != "world" {
		t.Fatalf("--json output = %v, want hello=world", v)
	}
}

func TestContractHelp(t *testing.T) {
	addContractFake(t, &Command{
		Name:     "contract helpme",
		Summary:  "fake command for -h",
		Examples: []string{"messh contract helpme"},
		Run:      func(c *Context) error { return nil },
	})
	var stdout, stderr bytes.Buffer
	if code := run([]string{"contract", "helpme", "-h"}, strings.NewReader(""), false, &stdout, &stderr); code != exitOK {
		t.Fatalf("-h exit = %d, want 0", code)
	}
	if stdout.Len() == 0 {
		t.Fatal("-h printed nothing")
	}
}

func TestContractNeedsPerson(t *testing.T) {
	addContractFake(t, &Command{
		Name:     "contract person",
		Summary:  "fake command that needs a person",
		Examples: []string{"messh contract person"},
		Run: func(c *Context) error {
			return needsPerson("the owner must click approve", "run `messh approvals`")
		},
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"contract", "person", "--json"}, strings.NewReader(""), false, &stdout, &stderr)
	if code != exitPerson {
		t.Fatalf("needsPerson exit = %d, want %d", code, exitPerson)
	}
	kind, hint := contractRunErr(t, &stderr)
	if kind != "needs_person" {
		t.Fatalf("error kind = %q, want needs_person", kind)
	}
	if !strings.Contains(hint, "messh approvals") {
		t.Fatalf("hint = %q, want it to name the next command", hint)
	}
}

func TestContractReadPipedTTY(t *testing.T) {
	pr, pw := io.Pipe()
	defer pw.Close() // never written: the command must refuse before reading
	addContractFake(t, &Command{
		Name:     "contract piped",
		Summary:  "fake command that reads a piped secret",
		Examples: []string{"messh contract piped"},
		Run: func(c *Context) error {
			_, err := c.ReadPiped("a secret")
			return err
		},
	})
	var stdout, stderr bytes.Buffer
	if code := run([]string{"contract", "piped"}, pr, true, &stdout, &stderr); code != exitUsage {
		t.Fatalf("ReadPiped on a terminal exit = %d, want %d", code, exitUsage)
	}
}

func TestContractReadPipedValue(t *testing.T) {
	var got string
	addContractFake(t, &Command{
		Name:     "contract piped",
		Summary:  "fake command that reads a piped secret",
		Examples: []string{"messh contract piped"},
		Run: func(c *Context) error {
			s, err := c.ReadPiped("a secret")
			got = s
			return err
		},
	})
	var stdout, stderr bytes.Buffer
	if code := run([]string{"contract", "piped"}, strings.NewReader("s3cret\n"), false, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	if got != "s3cret" {
		t.Fatalf("ReadPiped = %q, want %q", got, "s3cret")
	}
}

func TestContractDescribeFlags(t *testing.T) {
	addContractFake(t, &Command{
		Name:     "specgroup verb",
		Summary:  "fake command with every flag type",
		Examples: []string{"messh specgroup verb"},
		Flags: func(fs *flag.FlagSet) {
			fs.String("name", "def", "a `NAME`")
			fs.Bool("verbose", false, "be loud")
			fs.Int("count", 3, "how many `N`")
			fs.Duration("wait", time.Second, "wait up to `DURATION`")
			listFlag(fs, "tag", "a `TAG`")
			choiceFlag(fs, "mode", "a", "pick `MODE`", "a", "b")
		},
		Run: func(c *Context) error { return nil },
	})
	var stdout, stderr bytes.Buffer
	if code := run([]string{"describe", "specgroup", "--json"}, strings.NewReader(""), false, &stdout, &stderr); code != exitOK {
		t.Fatalf("describe exit = %d, want 0 (stderr: %s)", code, stderr.String())
	}
	var doc ocDoc
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("describe --json is not valid JSON: %v", err)
	}
	var codes []int
	for _, e := range doc.Command.ExitCodes {
		codes = append(codes, e.Code)
	}
	for _, want := range []int{0, 1, 2, 3, 4} {
		found := false
		for _, c := range codes {
			found = found || c == want
		}
		if !found {
			t.Fatalf("root exitCodes = %v, want it to include %d", codes, want)
		}
	}
	var group *ocCommand
	for i := range doc.Command.Commands {
		if doc.Command.Commands[i].Name == "specgroup" {
			group = &doc.Command.Commands[i]
		}
	}
	if group == nil {
		t.Fatalf("no specgroup group; top-level = %v", contractCmdNames(doc.Command.Commands))
	}
	var leaf *ocCommand
	for i := range group.Commands {
		if group.Commands[i].Name == "verb" {
			leaf = &group.Commands[i]
		}
	}
	if leaf == nil {
		t.Fatalf("specgroup verb not nested under specgroup; got %v", contractCmdNames(group.Commands))
	}
	if leaf.Interactive {
		t.Fatal("interactive = true, want false: no command waits for typed input")
	}
	opts := map[string]ocOption{}
	for _, o := range leaf.Options {
		opts[o.Name] = o
	}
	meta := func(o ocOption) map[string]any {
		m := map[string]any{}
		for _, e := range o.Metadata {
			m[e.Name] = e.Value
		}
		return m
	}
	check := func(name, typ string, wantDefault any, wantRepeatable bool, wantValues []string) {
		t.Helper()
		o, ok := opts["--"+name]
		if !ok {
			t.Fatalf("option --%s missing; got %v", name, contractOptNames(leaf.Options))
		}
		m := meta(o)
		if m["type"] != typ {
			t.Errorf("--%s type = %v, want %s", name, m["type"], typ)
		}
		if wantDefault != nil && fmt.Sprint(m["default"]) != fmt.Sprint(wantDefault) {
			t.Errorf("--%s default = %v, want %v", name, m["default"], wantDefault)
		}
		if rep, _ := m["repeatable"].(bool); rep != wantRepeatable {
			t.Errorf("--%s repeatable = %v, want %v", name, rep, wantRepeatable)
		}
		var got []string
		for _, a := range o.Arguments {
			got = append(got, a.AcceptedValues...)
		}
		if fmt.Sprint(got) != fmt.Sprint(wantValues) {
			t.Errorf("--%s acceptedValues = %v, want %v", name, got, wantValues)
		}
	}
	check("name", "string", "def", false, nil)
	check("verbose", "bool", nil, false, nil)
	check("count", "int", "3", false, nil)
	check("wait", "duration", "1s", false, nil)
	check("tag", "string[]", nil, true, nil)
	check("mode", "string", "a", false, []string{"a", "b"})
	if len(opts["--verbose"].Arguments) != 0 {
		t.Errorf("--verbose takes arguments: %v", opts["--verbose"].Arguments)
	}
}

func contractCmdNames(cmds []ocCommand) []string {
	var out []string
	for _, c := range cmds {
		out = append(out, c.Name)
	}
	return out
}

func contractOptNames(opts []ocOption) []string {
	var out []string
	for _, o := range opts {
		out = append(out, o.Name)
	}
	return out
}

// Every visible command must be documented: a one-line summary and at least
// one example invocation.
func TestContractRegistryDocs(t *testing.T) {
	for _, cmd := range subcommands("") {
		if cmd.Summary == "" {
			t.Errorf("%q has no Summary", cmd.Name)
		}
		if len(cmd.Examples) == 0 {
			t.Errorf("%q has no Examples", cmd.Name)
		}
	}
}

// Every example must be a real invocation: it resolves to its own command
// and its flags and args parse. This catches help text drifting from the
// parser. Real commands are never executed.
func TestContractRegistryExamples(t *testing.T) {
	for _, cmd := range subcommands("") {
		for _, ex := range cmd.Examples {
			args, ok := contractSplitExample(t, ex)
			if !ok {
				continue
			}
			got, rest := lookup(args)
			if got != cmd {
				name := "<unknown>"
				if got != nil {
					name = got.Name
				}
				t.Errorf("%q of %q resolves to %s", ex, cmd.Name, name)
				continue
			}
			fs := flagSet(got)
			pos, err := parseArgs(fs, rest)
			if err != nil && !errors.Is(err, flag.ErrHelp) {
				t.Errorf("%q of %q does not parse: %v", ex, cmd.Name, err)
				continue
			}
			if err := checkArgs(got, pos); err != nil {
				t.Errorf("%q of %q has bad args: %v", ex, cmd.Name, err)
			}
		}
	}
}

// splitExample strips the leading "messh " and splits the rest the way a
// shell would for simple quoting; examples must not use anything fancier.
func contractSplitExample(t *testing.T, ex string) ([]string, bool) {
	t.Helper()
	s, ok := strings.CutPrefix(ex, "messh ")
	if !ok {
		t.Errorf("example %q does not start with \"messh \"", ex)
		return nil, false
	}
	var out []string
	var cur strings.Builder
	var quote rune
	inWord := false
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, inWord = r, true
		case r == ' ' || r == '\t':
			if inWord {
				out = append(out, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if quote != 0 {
		t.Errorf("example %q has an unbalanced quote", ex)
		return nil, false
	}
	if inWord {
		out = append(out, cur.String())
	}
	return out, true
}

// The embedded skill must satisfy the Agent Skills spec: name matches the
// skill directory and fits the constraints, description says what it is and
// when to use it.
func TestSkillFrontmatter(t *testing.T) {
	rest, ok := strings.CutPrefix(skillMarkdown, "---\n")
	if !ok {
		t.Fatal("SKILL.md does not start with YAML frontmatter")
	}
	front, _, ok := strings.Cut(rest, "\n---")
	if !ok {
		t.Fatal("SKILL.md frontmatter is not closed")
	}
	fields := map[string]string{}
	for _, line := range strings.Split(front, "\n") {
		if name, v, ok := strings.Cut(line, ":"); ok && !strings.Contains(name, " ") {
			fields[strings.TrimSpace(name)] = strings.TrimSpace(v)
		}
	}
	name := fields["name"]
	if name != skillName {
		t.Errorf("frontmatter name = %q, want the skill directory %q", name, skillName)
	}
	if len(name) > 64 || strings.Contains(name, "--") || strings.Trim(name, "-") != name {
		t.Errorf("frontmatter name = %q violates the Agent Skills naming rules", name)
	}
	for _, r := range name {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			t.Errorf("frontmatter name = %q has invalid character %q", name, r)
		}
	}
	desc := fields["description"]
	if desc == "" || len(desc) > 1024 {
		t.Errorf("description length = %d, want 1-1024 chars", len(desc))
	}
}

// The install defaults must be the real skill directories of each harness.
func TestSkillDefaultDirs(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	for harness, want := range map[string]string{
		"omp":    filepath.Join(tmp, ".omp", "agent", "skills"),
		"pi":     filepath.Join(tmp, ".pi", "agent", "skills"),
		"agents": filepath.Join(tmp, ".agents", "skills"),
	} {
		dir, err := skillDirFor(harness)
		if err != nil {
			t.Errorf("skillDirFor(%q) failed: %v", harness, err)
			continue
		}
		if dir != want {
			t.Errorf("skillDirFor(%q) = %q, want %q", harness, dir, want)
		}
	}
	if _, err := skillDirFor("bogus"); err == nil {
		t.Error("skillDirFor(bogus) succeeded, want an error")
	}
	t.Setenv("PI_CODING_AGENT_DIR", filepath.Join(tmp, "custom-agent"))
	if dir, err := skillDirFor("omp"); err != nil || dir != filepath.Join(tmp, "custom-agent", "skills") {
		t.Errorf("skillDirFor(omp) with PI_CODING_AGENT_DIR = %q, %v", dir, err)
	}
}
