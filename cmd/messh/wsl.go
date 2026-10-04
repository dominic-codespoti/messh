package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"messh/internal/state"
)

// Shared cross-platform helpers for `wsl setup` / `wsl refresh`: command
// registration, flag parsing, runners, result shapes, and output writers.
// Plan/apply implementations live in wsl_setup.go (Windows) with unsupported
// stubs in wsl_other.go; `wsl status` is diagnostic-owned in wsl_status.go.

func init() {
	add(&Command{
		Name:    "wsl setup",
		Summary: "capture the WSL target and install its Windows startup and host route (Windows only; one UAC prompt for the machine route)",
		Help: "Captures the selected WSL target (distro, guest user, target identity, guest state, ports, " +
			"host interface, allowed laptop peers) into the Windows owner's wsl-target.json, installs the " +
			"owner-context logon task that boots the distro and starts its messh user service, then asks " +
			"Windows for administrator rights (one UAC prompt) to install the administrator-owned ProgramData " +
			"route script/config and the SYSTEM route-only task plus the portproxy/firewall route itself. " +
			"The portproxy forwards the Windows LAN mesh port to literal 127.0.0.1:same port (native WSL " +
			"localhost forwarder); the guest local API port is never forwarded or firewalled. The guest binary " +
			"and user service must already be installed; this command never installs into the guest, never " +
			"stores credentials, and never resets networking, shuts down, or resets the distribution. " +
			"Re-running setup re-validates, replaces only messh's own rows/tasks, and refuses unrelated collisions.",
		Flags: func(fs *flag.FlagSet) {
			wslSetupFlags(fs)
		},
		Output:  `{distro, user, name, id, guest_state, mesh_port, local_port, host_address, interface_index, allowed_peers[], tasks{logon, route}, route{proxy, rule, applied}, route_task_started}`,
		Mutates: true,
		Person:  "on Windows without --dry-run: approve the Windows UAC prompt for the machine route",
		Waits:   "on Windows without --dry-run: until the elevated route install finishes",
		Examples: []string{
			`messh wsl setup --distro Ubuntu --user dom --name dompc-wsl --id <WSL_ID> --guest-state /home/dom/.local/state/messh --mesh-port 7521 --local-port 7522 --interface-index 10 --peer 192.168.1.189`,
			`messh wsl setup --distro Ubuntu --user dom --name dompc-wsl --id <WSL_ID> --guest-state /home/dom/.local/state/messh --mesh-port 7521 --local-port 7522 --host-address 192.168.1.31 --peer 192.168.1.189 --dry-run`,
		},
		Run: runWSLSetup,
	})
	add(&Command{
		Name:    "wsl refresh",
		Summary: "re-apply the captured WSL host route after a DHCP move or reboot (Windows only; one UAC prompt for the machine route)",
		Help: "Re-reads the captured wsl-target.json, re-resolves the Windows LAN address on the selected " +
			"interface, triggers the secured SYSTEM route task when its config is current (refreshing the " +
			"owner-private metadata afterwards), and otherwise asks Windows for administrator rights (one UAC " +
			"prompt) to re-apply the portproxy/firewall rows. Never touches the guest, the distro, or the " +
			"Windows node; never reads executable route parameters from user-writable state. Refuses unrelated " +
			"route/rule/task collisions instead of overwriting them.",
		Flags: func(fs *flag.FlagSet) {
			fs.Bool("dry-run", false, "print what would change without touching tasks, routes, or the firewall")
			fs.Duration("timeout", 2*time.Minute, "give up after `DURATION`")
		},
		Output:  `{host_address, previous_host_address, stale, interface_index, route{proxy, rule, applied}, route_task_started}`,
		Mutates: true,
		Person:  "on Windows without --dry-run: approve the Windows UAC prompt when the route task cannot apply the route itself",
		Waits:   "on Windows without --dry-run: until the route task or the elevated route install finishes",
		Examples: []string{
			"messh wsl refresh",
			"messh wsl refresh --dry-run",
		},
		Run: runWSLRefresh,
	})
}

