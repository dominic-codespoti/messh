package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"messh/internal/netcheck"
)

func init() {
	add(&Command{
		Name:    "doctor",
		Summary: "check this device's network setup (node, interfaces, firewall, multicast, peers) and print fixes; changes nothing",
		Help:    "Each check is ok, warn, fail or unknown with a one-line finding, followed by the Fix: commands. With the node stopped it still runs the OS checks.",
		Output:  `the netcheck report {host, os, program, checks[{id, status, finding, fix[]}]}`,
		Examples: []string{
			"messh doctor",
			"messh doctor --json",
		},
		Run: func(c *Context) error {
			paths, err := c.Paths()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			rep := netcheck.Run(ctx, paths)
			if err := c.Emit(rep, func(w io.Writer) {
				fmt.Fprintf(w, "messh doctor on %s (%s)", rep.Host, rep.OS)
				if rep.Program != "" {
					fmt.Fprintf(w, ", program %s", rep.Program)
				}
				fmt.Fprint(w, "\n\n")
				rep.Render(w)
			}); err != nil {
				return err
			}
			if n := rep.Count(netcheck.Fail); n > 0 {
				return fmt.Errorf("%d check(s) failed", n)
			}
			return nil
		},
	})
}
