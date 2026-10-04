package main

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"messh/internal/node"
	"messh/internal/state"
)

func init() {
	add(&Command{
		Name:    "node config",
		Summary: "show the stored node name and listen addresses",
		Help: "Shows the stored node configuration: the device name and the " +
			"mesh listen address plus the loopback-only local address. Empty " +
			"addresses mean the built-in defaults (" + node.DefaultMeshAddr + " and " +
			node.DefaultLocalAddr + "); `messh node` uses a flag when given, " +
			"otherwise the stored address, otherwise the default. " +
			"Changing the addresses needs a node restart to take effect.",
		Output: `{name, listen, local}`,
		Examples: []string{
			"messh node config",
			"messh node config --json",
		},
		Run: runNodeConfigGet,
	})
	add(&Command{
		Name:    "node config get",
		Summary: "show the stored node name and listen addresses",
		Help: "Shows the stored node configuration: the device name and the " +
			"mesh listen address plus the loopback-only local address. Empty " +
			"addresses mean the built-in defaults (" + node.DefaultMeshAddr + " and " +
			node.DefaultLocalAddr + "); `messh node` uses a flag when given, " +
			"otherwise the stored address, otherwise the default. " +
			"Changing the addresses needs a node restart to take effect.",
		Output: `{name, listen, local}`,
		Examples: []string{
			"messh node config get",
			"messh node config get --json",
		},
		Run: runNodeConfigGet,
	})
	add(&Command{
		Name:    "node config set",
		Summary: "store the node name and listen addresses (takes effect on restart)",
		Help: "Stores the node name and listen addresses. Only the supplied " +
			"flags change: --name renames the device, --listen sets the mesh " +
			"address peers connect to, --local sets the loopback-only address " +
			"for agents and the CLI. Addresses are validated before anything " +
			"is saved; --local must be a literal loopback IP, never a hostname. " +
			"Nothing restarts: restart the node for new addresses to take effect.",
		Flags: func(fs *flag.FlagSet) {
			fs.String("name", "", "device name in `NAME` (default: unchanged)")
			fs.String("listen", "", "address peers connect to in `ADDR` (default: unchanged)")
			fs.String("local", "", "loopback address for agents and the CLI in `ADDR` (default: unchanged)")
		},
		Output:  `{name, listen, local, restart_needed}`,
		Mutates: true,
		Examples: []string{
			"messh node config set --name desktop",
			"messh node config set --listen :7521 --local 127.0.0.1:7522",
		},
		Run: runNodeConfigSet,
	})
}

func runNodeConfigGet(c *Context) error {
	paths, err := c.Paths()
	if err != nil {
		return err
	}
	cfg, err := paths.LoadConfig()
	if err != nil {
		return err
	}
	return c.Emit(nodeConfigView(cfg), func(w io.Writer) {
		writeNodeConfig(w, cfg)
	})
}

func runNodeConfigSet(c *Context) error {
	paths, err := c.Paths()
	if err != nil {
		return err
	}
	cfg, err := paths.LoadConfig()
	if err != nil {
		return err
	}
	if !c.Set("name") && !c.Set("listen") && !c.Set("local") {
		return usageErrorf("nothing to change: pass --name, --listen or --local")
	}
	changed := false
	if c.Set("name") {
		name := c.String("name")
		if !node.ValidName(name) {
			return usageErrorf("invalid device name %q: use 1-64 printable characters", name)
		}
		changed = changed || cfg.Name != name
		cfg.Name = name
	}
	if c.Set("listen") {
		listen := strings.TrimSpace(c.String("listen"))
		if err := state.ValidateListenAddress(listen); err != nil {
			return usageErrorf("%v", err)
		}
		changed = changed || cfg.Listen != listen
		cfg.Listen = listen
	}
	if c.Set("local") {
		local := strings.TrimSpace(c.String("local"))
		if err := state.ValidateLocalAddress(local); err != nil {
			return usageErrorf("%v", err)
		}
		changed = changed || cfg.Local != local
		cfg.Local = local
	}
	if err := paths.SaveConfig(cfg); err != nil {
		return err
	}
	view := nodeConfigView(cfg)
	view["restart_needed"] = changed
	return c.Emit(view, func(w io.Writer) {
		writeNodeConfig(w, cfg)
		if changed {
			fmt.Fprintln(w, "Restart the node for the change to take effect.")
		} else {
			fmt.Fprintln(w, "No change: already configured this way.")
		}
	})
}

func nodeConfigView(cfg state.Config) map[string]any {
	return map[string]any{"name": cfg.Name, "listen": cfg.Listen, "local": cfg.Local}
}

func writeNodeConfig(w io.Writer, cfg state.Config) {
	name := cfg.Name
	if name == "" {
		name = "(unset: hostname on first start)"
	}
	listen := cfg.Listen
	if listen == "" {
		listen = node.DefaultMeshAddr + " (default)"
	}
	local := cfg.Local
	if local == "" {
		local = node.DefaultLocalAddr + " (default)"
	}
	fmt.Fprintf(w, "name     %s\nmesh     %s\nagents   %s\n", name, listen, local)
}