func wslSetupFlags(fs *flag.FlagSet) {
	fs.String("distro", "", "WSL distribution name exactly as `wsl --list --quiet` shows it in `NAME`")
	fs.String("user", "", "guest user owning the messh user service in `NAME`")
	fs.String("name", "", "messh target name of the WSL node in `NAME`")
	fs.String("id", "", "52-char device ID of the WSL node in `ID`")
	fs.String("guest-state", "", "absolute guest-native state directory in `DIR` (e.g. /home/dom/.local/state/messh)")
	fs.Int("mesh-port", 0, "guest mesh port to expose on the Windows LAN in `PORT`")
	fs.Int("local-port", 0, "guest local API port (recorded only; never forwarded or firewalled) in `PORT`")
	fs.Int("interface-index", 0, "Windows adapter index holding the LAN address in `N` (from `messh doctor`)")
	fs.String("host-address", "", "explicit Windows LAN address in `ADDR` (default: current address of --interface-index)")
	listFlag(fs, "peer", "allowed laptop/trusted private address in `ADDR` (repeatable)")
	fs.Bool("dry-run", false, "validate and print what would change without touching tasks, routes, or the firewall")
	fs.Duration("timeout", 5*time.Minute, "give up after `DURATION`")
}

// wslSetupResult is the --json shape of `wsl setup`.
type wslSetupResult struct {
	Distro          string   `json:"distro"`
	User            string   `json:"user"`
	Name            string   `json:"name"`
	ID              string   `json:"id"`
	GuestState      string   `json:"guest_state"`
	MeshPort        int      `json:"mesh_port"`
	LocalPort       int      `json:"local_port"`
	HostAddress     string   `json:"host_address"`
	InterfaceIndex  int      `json:"interface_index"`
	AllowedPeers    []string `json:"allowed_peers"`
	Tasks           struct {
		Logon string `json:"logon"`
		Route string `json:"route"`
	} `json:"tasks"`
	Route struct {
		Proxy   string `json:"proxy"`
		Rule    string `json:"rule"`
		Applied bool   `json:"applied"`
	} `json:"route"`
	RouteTaskStarted bool `json:"route_task_started"`
}

// wslRefreshResult is the --json shape of `wsl refresh`.
type wslRefreshResult struct {
	HostAddress         string `json:"host_address"`
	PreviousHostAddress string `json:"previous_host_address,omitempty"`
	Stale               bool   `json:"stale"`
	InterfaceIndex      int    `json:"interface_index"`
	Route               struct {
		Proxy   string `json:"proxy"`
		Rule    string `json:"rule"`
		Applied bool   `json:"applied"`
	} `json:"route"`
	RouteTaskStarted bool `json:"route_task_started"`
}

func runWSLSetup(c *Context) error {
	paths, err := c.Paths()
	if err != nil {
		return err
	}
	cfg, hostExplicit, err := wslConfigFromFlags(c)
	if err != nil {
		return err
	}
	timeout := c.Duration("timeout")
	if timeout <= 0 {
		return usageErrorf("--timeout must be positive")
	}
	ctx, cancel := newWSLContext(timeout)
	defer cancel()
	plan, err := wslPlanSetup(ctx, paths, cfg, hostExplicit)
	if err != nil {
		return err
	}
	if c.Bool("dry-run") {
		return c.Emit(wslSetupJSON(plan, false, false), func(w io.Writer) {
			wslWriteSetupPlan(w, plan)
		})
	}
	if err := wslWriteOwnerStartScript(paths, plan); err != nil {
		return err
	}
	// Persist the validated target before any task mutation: the logon task
	// may already be administrator-owned from a prior elevated run, and an
	// elevated run uses a different profile state. Owner metadata must exist
	// for `wsl status` and `refresh` regardless of task outcome.
	if err := paths.SaveWSLTarget(plan); err != nil {
		return withHint(fmt.Errorf("validated target could not be saved: %v", err),
			"re-run `messh wsl setup` with the same flags once the state directory is writable")
	}
	if _, err := wslEnsureLogonTask(ctx, paths, plan); err != nil {
		return err
	}
	applied, taskStarted, err := wslApplyMachineRoute(ctx, c, plan)
	if err != nil {
		return err
	}
	return c.Emit(wslSetupJSON(plan, applied, taskStarted), func(w io.Writer) {
		wslWriteSetupDone(w, plan, applied, taskStarted)
	})
}

