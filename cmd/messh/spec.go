package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"messh/internal/control"
	"messh/internal/state"
)

// Command is one invocation path of the CLI: a top-level word such as
// "status" or a verb under one such as "agent add". Each file declares its
// commands with add in an init function. Usage text, -h, and `messh
// describe` are generated from these declarations and from the flags the
// command really defines, so they cannot drift from what the parser accepts.
//
// No command may wait for typed input. A step that needs a person (comparing
// a pairing code, a UAC prompt, an approval click) is either a separate
// command that takes the answer as a flag, or fails with exitPerson and a
// hint naming the command to run.
type Command struct {
	Name     string                 // space-separated path, e.g. "agent add"
	Args     []Arg                  // positional arguments, in order
	Summary  string                 // one line, shown in usage
	Help     string                 // optional longer description for -h and describe
	Flags    func(fs *flag.FlagSet) // the command's own flags; --state and --json are added to every command
	Required []string               // names of flags (without --) that must be given
	Run      func(c *Context) error
	Output   string // shape of what --json prints on success, e.g. `[{name, id, online, last_seen, tools[]}]`
	Mutates  bool   // changes state on this or another device
	Person   string // non-empty when a person must act for the command to succeed, e.g. "a Windows UAC prompt"
	Waits    string // non-empty when it blocks (always bounded), e.g. "up to --wait for the device to answer"
	Hidden   bool
	Examples []string // full command lines starting with "messh "
}

// Arg is one positional argument.
type Arg struct {
	Name     string // upper case, e.g. "NAME"
	Help     string
	Optional bool
	Repeated bool // the last argument may repeat
	Choices  []string
}

// Context is what a command's Run receives.
type Context struct {
	Cmd    *Command
	Args   []string // positional arguments, already checked against Cmd.Args
	Flags  *flag.FlagSet
	JSON   bool
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader
	// StdinTTY reports whether stdin is a terminal. Commands never read a
	// terminal: a person at it would have to type, which an agent cannot.
	StdinTTY bool
	state    string
}

// Exit codes. They are part of the CLI contract and listed by describe.
const (
	exitOK      = 0
	exitFailure = 1
	exitUsage   = 2 // bad command, arguments or flags
	exitNoNode  = 3 // the command needs the running node and none answers
	exitPerson  = 4 // a person must act first (confirm a code, answer a prompt, approve elevation)
)

var exitCodes = []struct {
	Code        int
	Description string
}{
	{exitOK, "success"},
	{exitFailure, "the command failed"},
	{exitUsage, "invalid command, arguments or flags"},
	{exitNoNode, "no messh node is running for this state directory (start one with `messh node`)"},
	{exitPerson, "a person must act first; the error's hint names what to do"},
}

// cliError carries an exit code, a stable kind for --json errors, and an
// optional hint naming the next command to run.
type cliError struct {
	exit int
	kind string
	msg  string
	hint string
}

func (e *cliError) Error() string { return e.msg }

func usageErrorf(format string, a ...any) error {
	return &cliError{exit: exitUsage, kind: "usage", msg: fmt.Sprintf(format, a...)}
}

// needsPerson reports that a person must do something before the command can succeed.
func needsPerson(msg, hint string) error {
	return &cliError{exit: exitPerson, kind: "needs_person", msg: msg, hint: hint}
}

// withHint attaches the next command to run to an ordinary failure.
func withHint(err error, hint string) error {
	if err == nil {
		return nil
	}
	var ce *cliError
	if errors.As(err, &ce) {
		cp := *ce
		cp.hint = hint
		return &cp
	}
	return &cliError{exit: exitFailure, kind: "failure", msg: err.Error(), hint: hint}
}

var registry = map[string]*Command{}

// add registers a command; call it from an init function.
func add(c *Command) {
	if c.Name == "" || c.Run == nil {
		panic("messh: command without name or Run")
	}
	if _, dup := registry[c.Name]; dup {
		panic("messh: duplicate command " + c.Name)
	}
	registry[c.Name] = c
}

// Paths resolves the state directory from --state / $MESSH_STATE.
func (c *Context) Paths() (state.Paths, error) { return state.Resolve(c.state) }

