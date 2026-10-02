package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"messh/internal/grants"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"
)

func init() {
	add(&Command{Name: "grant ls", Summary: "list owner-managed capability grants", Output: "[{id, subject, kind, tool/path, actions, created_at, expires_at, revoked_at}]", Examples: []string{"messh grant ls", "messh grant ls --json"}, Run: func(c *Context) error {
		cl, _, e := c.Node()
		if e != nil {
			return e
		}
		var gs []grants.Grant
		if e = cl.Do(context.Background(), http.MethodGet, "/v1/grants", nil, &gs); e != nil {
			return e
		}
		if gs == nil {
			gs = []grants.Grant{}
		}
		return c.Emit(gs, func(w io.Writer) {
			if len(gs) == 0 {
				fmt.Fprintln(w, "No capability grants.")
				return
			}
			tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tSUBJECT\tSCOPE\tACTIONS\tEXPIRES\tSTATE")
			for _, g := range gs {
				scope := g.Tool
				if g.Kind == "file" {
					scope = g.Path
				}
				status := "active"
				if g.RevokedAt != nil {
					status = "revoked"
				} else if time.Now().After(g.ExpiresAt) {
					status = "expired"
				}
				fmt.Fprintf(tw, "%s\t%s/%s\t%s\t%s\t%s\t%s\n", g.ID, g.Subject.DeviceID, g.Subject.Agent, scope, strings.Join(g.Actions, ","), g.ExpiresAt.Local().Format(time.DateTime), status)
			}
			tw.Flush()
		})
	}})
	add(&Command{Name: "grant add", Summary: "create an exact expiring capability grant", Help: "Grants are scoped to an immutable device ID and agent label. Tool grants require the args_hash returned by capability_check. File grants are path-subtree/action scoped. Expiry is mandatory and must be finite.", Args: []Arg{{Name: "KIND", Choices: []string{"tool", "file"}}}, Flags: func(fs *flag.FlagSet) {
		fs.String("device", "", "immutable subject device ID")
		fs.String("agent", "", "subject agent label")
		fs.String("tool", "", "exact tool name")
		fs.String("args-hash", "", "native exact key returned by capability_check")
		fs.String("path", "", "canonical file subtree, e.g. ws/project")
		fs.String("actions", "", "comma-separated actions: invoke, read, write")
		fs.Duration("expires-in", 0, "required finite lifetime, e.g. 30m or 24h")
	}, Required: []string{"device", "agent", "actions", "expires-in"}, Output: "grant", Mutates: true, Examples: []string{"messh grant add file --device DEVICE_ID --agent pi --path ws/project --actions read,write --expires-in 8h", "messh grant add tool --device DEVICE_ID --agent pi --tool job_submit --args-hash KEY --actions invoke --expires-in 30m"}, Run: func(c *Context) error {
		dur := c.Duration("expires-in")
		if dur <= 0 {
			return usageErrorf("--expires-in must be positive")
		}
		actions := strings.Split(c.String("actions"), ",")
		g := grants.Grant{Subject: grants.Subject{DeviceID: c.String("device"), Agent: c.String("agent")}, Kind: c.Args[0], Tool: c.String("tool"), ArgsHash: c.String("args-hash"), Path: c.String("path"), Actions: actions, ExpiresAt: time.Now().UTC().Add(dur)}
		cl, _, e := c.Node()
		if e != nil {
			return e
		}
		var out grants.Grant
		if e = cl.Do(context.Background(), http.MethodPost, "/v1/grants", g, &out); e != nil {
			return e
		}
		return c.Emit(out, func(w io.Writer) {
			fmt.Fprintf(w, "Created capability grant %s for %s/%s until %s.\n", out.ID, out.Subject.DeviceID, out.Subject.Agent, out.ExpiresAt.Local().Format(time.DateTime))
		})
	}})
	add(&Command{Name: "grant rm", Args: []Arg{{Name: "ID", Help: "grant ID"}}, Summary: "durably revoke a capability grant", Output: "{grant}", Mutates: true, Examples: []string{"messh grant rm 3fa85f64"}, Run: func(c *Context) error {
		cl, _, e := c.Node()
		if e != nil {
			return e
		}
		var out grants.Grant
		if e = cl.Do(context.Background(), http.MethodDelete, "/v1/grants/"+url.PathEscape(c.Args[0]), nil, &out); e != nil {
			return e
		}
		return c.Emit(out, func(w io.Writer) { fmt.Fprintf(w, "Revoked capability grant %s.\n", out.ID) })
	}})
}
