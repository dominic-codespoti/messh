package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"messh/internal/control"
	"messh/internal/identity"
	"messh/internal/node"
	"messh/internal/state"
)

func init() {
	add(&Command{
		Name:    "id",
		Summary: "print this device's name and ID",
		Output:  `{"name": "...", "id": "..."}`,
		Examples: []string{
			"messh id",
			"messh id --json",
		},
		Run: func(c *Context) error {
			paths, err := c.Paths()
			if err != nil {
				return err
			}
			cfg, err := paths.LoadConfig()
			if err != nil {
				return err
			}
			name := cfg.Name
			if name == "" {
				name = "unnamed"
			}
			id, err := identity.LoadOrCreate(paths.IdentityDir(), name)
			if err != nil {
				return err
			}
			v := map[string]string{"name": name, "id": id.ID}
			return c.Emit(v, func(w io.Writer) {
				fmt.Fprintf(w, "%s\t%s\n", name, id.ID)
			})
		},
	})
	add(&Command{
		Name:    "status",
		Summary: "show the running node's status",
		Output:  `{id, name, version, build: {version, commit, channel, build}, mesh, local, started} (started is RFC 3339; build.build is an unsigned integer)`,
		Examples: []string{
			"messh status",
			"messh status --json",
		},
		Run: func(c *Context) error {
			cl, _, err := c.Node()
			if err != nil {
				return err
			}
			var st control.Status
			if err := cl.Do(context.Background(), http.MethodGet, "/v1/status", nil, &st); err != nil {
				return err
			}
			return c.Emit(st, func(w io.Writer) {
				fmt.Fprintf(w, "name     %s\nid       %s\nversion  %s\nmesh     %s\nagents   http://%s/mcp\nup since %s\n",
					st.Name, st.ID, st.Version, st.Mesh, st.Local, st.Started.Format(time.DateTime))
			})
		},
	})
	add(&Command{
		Name:    "peers",
		Summary: "list paired devices",
		Output:  `[{name, id, online, last_seen, address, tools[]}] (last_seen is RFC 3339)`,
		Examples: []string{
			"messh peers",
			"messh peers --json",
		},
		Run: func(c *Context) error {
			cl, _, err := c.Node()
			if err != nil {
				return err
			}
			var peers []control.PeerStatus
			if err := cl.Do(context.Background(), http.MethodGet, "/v1/peers", nil, &peers); err != nil {
				return err
			}
			if peers == nil {
				peers = []control.PeerStatus{}
			}
			type peerJSON struct {
				Name     string    `json:"name"`
				ID       string    `json:"id"`
				Online   bool      `json:"online"`
				LastSeen time.Time `json:"last_seen"`
				Address  string    `json:"address"`
				Tools    []string  `json:"tools"`
			}
			out := make([]peerJSON, 0, len(peers))
			for _, p := range peers {
				addr := ""
				if len(p.Addrs) > 0 {
					addr = p.Addrs[0]
				}
				tools := p.Tools
				if tools == nil {
					tools = []string{}
				}
				out = append(out, peerJSON{Name: p.Name, ID: p.ID, Online: p.Online, LastSeen: p.LastSeen, Address: addr, Tools: tools})
			}
			return c.Emit(out, func(w io.Writer) {
				if len(peers) == 0 {
					fmt.Fprintln(w, "No paired devices. Pair with: messh pair accept (on one device) and messh pair NAME (on the other).")
					return
				}
				tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tID\tSTATUS\tLAST SEEN\tADDRESS\tTOOLS")
				for _, p := range peers {
					status := "offline"
					switch {
					case p.Asleep:
						status = "asleep"
					case p.Online:
						status = "online"
					}
					if p.AutoWake {
						status += " (auto-wake)"
					}
					addr := ""
					if len(p.Addrs) > 0 {
						addr = p.Addrs[0]
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", p.Name, identity.Short(p.ID), status, ago(p.LastSeen), addr, strings.Join(p.Tools, ","))
				}
				tw.Flush()
			})
		},
	})
	add(&Command{
		Name:    "discover",
		Summary: "list devices heard on the LAN",
		Output:  `[{name, id, address, heard, paired}] (heard is RFC 3339)`,
		Examples: []string{
			"messh discover",
			"messh discover --json",
		},
		Run: func(c *Context) error {
			cl, _, err := c.Node()
			if err != nil {
				return err
			}
			var seen []control.Sighting
			if err := cl.Do(context.Background(), http.MethodGet, "/v1/discovered", nil, &seen); err != nil {
				return err
			}
			if seen == nil {
				seen = []control.Sighting{}
			}
			type sightJSON struct {
				Name    string    `json:"name"`
				ID      string    `json:"id"`
				Address string    `json:"address"`
				Heard   time.Time `json:"heard"`
				Paired  bool      `json:"paired"`
			}
			out := make([]sightJSON, 0, len(seen))
			for _, s := range seen {
				out = append(out, sightJSON{Name: s.Name, ID: s.ID, Address: s.Addr, Heard: s.Seen, Paired: s.Paired})
			}
			return c.Emit(out, func(w io.Writer) {
				if len(seen) == 0 {
					fmt.Fprintln(w, "No devices heard yet. Discovery needs other nodes on the same LAN segment.")
					return
				}
				tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tID\tADDRESS\tHEARD\tPAIRED")
				for _, s := range seen {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%v\n", s.Name, identity.Short(s.ID), s.Addr, ago(s.Seen), s.Paired)
				}
				tw.Flush()
			})
		},
	})
	add(&Command{
		Name:    "unpair",
		Args:    []Arg{{Name: "DEVICE", Help: "paired device name or ID prefix"}},
		Summary: "forget a paired device",
		Output:  `{"name": "...", "id": "...", "state": "unpaired"}`,
		Mutates: true,
		Examples: []string{
			"messh unpair desktop",
			"messh unpair desktop --json",
		},
		Run: func(c *Context) error {
			cl, _, err := c.Node()
			if err != nil {
				return err
			}
			var p control.PeerStatus
			if err := cl.Do(context.Background(), http.MethodDelete, "/v1/peers/"+url.PathEscape(c.Args[0]), nil, &p); err != nil {
				return err
			}
			v := map[string]string{"name": p.Name, "id": p.ID, "state": "unpaired"}
			return c.Emit(v, func(w io.Writer) {
				fmt.Fprintf(w, "Unpaired %s (%s). Run `messh unpair` on it as well to revoke its side.\n", p.Name, identity.Short(p.ID))
			})
		},
	})
}

func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t).Round(time.Second)
	if d < time.Second {
		return "now"
	}
	return d.String() + " ago"
}

// localMCPURL is where agents on this device connect.
func localMCPURL(paths state.Paths) string {
	if run, err := paths.LoadRunInfo(); err == nil {
		return "http://" + run.Local + "/mcp"
	}
	return "http://" + node.DefaultLocalAddr + "/mcp"
}