func runWSLRefresh(c *Context) error {
	paths, err := c.Paths()
	if err != nil {
		return err
	}
	timeout := c.Duration("timeout")
	if timeout <= 0 {
		return usageErrorf("--timeout must be positive")
	}
	cfg, err := paths.LoadWSLTarget()
	if err != nil {
		return fmt.Errorf("cannot read the WSL target: %v", err)
	}
	if cfg == nil {
		return withHint(fmt.Errorf("no WSL target configured for this state directory"),
			"capture one on the Windows host with `messh wsl setup`")
	}
	ctx, cancel := newWSLContext(timeout)
	defer cancel()
	plan, previous, stale, err := wslPlanRefresh(ctx, *cfg)
	if err != nil {
		return err
	}
	if c.Bool("dry-run") {
		res := wslRefreshJSON(plan, previous, stale, false, false)
		return c.Emit(res, func(w io.Writer) {
			wslWriteRefreshPlan(w, plan, previous, stale)
		})
	}
	applied, taskStarted, err := wslApplyMachineRoute(ctx, c, plan)
	if err != nil {
		return err
	}
	if plan.HostAddress != previous {
		updated := plan
		updated.AllowedPeers = append([]string(nil), plan.AllowedPeers...)
		if err := paths.SaveWSLTarget(updated); err != nil {
			return withHint(fmt.Errorf("route refreshed but the host address was not re-captured: %v", err),
				"re-run `messh wsl refresh` once the state directory is writable")
		}
	}
	res := wslRefreshJSON(plan, previous, stale, applied, taskStarted)
	return c.Emit(res, func(w io.Writer) {
		wslWriteRefreshDone(w, plan, previous, stale, applied, taskStarted)
	})
}

// peer scope, and never forwards the local API port.
func wslConfigFromFlags(c *Context) (state.WSLTargetConfig, bool, error) {
	cfg := state.WSLTargetConfig{
		Distro:             strings.TrimSpace(c.String("distro")),
		User:               strings.TrimSpace(c.String("user")),
		Name:               strings.TrimSpace(c.String("name")),
		ID:                 strings.TrimSpace(c.String("id")),
		GuestState:         strings.TrimSpace(c.String("guest-state")),
		MeshPort:           c.Int("mesh-port"),
		LocalPort:          c.Int("local-port"),
		HostInterfaceIndex: c.Int("interface-index"),
		TaskName:           state.DefaultWSLTaskName,
		RouteTaskName:      state.DefaultWSLRouteTaskName,
		AllowedPeers:       append([]string(nil), c.List("peer")...),
	}
	var missing []string
	if !c.Set("distro") || cfg.Distro == "" {
		missing = append(missing, "--distro")
	}
	if !c.Set("user") || cfg.User == "" {
		missing = append(missing, "--user")
	}
	if !c.Set("name") || cfg.Name == "" {
		missing = append(missing, "--name")
	}
	if !c.Set("id") || cfg.ID == "" {
		missing = append(missing, "--id")
	}
	if !c.Set("guest-state") || cfg.GuestState == "" {
		missing = append(missing, "--guest-state")
	}
	if !c.Set("mesh-port") || cfg.MeshPort == 0 {
		missing = append(missing, "--mesh-port")
	}
	if !c.Set("local-port") || cfg.LocalPort == 0 {
		missing = append(missing, "--local-port")
	}
	if !c.Set("interface-index") || cfg.HostInterfaceIndex == 0 {
		missing = append(missing, "--interface-index")
	}
	if len(cfg.AllowedPeers) == 0 {
		missing = append(missing, "--peer")
	}
	if len(missing) > 0 {
		return state.WSLTargetConfig{}, false, usageErrorf("missing required flags: %s (every target field is explicit; see `messh wsl setup -h`)", strings.Join(missing, ", "))
	}
	hostExplicit := c.Set("host-address")
	if hostExplicit {
		cfg.HostAddress = strings.TrimSpace(c.String("host-address"))
		if cfg.HostAddress == "" {
			return state.WSLTargetConfig{}, false, usageErrorf("--host-address is empty: pass the explicit Windows LAN address or omit the flag to use --interface-index")
		}
	}
	return cfg, hostExplicit, nil
}

func wslSetupJSON(cfg state.WSLTargetConfig, applied, taskStarted bool) wslSetupResult {
	var res wslSetupResult
	res.Distro, res.User, res.Name, res.ID = cfg.Distro, cfg.User, cfg.Name, cfg.ID
	res.GuestState, res.MeshPort, res.LocalPort = cfg.GuestState, cfg.MeshPort, cfg.LocalPort
	res.HostAddress, res.InterfaceIndex = cfg.HostAddress, cfg.HostInterfaceIndex
	res.AllowedPeers = append([]string(nil), cfg.AllowedPeers...)
	res.Tasks.Logon, res.Tasks.Route = cfg.TaskName, cfg.RouteTaskName
	res.Route.Proxy = wslProxyLabel(cfg)
	res.Route.Rule = state.WSLFirewallRuleName(cfg.MeshPort)
	res.Route.Applied = applied
	res.RouteTaskStarted = taskStarted
	return res
}