// Node dials the running node for the state directory.
func (c *Context) Node() (*control.Client, state.Paths, error) {
	paths, err := c.Paths()
	if err != nil {
		return nil, paths, err
	}
	cl, err := control.Dial(paths)
	return cl, paths, err
}

// Emit prints v as one line of JSON with --json, otherwise calls text.
func (c *Context) Emit(v any, text func(w io.Writer)) error {
	if c.JSON {
		return json.NewEncoder(c.Stdout).Encode(v)
	}
	if text != nil {
		text(c.Stdout)
	}
	return nil
}

// ReadPiped reads a value piped on stdin (for secrets that must not appear
// on the command line). It refuses a terminal rather than waiting for typing.
func (c *Context) ReadPiped(what string) (string, error) {
	if c.StdinTTY {
		return "", usageErrorf("pipe %s on standard input (e.g. `... | messh %s`); messh never waits for typed input", what, c.Cmd.Name)
	}
	data, err := io.ReadAll(io.LimitReader(c.Stdin, 64<<10))
	if err != nil {
		return "", err
	}
	v := strings.TrimSpace(string(data))
	if v == "" {
		return "", usageErrorf("no %s on standard input", what)
	}
	return v, nil
}

func (c *Context) value(name string) any {
	f := c.Flags.Lookup(name)
	if f == nil {
		panic("messh: command " + c.Cmd.Name + " reads undefined flag --" + name)
	}
	if g, ok := f.Value.(flag.Getter); ok {
		return g.Get()
	}
	return f.Value.String()
}

func (c *Context) String(name string) string { v, _ := c.value(name).(string); return v }
func (c *Context) Bool(name string) bool     { v, _ := c.value(name).(bool); return v }
func (c *Context) Int(name string) int       { v, _ := c.value(name).(int); return v }
func (c *Context) Duration(name string) time.Duration {
	v, _ := c.value(name).(time.Duration)
	return v
}
func (c *Context) List(name string) []string { v, _ := c.value(name).([]string); return v }

// Set reports whether the flag was given on the command line.
func (c *Context) Set(name string) bool {
	set := false
	c.Flags.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

// listValue is a repeatable string flag: --tag a --tag b.
type listValue []string

func (l *listValue) String() string     { return strings.Join(*l, ",") }
func (l *listValue) Set(s string) error { *l = append(*l, s); return nil }
func (l *listValue) Get() any           { return []string(*l) }
func (l *listValue) Type() string       { return "string[]" }

// listFlag defines a repeatable string flag.
func listFlag(fs *flag.FlagSet, name, usage string) { fs.Var(&listValue{}, name, usage) }

// choiceValue is a string flag limited to fixed values.
type choiceValue struct {
	v       string
	choices []string
}

func (c *choiceValue) String() string { return c.v }
func (c *choiceValue) Get() any       { return c.v }
func (c *choiceValue) Type() string   { return "string" }
func (c *choiceValue) Set(s string) error {
	if !slices.Contains(c.choices, s) {
		return fmt.Errorf("must be one of %s", strings.Join(c.choices, ", "))
	}
	c.v = s
	return nil
}

// choiceFlag defines a string flag that only accepts the given values.
func choiceFlag(fs *flag.FlagSet, name, def, usage string, choices ...string) {
	fs.Var(&choiceValue{v: def, choices: choices}, name, usage)
}

const (
	stateUsage = "state directory (default $MESSH_STATE, else %LOCALAPPDATA%\\messh on Windows, ~/.local/state/messh elsewhere)"
	jsonUsage  = "print the result as one line of JSON on stdout; errors become {\"error\":{kind,message,hint}} on stderr"
)

// flagSet builds the parser for cmd: its own flags plus --state and --json.
func flagSet(cmd *Command) *flag.FlagSet {
	fs := flag.NewFlagSet("messh "+cmd.Name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if cmd.Flags != nil {
		cmd.Flags(fs)
	}
	fs.String("state", "", stateUsage)
	fs.Bool("json", false, jsonUsage)
	return fs
}

// lookup finds the command named by the leading words of args.
func lookup(args []string) (*Command, []string) {
	var words []string
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			break
		}
		words = append(words, a)
	}
	for n := len(words); n > 0; n-- {
		if c, ok := registry[strings.Join(words[:n], " ")]; ok {
			return c, args[n:]
		}
	}
	return nil, args
}

