package main

import (
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"messh/internal/node"
)

func init() {
	add(&Command{
		Name:    "describe",
		Args:    []Arg{{Name: "COMMAND", Optional: true, Repeated: true, Help: "limit the description to this command or group, e.g. agent add"}},
		Summary: "describe the CLI's commands, arguments, flags and exit codes (OpenCLI JSON with --json)",
		Help: "The JSON form is an OpenCLI 0.1 document (https://opencli.org). messh-specific facts are in\n" +
			"each command's metadata: json_output (what --json prints), mutates, needs_person, waits.",
		Output:   "an OpenCLI 0.1 document: {opencli, info, conventions, command{name, options, commands[...], exitCodes}}",
		Examples: []string{"messh describe --json", "messh describe agent add"},
		Run: func(c *Context) error {
			path := strings.Join(c.Args, " ")
			if path != "" && len(subcommands(path)) == 0 {
				return usageErrorf("no command %q (run `messh describe` for the list)", path)
			}
			return c.Emit(describeDoc(path), func(w io.Writer) {
				for _, cmd := range subcommands(path) {
					fmt.Fprintln(w, commandHelp(cmd))
				}
			})
		},
	})
	add(&Command{
		Name:     "version",
		Summary:  "print the messh version",
		Output:   `{"version": "0.1.0"}`,
		Examples: []string{"messh version --json"},
		Run: func(c *Context) error {
			return c.Emit(map[string]string{"version": node.Version}, func(w io.Writer) { fmt.Fprintln(w, "messh", node.Version) })
		},
	})
}

// OpenCLI 0.1 (https://opencli.org) document types.
type ocDoc struct {
	OpenCLI     string        `json:"opencli"`
	Info        ocInfo        `json:"info"`
	Conventions ocConventions `json:"conventions"`
	Command     ocCommand     `json:"command"`
}

type ocInfo struct {
	Title       string `json:"title"`
	Summary     string `json:"summary,omitempty"`
	Description string `json:"description,omitempty"`
	Version     string `json:"version"`
}

type ocConventions struct {
	GroupOptions            bool   `json:"groupOptions"`
	OptionArgumentSeparator string `json:"optionArgumentSeparator"`
}

type ocCommand struct {
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	Arguments   []ocArgument `json:"arguments,omitempty"`
	Options     []ocOption   `json:"options,omitempty"`
	Commands    []ocCommand  `json:"commands,omitempty"`
	ExitCodes   []ocExitCode `json:"exitCodes,omitempty"`
	Examples    []string     `json:"examples,omitempty"`
	Hidden      bool         `json:"hidden,omitempty"`
	Interactive bool         `json:"interactive"`
	Metadata    []ocMeta     `json:"metadata,omitempty"`
}

type ocArgument struct {
	Name           string   `json:"name"`
	Required       bool     `json:"required"`
	Arity          ocArity  `json:"arity"`
	AcceptedValues []string `json:"acceptedValues,omitempty"`
	Description    string   `json:"description,omitempty"`
}

type ocArity struct {
	Minimum int  `json:"minimum"`
	Maximum *int `json:"maximum"` // nil: unlimited
}

type ocOption struct {
	Name        string       `json:"name"`
	Required    bool         `json:"required"`
	Arguments   []ocArgument `json:"arguments,omitempty"`
	Description string       `json:"description,omitempty"`
	Recursive   bool         `json:"recursive,omitempty"`
	Metadata    []ocMeta     `json:"metadata,omitempty"`
}

type ocExitCode struct {
	Code        int    `json:"code"`
	Description string `json:"description"`
}

type ocMeta struct {
	Name  string `json:"name"`
	Value any    `json:"value"`
}

func one() *int { n := 1; return &n }

