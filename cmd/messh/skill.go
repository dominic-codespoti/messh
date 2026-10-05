package main

import (
	_ "embed"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

//go:embed skills/messh/SKILL.md
var skillMarkdown string

const skillName = "messh"

func init() {
	add(&Command{
		Name:    "skill show",
		Summary: "print the messh agent skill (SKILL.md)",
		Output:  `{name, content}`,
		Examples: []string{
			"messh skill show",
		},
		Run: func(c *Context) error {
			return c.Emit(map[string]string{"name": skillName, "content": skillMarkdown}, func(w io.Writer) {
				io.WriteString(w, skillMarkdown)
			})
		},
	})
	add(&Command{
		Name:     "skill install",
		Summary:  "install the messh agent skill into a skills directory",
		Examples: []string{"messh skill install --dir DIR"},
		Help:     "Writes <dir>/messh/SKILL.md, creating directories as needed. An explicit destination is required.",
		Output:   `{path, written, unchanged}`,
		Mutates:  true,
		Flags: func(fs *flag.FlagSet) {
			fs.String("dir", "", "skills directory in `DIR` (required)")
			fs.Bool("force", false, "overwrite an existing SKILL.md with different content")
		},
		Run: func(c *Context) error {
			dir := c.String("dir")
			if dir == "" {
				return usageErrorf("choose a target with --dir DIR")
			}
			if dir == "~" || strings.HasPrefix(dir, "~/") || strings.HasPrefix(dir, `~\`) {
				home, err := os.UserHomeDir()
				if err != nil {
					return err
				}
				dir = filepath.Join(home, dir[1:])
			}
			target := filepath.Join(dir, skillName, "SKILL.md")
			if data, err := os.ReadFile(target); err == nil {
				if string(data) == skillMarkdown {
					return c.Emit(map[string]any{"path": target, "written": false, "unchanged": true}, func(w io.Writer) {
						fmt.Fprintf(w, "Skill is already installed at %s.\n", target)
					})
				}
				if !c.Bool("force") {
					return usageErrorf("%s already exists and differs; pass --force to overwrite it", target)
				}
			} else if !os.IsNotExist(err) {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(target, []byte(skillMarkdown), 0o644); err != nil {
				return err
			}
			return c.Emit(map[string]any{"path": target, "written": true, "unchanged": false}, func(w io.Writer) {
				fmt.Fprintf(w, "Installed the messh skill at %s.\n", target)
			})
		},
	})
}
