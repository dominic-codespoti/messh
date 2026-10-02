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

// skillDirFor returns the default skills directory for a harness: omp keeps
// native user skills under ~/.omp/agent/skills, pi under ~/.pi/agent/skills,
// and "agents" is the plain Agent Skills layout under ~/.agents/skills (also
// read by pi). A named omp profile or PI_CODING_AGENT_DIR relocates the omp
// agent directory.
func skillDirFor(harness string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch harness {
	case "omp":
		if agentDir := os.Getenv("PI_CODING_AGENT_DIR"); agentDir != "" {
			return filepath.Join(agentDir, "skills"), nil
		}
		return filepath.Join(home, ".omp", "agent", "skills"), nil
	case "pi":
		return filepath.Join(home, ".pi", "agent", "skills"), nil
	case "agents":
		return filepath.Join(home, ".agents", "skills"), nil
	}
	return "", usageErrorf("unknown harness %q", harness)
}

func init() {
	add(&Command{
		Name:    "skill show",
		Summary: "print the messh agent skill (SKILL.md)",
		Output:  `{name, content}`,
		Examples: []string{
			"messh skill show",
			"messh skill show --json",
		},
		Run: func(c *Context) error {
			v := map[string]string{"name": skillName, "content": skillMarkdown}
			return c.Emit(v, func(w io.Writer) { fmt.Fprint(w, skillMarkdown) })
		},
	})
	add(&Command{
		Name:     "skill install",
		Summary:  "install the messh agent skill into a harness skills directory",
		Examples: []string{"messh skill install --for pi", "messh skill install --dir DIR"},
		Help: "Writes <dir>/messh/SKILL.md, creating directories as needed. Choose either --for HARNESS or --dir DIR; there is no default target.\n" +
			"The default <dir> depends on --for:\n" +
			"omp: ~/.omp/agent/skills (native omp user skills)\n" +
			"pi: ~/.pi/agent/skills\n" +
			"agents: ~/.agents/skills (plain Agent Skills layout, also read by pi)",
		Output:  `{path, written, unchanged}`,
		Mutates: true,
		Flags: func(fs *flag.FlagSet) {
			choiceFlag(fs, "for", "", "harness skills directory to use in `HARNESS`", "omp", "pi", "agents")
			fs.String("dir", "", "skills directory in `DIR` (overrides --for)")
			fs.Bool("force", false, "overwrite an existing SKILL.md with different content")
		},
		Run: func(c *Context) error {
			dir := c.String("dir")
			if dir == "" {
				if !c.Set("for") {
					return usageErrorf("choose a target with --for HARNESS or --dir DIR")
				}
				var err error
				dir, err = skillDirFor(c.String("for"))
				if err != nil {
					return err
				}
			} else if dir == "~" || strings.HasPrefix(dir, "~/") || strings.HasPrefix(dir, `~\`) {
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
