package main

import (
	"context"
	"flag"
	"time"

	"messh/internal/netcheck"
)

func init() {
	add(&Command{
		Name:    "firewall allow",
		Summary: "let peers reach TCP/UDP 7519 from the local subnet (Windows: one UAC prompt; Linux: prints sudo commands)",
		Help: "Deletes the block rules Windows made for messh.exe and adds two inbound rules for this program only, " +
			"`messh (mesh TCP 7519)` and `messh (discovery UDP 7519)`, on the Private and Domain profiles with remote " +
			"addresses limited to LocalSubnet. --private marks that network Private (only for your own home or office " +
			"network; Public stays right for cafes and hotels). Afterwards it re-runs the doctor's interface and firewall " +
			"checks. On Linux it only prints the commands for the detected firewall and never runs sudo itself.",
		Flags: func(fs *flag.FlagSet) {
			fs.String("private", "", "set this network (interface alias) to the Private category in `NETWORK_ALIAS`")
			fs.Bool("dry-run", false, "print the commands without running them")
		},
		Output:  `{dry_run, commands[], applied, checks[...]}`,
		Mutates: true,
		Person:  "on Windows without --dry-run: approve the Windows UAC prompt",
		Waits:   "on Windows without --dry-run: until the elevated step finishes",
		Examples: []string{
			`messh firewall allow --dry-run`,
			`messh firewall allow --private "WiFi 2"`,
		},
		Run: func(c *Context) error {
			paths, err := c.Paths()
			if err != nil {
				return err
			}
			// Long enough for the person to answer the UAC prompt.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			return firewallAllowRun(c, ctx, paths)
		},
	})
	add(&Command{
		Name:    "firewall remove",
		Summary: "delete the rules messh firewall allow added",
		Help:    "Deletes messh's inbound allow rules again. On Linux it only prints the commands and never runs sudo itself.",
		Flags: func(fs *flag.FlagSet) {
			fs.Bool("dry-run", false, "print the commands without running them")
		},
		Output:  `{dry_run, commands[], applied, checks[...]}`,
		Mutates: true,
		Person:  "on Windows without --dry-run: approve the Windows UAC prompt",
		Waits:   "on Windows without --dry-run: until the elevated step finishes",
		Examples: []string{
			"messh firewall remove --dry-run",
			"messh firewall remove",
		},
		Run: func(c *Context) error {
			paths, err := c.Paths()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			return firewallRemoveRun(c, ctx, paths)
		},
	})
}

// firewallResult is the --json shape of both firewall commands.
type firewallResult struct {
	DryRun   bool             `json:"dry_run"`
	Commands []string         `json:"commands"`
	Applied  bool             `json:"applied"`
	Checks   []netcheck.Check `json:"checks,omitempty"`
}
