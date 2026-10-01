package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"messh/internal/control"
)

func init() {
	add(&Command{
		Name:    "wake",
		Args:    []Arg{{Name: "DEVICE", Help: "device to wake"}},
		Summary: "wake a sleeping device with Wake-on-LAN and wait until it answers",
		Help: "With --auto on|off it only changes whether tool calls to the device wake it first\n" +
			"(up to 60s) when it is asleep or offline (default off), and returns at once.",
		Flags: func(fs *flag.FlagSet) {
			fs.Duration("wait", 60*time.Second, "wait up to `DURATION` for the device to answer (1s to 5m)")
			choiceFlag(fs, "auto", "", "wake the device automatically before tool calls (`on|off`)", "on", "off")
		},
		Output:  `{"device", "id", "before", "awake", "auto_wake", "macs"[], "packets_sent", "packets_failed", "routes"[], "no_shared_subnet", "seconds_to_wake", "error"}`,
		Mutates: true,
		Waits:   "up to --wait for the device to answer (--auto returns at once)",
		Examples: []string{
			"messh wake desktop",
			"messh wake desktop --wait 2m",
			"messh wake desktop --auto on",
		},
		Run: func(c *Context) error { return runWake(c) },
	})
}

func runWake(c *Context) error {
	wait := c.Duration("wait")
	cl, _, err := c.Node()
	if err != nil {
		return err
	}
	var req control.WakeRequest
	if c.Set("auto") {
		on := c.String("auto") == "on"
		req.Auto = &on
	} else {
		if wait < time.Second || wait > 5*time.Minute {
			return errors.New("--wait must be between 1s and 5m")
		}
		req.WaitSeconds = int((wait + time.Second - 1) / time.Second)
		cl.HTTP.Timeout = wait + 30*time.Second // the node answers once the device does or the wait ends
	}
	var res control.WakeResult
	if err := cl.Do(context.Background(), http.MethodPost, "/v1/wake/"+url.PathEscape(c.Args[0]), req, &res); err != nil {
		return err
	}
	var b strings.Builder
	if req.Auto == nil {
		fmt.Fprintf(&b, "Waking %s (waiting up to %s)...\n", c.Args[0], wait)
	} else {
		state := "off"
		if res.AutoWake {
			state = "on: tool calls to it wake it first (up to 60s) when it is asleep or offline"
		}
		fmt.Fprintf(&b, "Auto-wake for %s is %s. It is %s now.\n", res.Device, state, res.Before)
		return c.Emit(res, func(w io.Writer) { io.WriteString(w, b.String()) })
	}
	if res.Awake && res.PacketsSent == 0 {
		fmt.Fprintf(&b, "%s is already online.\n", res.Device)
		return c.Emit(res, func(w io.Writer) { io.WriteString(w, b.String()) })
	}
	if len(res.MACs) > 0 {
		fmt.Fprintf(&b, "Sent %d magic packets", res.PacketsSent)
		if res.PacketsFailed > 0 {
			fmt.Fprintf(&b, " (%d failed)", res.PacketsFailed)
		}
		fmt.Fprintf(&b, " to %s via:\n", strings.Join(res.MACs, ", "))
		for _, r := range res.Routes {
			fmt.Fprintf(&b, "  %s\n", r)
		}
		if res.NoSharedSubnet {
			fmt.Fprintf(&b, "No interface here is in %s's subnet, so only 255.255.255.255 was used.\n", res.Device)
		}
	}
	if res.Error != "" {
		if !c.JSON {
			io.WriteString(c.Stdout, b.String())
		}
		return errors.New(res.Error)
	}
	fmt.Fprintf(&b, "%s is awake after %.1fs (it was %s).\n", res.Device, res.Seconds, res.Before)
	return c.Emit(res, func(w io.Writer) { io.WriteString(w, b.String()) })
}