func wslRefreshJSON(cfg state.WSLTargetConfig, previous string, stale, applied, taskStarted bool) wslRefreshResult {
	var res wslRefreshResult
	res.HostAddress, res.PreviousHostAddress, res.Stale = cfg.HostAddress, previous, stale
	res.InterfaceIndex = cfg.HostInterfaceIndex
	res.Route.Proxy = wslProxyLabel(cfg)
	res.Route.Rule = state.WSLFirewallRuleName(cfg.MeshPort)
	res.Route.Applied = applied
	res.RouteTaskStarted = taskStarted
	return res
}

func wslProxyLabel(cfg state.WSLTargetConfig) string {
	return net.JoinHostPort(cfg.HostAddress, strconv.Itoa(cfg.MeshPort)) + " -> 127.0.0.1:" + strconv.Itoa(cfg.MeshPort)
}

func wslWriteSetupPlan(w io.Writer, cfg state.WSLTargetConfig) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "distro\t%s (user %s)\n", cfg.Distro, cfg.User)
	fmt.Fprintf(tw, "target\t%s\n", cfg.Name)
	fmt.Fprintf(tw, "guest state\t%s\n", cfg.GuestState)
	fmt.Fprintf(tw, "mesh\t%s (native WSL localhost forwarder)\n", wslProxyLabel(cfg))
	fmt.Fprintf(tw, "guest local API\t%d (never forwarded or firewalled)\n", cfg.LocalPort)
	fmt.Fprintf(tw, "peers\t%s on Private\n", strings.Join(cfg.AllowedPeers, ", "))
	fmt.Fprintf(tw, "tasks\tlogon %s (owner) + route %s (SYSTEM)\n", cfg.TaskName, cfg.RouteTaskName)
	tw.Flush()
	fmt.Fprintln(w, "Would install the owner logon task and, after one UAC prompt, the machine route. Re-run without --dry-run to apply.")
}

func wslWriteSetupDone(w io.Writer, cfg state.WSLTargetConfig, applied, taskStarted bool) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "distro\t%s (user %s)\n", cfg.Distro, cfg.User)
	fmt.Fprintf(tw, "target\t%s\n", cfg.Name)
	fmt.Fprintf(tw, "route\t%s\n", wslProxyLabel(cfg))
	fmt.Fprintf(tw, "firewall\t%s from %s on Private\n", state.WSLFirewallRuleName(cfg.MeshPort), strings.Join(cfg.AllowedPeers, ", "))
	tw.Flush()
	switch {
	case taskStarted:
		fmt.Fprintln(w, "Route applied by the secured SYSTEM task.")
	case applied:
		fmt.Fprintln(w, "Route applied elevated (one UAC prompt).")
	}
	fmt.Fprintln(w, "Start the guest service inside the distro if it is not running: systemctl --user start messh.service")
	fmt.Fprintln(w, "Then check with: messh wsl status")
}

func wslWriteRefreshPlan(w io.Writer, cfg state.WSLTargetConfig, previous string, stale bool) {
	if stale {
		fmt.Fprintf(w, "Host address moved %s -> %s on interface %d (DHCP); would re-apply %s.\n", previous, cfg.HostAddress, cfg.HostInterfaceIndex, wslProxyLabel(cfg))
	} else {
		fmt.Fprintf(w, "Host address unchanged (%s on interface %d); would re-apply %s.\n", cfg.HostAddress, cfg.HostInterfaceIndex, wslProxyLabel(cfg))
	}
	fmt.Fprintln(w, "Re-run without --dry-run to apply.")
}

func wslWriteRefreshDone(w io.Writer, cfg state.WSLTargetConfig, previous string, stale, applied, taskStarted bool) {
	if stale {
		fmt.Fprintf(w, "Host address refreshed %s -> %s on interface %d.\n", previous, cfg.HostAddress, cfg.HostInterfaceIndex)
	} else {
		fmt.Fprintf(w, "Host address unchanged (%s on interface %d).\n", cfg.HostAddress, cfg.HostInterfaceIndex)
	}
	fmt.Fprintf(w, "Route %s (%s).\n", wslProxyLabel(cfg), map[bool]string{true: "applied", false: "already present"}[applied])
	if taskStarted {
		fmt.Fprintln(w, "Applied by the secured SYSTEM task.")
	}
	fmt.Fprintln(w, "Then check with: messh wsl status")
}
