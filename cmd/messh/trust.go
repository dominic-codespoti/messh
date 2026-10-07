package main

import (
	"context"
	"fmt"
	"io"
	"messh/internal/trust"
	"net/http"
	"net/url"
	"text/tabwriter"
)

func init() {
	add(&Command{Name: "trust ls", Summary: "list devices trusted to bypass approval on this node", Help: "Device-wide trust applies to every agent and exposed action on THIS node. Those actions are automatically approved and can modify or delete user files. Trust does not grant access on the other device.", Output: "[{device_id, device, created}]", Examples: []string{"messh trust ls", "messh trust ls --json"}, Run: func(c *Context) error {
		cl, _, err := c.Node()
		if err != nil {
			return err
		}
		var devices []trust.Device
		if err = cl.Do(context.Background(), http.MethodGet, "/v1/trust", nil, &devices); err != nil {
			return err
		}
		if devices == nil {
			devices = []trust.Device{}
		}
		return c.Emit(devices, func(w io.Writer) {
			if len(devices) == 0 {
				fmt.Fprintln(w, "No trusted devices.")
				return
			}
			tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "DEVICE ID\tDEVICE\tTRUSTED SINCE")
			for _, d := range devices {
				fmt.Fprintf(tw, "%s\t%s\t%s\n", d.DeviceID, d.Device, d.Created.Local().Format("2006-01-02 15:04:05"))
			}
			tw.Flush()
		})
	}})
	add(&Command{Name: "trust add", Args: []Arg{{Name: "DEVICE", Help: "paired device name or ID prefix"}}, Summary: "trust a paired device for all-agent approval bypass on this node", Help: "WARNING: every agent and exposed action from this device is automatically approved on THIS node and can modify or delete user files. This does not extend trust to the other device. Trust is permanent until revoked or the device is unpaired.", Output: "{device_id, device, created}", Mutates: true, Person: "confirm that you want every agent and exposed action from this device automatically approved on this node, with access that can modify or delete user files", Examples: []string{"messh trust add worker", "messh trust add 3fa85f64"}, Run: func(c *Context) error {
		cl, _, err := c.Node()
		if err != nil {
			return err
		}
		var device trust.Device
		if err = cl.Do(context.Background(), http.MethodPost, "/v1/trust", map[string]string{"device": c.Args[0]}, &device); err != nil {
			return err
		}
		return c.Emit(device, func(w io.Writer) {
			fmt.Fprintf(w, "Trusted %s (%s) on this node; all agents and exposed actions are automatically approved.\n", device.Device, device.DeviceID)
		})
	}})
	add(&Command{Name: "trust rm", Args: []Arg{{Name: "DEVICE", Help: "paired device name/ID prefix, or full stored immutable ID"}}, Summary: "revoke device-wide trust on this node", Help: "Removing trust restores this node's normal approval policy. A full stored immutable device ID can be used even if the device is no longer paired. This changes trust on THIS node only.", Output: "204 No Content", Mutates: true, Person: "confirm that you want to revoke device-wide trust on this node", Examples: []string{"messh trust rm worker", "messh trust rm 3fa85f64"}, Run: func(c *Context) error {
		cl, _, err := c.Node()
		if err != nil {
			return err
		}
		if err = cl.Do(context.Background(), http.MethodDelete, "/v1/trust/"+url.PathEscape(c.Args[0]), nil, nil); err != nil {
			return err
		}
		return c.Emit(struct{}{}, func(w io.Writer) { fmt.Fprintf(w, "Revoked trust for %s on this node.\n", c.Args[0]) })
	}})
}
