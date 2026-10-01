package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"messh/internal/identity"
	"messh/internal/node"
)

func init() {
	add(&Command{
		Name:    "node",
		Summary: "run the local node (long-running server)",
		Help:    "Runs the messh node in the foreground until interrupted (Ctrl-C / SIGTERM).",
		Output:  `one startup object {id, name, mesh, local, state}`,
		Waits:   "runs until stopped",
		Flags: func(fs *flag.FlagSet) {
			fs.String("name", "", "device name (default: stored name, else hostname)")
			fs.String("listen", node.DefaultMeshAddr, "address peers connect to in `ADDR`")
			fs.String("local", node.DefaultLocalAddr, "loopback address for agents and the CLI in `ADDR`")
			fs.Bool("no-discovery", false, "do not announce or listen on the LAN")
			fs.String("discovery-iface", "", "comma-separated interface names for discovery (default: all suitable) in `NAMES`")
			fs.Bool("v", false, "debug logging")
		},
		Examples: []string{
			"messh node",
			"messh node --name desktop",
		},
		Run: func(c *Context) error {
			paths, err := c.Paths()
			if err != nil {
				return err
			}
			level := slog.LevelInfo
			if c.Bool("v") {
				level = slog.LevelDebug
			}
			log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

			var ifaceList []string
			for _, s := range strings.Split(c.String("discovery-iface"), ",") {
				if s = strings.TrimSpace(s); s != "" {
					ifaceList = append(ifaceList, s)
				}
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			n, err := node.Start(ctx, node.Options{
				Paths:           paths,
				Name:            c.String("name"),
				MeshAddr:        c.String("listen"),
				LocalAddr:       c.String("local"),
				Discovery:       !c.Bool("no-discovery"),
				DiscoveryIfaces: ifaceList,
				Logger:          log,
			})
			if err != nil {
				return err
			}
			startup := map[string]string{
				"id":    n.ID(),
				"name":  n.Name(),
				"mesh":  n.MeshAddr(),
				"local": n.LocalAddr(),
				"state": paths.Root,
			}
			if err := c.Emit(startup, func(w io.Writer) {}); err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "messh %s: %s (id %s)\n  peers connect to  %s\n  agents connect to http://%s/mcp\n  state             %s\n",
				node.Version, n.Name(), identity.Short(n.ID()), n.MeshAddr(), n.LocalAddr(), paths.Root)
			<-n.Done()
			return nil
		},
	})
}