// describeDoc builds the OpenCLI document for every command under path
// ("" for all), nested by command words.
func describeDoc(path string) ocDoc {
	root := ocCommand{
		Name: "messh",
		Description: "Flags may come before, between or after arguments, as --flag value or --flag=value. " +
			"No command waits for typed input.",
		Options: []ocOption{
			{Name: "--state", Recursive: true, Description: stateUsage, Arguments: []ocArgument{{Name: "DIR", Required: true, Arity: ocArity{1, one()}}}},
			{Name: "--json", Recursive: true, Description: jsonUsage},
		},
	}
	for _, c := range exitCodes {
		root.ExitCodes = append(root.ExitCodes, ocExitCode{c.Code, c.Description})
	}
	for _, cmd := range subcommands(path) {
		insert(&root, strings.Fields(cmd.Name), describeCommand(cmd))
	}
	return ocDoc{
		OpenCLI:     "0.1",
		Info:        ocInfo{Title: "messh", Summary: "connect the devices on a LAN so agents on one can use the others", Version: node.Version},
		Conventions: ocConventions{GroupOptions: false, OptionArgumentSeparator: " "},
		Command:     root,
	}
}

// insert places leaf at words below parent, creating group commands on the way.
func insert(parent *ocCommand, words []string, leaf ocCommand) {
	if len(words) == 1 {
		for i := range parent.Commands {
			if parent.Commands[i].Name == words[0] {
				// A group that is also a command itself ("pair" and "pair accept").
				sub := parent.Commands[i].Commands
				parent.Commands[i] = leaf
				parent.Commands[i].Commands = sub
				return
			}
		}
		parent.Commands = append(parent.Commands, leaf)
		return
	}
	for i := range parent.Commands {
		if parent.Commands[i].Name == words[0] {
			insert(&parent.Commands[i], words[1:], leaf)
			return
		}
	}
	parent.Commands = append(parent.Commands, ocCommand{Name: words[0]})
	insert(&parent.Commands[len(parent.Commands)-1], words[1:], leaf)
}

func describeCommand(cmd *Command) ocCommand {
	words := strings.Fields(cmd.Name)
	oc := ocCommand{Name: words[len(words)-1], Description: cmd.Summary, Hidden: cmd.Hidden}
	if cmd.Help != "" {
		oc.Description += "\n\n" + cmd.Help
	}
	for _, a := range cmd.Args {
		ar := ocArity{Minimum: 1, Maximum: one()}
		if a.Optional {
			ar.Minimum = 0
		}
		if a.Repeated {
			ar.Maximum = nil
		}
		oc.Arguments = append(oc.Arguments, ocArgument{Name: a.Name, Required: !a.Optional, Arity: ar, AcceptedValues: a.Choices, Description: a.Help})
	}
	for _, f := range ownFlags(cmd) {
		name, u := flag.UnquoteUsage(f)
		opt := ocOption{Name: "--" + f.Name, Description: u, Required: slices.Contains(cmd.Required, f.Name)}
		typ := flagType(f)
		opt.Metadata = append(opt.Metadata, ocMeta{"type", typ})
		if f.DefValue != "" && f.DefValue != "[]" {
			opt.Metadata = append(opt.Metadata, ocMeta{"default", f.DefValue})
		}
		if typ != "bool" {
			if name == "" {
				name = "value"
			}
			arg := ocArgument{Name: strings.ToUpper(name), Required: true, Arity: ocArity{1, one()}}
			if c, ok := f.Value.(*choiceValue); ok {
				arg.AcceptedValues = c.choices
			}
			opt.Arguments = []ocArgument{arg}
		}
		if typ == "string[]" {
			opt.Metadata = append(opt.Metadata, ocMeta{"repeatable", true})
		}
		oc.Options = append(oc.Options, opt)
	}
	oc.Examples = cmd.Examples
	if cmd.Output != "" {
		oc.Metadata = append(oc.Metadata, ocMeta{"json_output", cmd.Output})
	}
	oc.Metadata = append(oc.Metadata, ocMeta{"mutates", cmd.Mutates})
	if cmd.Person != "" {
		oc.Metadata = append(oc.Metadata, ocMeta{"needs_person", cmd.Person})
	}
	if cmd.Waits != "" {
		oc.Metadata = append(oc.Metadata, ocMeta{"waits", cmd.Waits})
	}
	return oc
}

func flagType(f *flag.Flag) string {
	if t, ok := f.Value.(interface{ Type() string }); ok {
		return t.Type()
	}
	if b, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && b.IsBoolFlag() {
		return "bool"
	}
	if g, ok := f.Value.(flag.Getter); ok {
		switch g.Get().(type) {
		case int, int64, uint, uint64:
			return "int"
		case float64:
			return "float"
		case time.Duration:
			return "duration"
		}
	}
	return "string"
}