// subcommands lists the visible commands below prefix ("" for all).
func subcommands(prefix string) []*Command {
	var out []*Command
	for name, c := range registry {
		if c.Hidden {
			continue
		}
		if prefix == "" || name == prefix || strings.HasPrefix(name, prefix+" ") {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b *Command) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// parseArgs parses flags anywhere among the positional arguments.
func parseArgs(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func checkArgs(cmd *Command, pos []string) error {
	min, max := 0, len(cmd.Args)
	for _, a := range cmd.Args {
		if !a.Optional {
			min++
		}
	}
	if n := len(cmd.Args); n > 0 && cmd.Args[n-1].Repeated {
		max = -1
	}
	if len(pos) < min || (max >= 0 && len(pos) > max) {
		return usageErrorf("usage: %s", synopsis(cmd))
	}
	for i, v := range pos {
		a := cmd.Args[min0(i, len(cmd.Args)-1)]
		if len(a.Choices) > 0 && !slices.Contains(a.Choices, v) {
			return usageErrorf("%s must be one of %s", a.Name, strings.Join(a.Choices, ", "))
		}
	}
	return nil
}

func min0(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// run executes one CLI invocation and returns the exit code.
func run(args []string, stdin io.Reader, stdinTTY bool, stdout, stderr io.Writer) int {
	wantJSON := slices.Contains(args, "--json") || slices.Contains(args, "-json")
	fail := func(err error) int {
		ce := &cliError{exit: exitFailure, kind: "failure", msg: err.Error()}
		if !errors.As(err, &ce) && errors.Is(err, state.ErrNodeNotRunning) {
			ce = &cliError{exit: exitNoNode, kind: "node_not_running", msg: err.Error(), hint: "start it with `messh node` (or pass the right --state)"}
		}
		if wantJSON {
			json.NewEncoder(stderr).Encode(map[string]any{"error": map[string]string{"kind": ce.kind, "message": ce.msg, "hint": ce.hint}})
		} else {
			fmt.Fprintln(stderr, "messh:", ce.msg)
			if ce.hint != "" {
				fmt.Fprintln(stderr, "  next:", ce.hint)
			}
		}
		return ce.exit
	}

	if len(args) == 0 {
		fmt.Fprint(stderr, usage())
		return exitUsage
	}
	switch args[0] {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage())
		return exitOK
	case "--version":
		args = []string{"version"}
	}
	cmd, rest := lookup(args)
	if cmd == nil {
		if group := subcommands(args[0]); len(group) > 0 {
			names := make([]string, len(group))
			for i, c := range group {
				names[i] = "messh " + c.Name
			}
			return fail(usageErrorf("%q needs a subcommand: %s", args[0], strings.Join(names, ", ")))
		}
		return fail(usageErrorf("unknown command %q (run `messh help` or `messh describe --json`)", args[0]))
	}

	fs := flagSet(cmd)
	pos, err := parseArgs(fs, rest)
	if errors.Is(err, flag.ErrHelp) {
		if wantJSON {
			if err := json.NewEncoder(stdout).Encode(describeDoc(cmd.Name)); err != nil {
				return fail(err)
			}
		} else {
			fmt.Fprint(stdout, commandHelp(cmd))
		}
		return exitOK
	}
	if err != nil {
		return fail(usageErrorf("%v (usage: %s)", err, synopsis(cmd)))
	}
	if err := checkArgs(cmd, pos); err != nil {
		return fail(err)
	}
	for _, name := range cmd.Required {
		given := false
		fs.Visit(func(f *flag.Flag) { given = given || f.Name == name })
		if !given {
			return fail(usageErrorf("--%s is required (usage: %s)", name, synopsis(cmd)))
		}
	}
	ctx := &Context{
		Cmd: cmd, Args: pos, Flags: fs, JSON: fs.Lookup("json").Value.(flag.Getter).Get().(bool),
		Stdout: stdout, Stderr: stderr, Stdin: stdin, StdinTTY: stdinTTY,
		state: fs.Lookup("state").Value.String(),
	}
	wantJSON = ctx.JSON
	if err := cmd.Run(ctx); err != nil {
		return fail(err)
	}
	return exitOK
}

// ---- help text ----

func flagSynopsis(f *flag.Flag) string {
	if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
		return "[--" + f.Name + "]"
	}
	if c, ok := f.Value.(*choiceValue); ok {
		return "[--" + f.Name + " " + strings.Join(c.choices, "|") + "]"
	}
	name, _ := flag.UnquoteUsage(f)
	if name == "" {
		name = "value"
	}
	s := "[--" + f.Name + " " + strings.ToUpper(name) + "]"
	if _, ok := f.Value.(*listValue); ok {
		s += "..."
	}
	return s
}

func ownFlags(cmd *Command) []*flag.Flag {
	var out []*flag.Flag
	flagSet(cmd).VisitAll(func(f *flag.Flag) {
		if f.Name != "state" && f.Name != "json" {
			out = append(out, f)
		}
	})
	return out
}

func synopsis(cmd *Command) string {
	parts := []string{"messh", cmd.Name}
	for _, a := range cmd.Args {
		s := a.Name
		if len(a.Choices) > 0 {
			s = strings.Join(a.Choices, "|")
		}
		if a.Repeated {
			s += "..."
		}
		if a.Optional {
			s = "[" + s + "]"
		}
		parts = append(parts, s)
	}
	for _, f := range ownFlags(cmd) {
		s := flagSynopsis(f)
		if slices.Contains(cmd.Required, f.Name) {
			s = strings.TrimSuffix(strings.TrimPrefix(s, "["), "]")
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, " ")
}

func usage() string {
	var b strings.Builder
	b.WriteString("messh connects the devices on your LAN so agents on one can use the others.\n\nCommands:\n")
	for _, c := range subcommands("") {
		fmt.Fprintf(&b, "  %s\n      %s\n", synopsis(c), c.Summary)
	}
	b.WriteString("\nEvery command accepts --state DIR and --json, never waits for typed input, and\n" +
		"exits 0 ok, 1 failed, 2 usage, 3 no running node, 4 a person must act first.\n" +
		"`messh COMMAND -h` explains one command; `messh describe --json` describes them all.\n")
	return b.String()
}

func commandHelp(cmd *Command) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n\n  %s\n", synopsis(cmd), cmd.Summary)
	if cmd.Help != "" {
		fmt.Fprintf(&b, "\n%s\n", indent(cmd.Help, "  "))
	}
	if len(cmd.Args) > 0 {
		b.WriteString("\nArguments:\n")
		for _, a := range cmd.Args {
			fmt.Fprintf(&b, "  %-12s %s\n", a.Name, a.Help)
		}
	}
	b.WriteString("\nFlags:\n")
	flagSet(cmd).VisitAll(func(f *flag.Flag) {
		def := ""
		if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" && f.DefValue != "0s" {
			def = fmt.Sprintf(" (default %s)", f.DefValue)
		}
		_, u := flag.UnquoteUsage(f)
		fmt.Fprintf(&b, "  --%-14s %s%s\n", f.Name, u, def)
	})
	if cmd.Output != "" {
		fmt.Fprintf(&b, "\n--json prints: %s\n", cmd.Output)
	}
	if cmd.Mutates {
		b.WriteString("Changes state.\n")
	}
	if cmd.Person != "" {
		fmt.Fprintf(&b, "Needs a person: %s.\n", cmd.Person)
	}
	if cmd.Waits != "" {
		fmt.Fprintf(&b, "Blocks: %s.\n", cmd.Waits)
	}
	if len(cmd.Examples) > 0 {
		b.WriteString("\nExamples:\n")
		for _, e := range cmd.Examples {
			fmt.Fprintf(&b, "  %s\n", e)
		}
	}
	return b.String()
}

func indent(s, pre string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		if l != "" {
			lines[i] = pre + l
		}
	}
	return strings.Join(lines, "\n")
}
