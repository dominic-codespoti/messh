//go:build !windows

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"

	"messh/internal/netcheck"
	"messh/internal/state"
)

// On Linux the rules need root, and messh never runs sudo: it prints the
// commands for the detected firewall. --dry-run changes nothing here.
func firewallAllowRun(c *Context, ctx context.Context, paths state.Paths) error {
	if c.String("private") != "" {
		return errors.New("--private sets a Windows network category; it does not apply here")
	}
	return printLinuxFirewall(c, ctx, true)
}

func firewallRemoveRun(c *Context, ctx context.Context, paths state.Paths) error {
	return printLinuxFirewall(c, ctx, false)
}

func printLinuxFirewall(c *Context, ctx context.Context, allow bool) error {
	dryRun := c.Bool("dry-run")
	ifaces, err := netcheck.LANInterfaces()
	if err != nil {
		return err
	}
	var subnets []netip.Prefix
	for _, i := range ifaces {
		subnets = append(subnets, i.Subnet())
	}
	t := netcheck.ProbeLinux(ctx, ifaces).Target()
	if t.Tool == "" {
		res := firewallResult{DryRun: dryRun, Commands: []string{}}
		return c.Emit(res, func(w io.Writer) {
			fmt.Fprintln(w, "No active firewall found (ufw, firewalld, nftables input filtering): peers can already reach TCP/UDP 7519.")
		})
	}
	cmds := netcheck.LinuxRemoveCommands(t, subnets)
	if allow {
		cmds = netcheck.LinuxAllowCommands(t, subnets)
	}
	if len(cmds) == 0 {
		return errors.New("no LAN interface with an IPv4 address found")
	}
	res := firewallResult{DryRun: dryRun, Commands: cmds}
	return c.Emit(res, func(w io.Writer) {
		fmt.Fprintf(w, "%s filters inbound traffic here. Run (messh does not run sudo itself):\n\n", t.Tool)
		for _, cmd := range cmds {
			fmt.Fprintln(w, "  "+cmd)
		}
		if t.Tool == "nftables" {
			fmt.Fprintln(w)
			if !t.Known {
				fmt.Fprintln(w, "The ruleset needs root to read, so the table and chain above are the usual ones from")
				fmt.Fprintln(w, "/etc/nftables.conf; check with: sudo nft list ruleset")
			}
			if allow {
				fmt.Fprintln(w, "nft rules added this way are lost at reboot; add the same lines to /etc/nftables.conf to keep them.")
			}
		}
		if allow {
			fmt.Fprintln(w, "\nThe rules only accept the local subnet(s), so the ports stay closed to everything else.")
		}
		fmt.Fprintln(w, "Then check with: messh doctor")
	})
}

var _ = state.Paths{}
