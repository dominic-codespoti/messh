package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"messh/internal/control"
	"messh/internal/identity"
)

func init() {
	add(&Command{
		Name:    "pair",
		Args:    []Arg{{Name: "TARGET", Help: "discovered name, ID prefix, host, or host:port"}},
		Summary: "start pairing with another device",
		Help: "Starts an outgoing pairing with TARGET (a discovered name, ID prefix, host, or host:port).\n" +
			"A device literally named like a pair verb (accept, close, requests, approve, reject,\n" +
			"confirm, cancel, status) must be addressed by ID prefix or address: `messh pair` dispatches\n" +
			"those words to the subcommand of the same name.",
		Output:  `{"id", "peer_name", "peer_id", "addr", "code", "state"}`,
		Mutates: true,
		Person:  "compare the code with the one shown on the other device",
		Examples: []string{
			"messh pair desktop",
			"messh pair 192.168.1.5:7519",
		},
		Run: func(c *Context) error {
			cl, _, err := c.Node()
			if err != nil {
				return err
			}
			ctx := context.Background()
			var og control.Outgoing
			if err := cl.Do(ctx, http.MethodPost, "/v1/pair/outgoing", control.PairRequest{Target: c.Args[0]}, &og); err != nil {
				return err
			}
			// The remote answers immediately when it is not accepting pairings; report that first.
			for range 4 {
				time.Sleep(150 * time.Millisecond)
				if err := cl.Do(ctx, http.MethodGet, "/v1/pair/outgoing/"+og.ID, nil, &og); err != nil {
					return err
				}
				if err := pairOutcome(og); err != nil {
					return err
				}
				if og.State != control.OutAwaitingConfirmation {
					break
				}
			}
			v := outgoingJSON(og)
			return c.Emit(v, func(w io.Writer) {
				fmt.Fprintf(w, "Pairing with %q (id %s) at %s\n  code: %s\n", og.PeerName, identity.Short(og.PeerID), og.Addr, og.Code)
				fmt.Fprintf(w, "On the other device, run `messh pair requests`, then `messh pair approve <their id> --code %s`.\n", og.Code)
				fmt.Fprintf(w, "Then confirm here: messh pair confirm %s --code <code shown on the other device>\n", og.ID)
			})
		},
	})
	add(&Command{
		Name:    "pair accept",
		Summary: "open the pairing window for incoming requests",
		Output:  `without --wait: {"name", "until"} (until is RFC 3339); with --wait: [{id, peer_name, peer_id, addr, code}]`,
		Mutates: true,
		Waits:   "with --wait, until the first request arrives or --timeout elapses",
		Flags: func(fs *flag.FlagSet) {
			fs.Duration("timeout", 2*time.Minute, "keep the window open for `DURATION`")
			fs.Bool("wait", false, "block until the first request arrives or the window closes")
		},
		Examples: []string{
			"messh pair accept",
			"messh pair accept --wait --timeout 5m",
		},
		Run: func(c *Context) error {
			cl, _, err := c.Node()
			if err != nil {
				return err
			}
			ctx := context.Background()
			var st control.Status
			if err := cl.Do(ctx, http.MethodGet, "/v1/status", nil, &st); err != nil {
				return err
			}
			var win control.PairWindow
			if err := cl.Do(ctx, http.MethodPost, "/v1/pair/accept", control.PairAcceptRequest{TimeoutSeconds: int(c.Duration("timeout").Seconds())}, &win); err != nil {
				return err
			}
			if !c.Bool("wait") {
				v := map[string]string{"name": st.Name, "until": win.Until.Format(time.RFC3339)}
				return c.Emit(v, func(w io.Writer) {
					fmt.Fprintf(w, "%s is accepting pairing requests until %s.\nOn the other device run: messh pair %s\nWhen it shows a code, run: messh pair requests, then messh pair approve ID --code CODE\n",
						st.Name, win.Until.Format(time.TimeOnly), st.Name)
				})
			}
			deadline := win.Until
			var pending []control.Incoming
			for time.Now().Before(deadline) {
				pending = nil
				if err := cl.Do(ctx, http.MethodGet, "/v1/pair/incoming", nil, &pending); err != nil {
					return err
				}
				if len(pending) > 0 {
					break
				}
				time.Sleep(500 * time.Millisecond)
			}
			waitOut := make([]map[string]string, 0, len(pending))
			for _, in := range pending {
				waitOut = append(waitOut, map[string]string{
					"id": in.ID, "peer_name": in.PeerName, "peer_id": in.PeerID,
					"addr": in.Addr, "code": in.Code,
				})
			}
			return c.Emit(waitOut, func(w io.Writer) {
				if len(pending) == 0 {
					fmt.Fprintln(w, "Pairing window closed without a request.")
					return
				}
				for _, in := range pending {
					fmt.Fprintf(w, "%q (id %s) at %s wants to pair.\n  code: %s\n", in.PeerName, identity.Short(in.PeerID), in.Addr, in.Code)
				}
				fmt.Fprintln(w, "Compare the code on both devices, then: messh pair approve ID --code CODE")
			})
		},
	})
	add(&Command{
		Name:    "pair close",
		Summary: "close the pairing window early",
		Output:  `{"open": false}`,
		Mutates: true,
		Examples: []string{
			"messh pair close",
		},
		Run: func(c *Context) error {
			cl, _, err := c.Node()
			if err != nil {
				return err
			}
			var win control.PairWindow
			if err := cl.Do(context.Background(), http.MethodDelete, "/v1/pair/accept", nil, &win); err != nil {
				return err
			}
			return c.Emit(map[string]bool{"open": false}, func(w io.Writer) {
				fmt.Fprintln(w, "Pairing window closed.")
			})
		},
	})
	add(&Command{
		Name:    "pair requests",
		Summary: "list incoming pairing requests",
		Output:  `[{id, peer_name, peer_id, addr, code, created}] (created is RFC 3339)`,
		Examples: []string{
			"messh pair requests",
			"messh pair requests --json",
		},
		Run: func(c *Context) error {
			cl, _, err := c.Node()
			if err != nil {
				return err
			}
			var pending []control.Incoming
			if err := cl.Do(context.Background(), http.MethodGet, "/v1/pair/incoming", nil, &pending); err != nil {
				return err
			}
			if pending == nil {
				pending = []control.Incoming{}
			}
			return c.Emit(incomingListJSON(pending), func(w io.Writer) {
				if len(pending) == 0 {
					fmt.Fprintln(w, "No incoming pairing requests.")
					return
				}
				for _, in := range pending {
					fmt.Fprintf(w, "%s\t%q (id %s) at %s\tcode %s\n", in.ID, in.PeerName, identity.Short(in.PeerID), in.Addr, in.Code)
				}
			})
		},
	})
	add(&Command{
		Name:    "pair approve",
		Args:    []Arg{{Name: "ID", Help: "incoming request ID"}},
		Summary: "approve an incoming pairing request",
		Output:  `{"id", "peer_name", "state": "approved"}`,
		Mutates: true,
		Person:  "compare the code with the one shown on the other device",
		Flags: func(fs *flag.FlagSet) {
			fs.String("code", "", "code shown on the other device in `CODE`")
		},
		Required: []string{"code"},
		Examples: []string{
			"messh pair approve abc123 --code 441-354",
		},
		Run: func(c *Context) error {
			if strings.TrimSpace(c.String("code")) == "" {
				return usageErrorf("--code is empty: pass the code shown on the other device")
			}
			cl, _, err := c.Node()
			if err != nil {
				return err
			}
			ctx := context.Background()
			var pending []control.Incoming
			if err := cl.Do(ctx, http.MethodGet, "/v1/pair/incoming", nil, &pending); err != nil {
				return err
			}
			id := c.Args[0]
			var match *control.Incoming
			for i := range pending {
				if pending[i].ID == id {
					match = &pending[i]
					break
				}
			}
			if match == nil {
				return fmt.Errorf("no incoming pairing request %q", id)
			}
			if !sameCode(c.String("code"), match.Code) {
				return withHint(fmt.Errorf("codes do not match; the request is still pending"), fmt.Sprintf("messh pair reject %s", id))
			}
			if err := cl.Do(ctx, http.MethodPost, "/v1/pair/incoming/"+id, control.Decision{Accept: true}, nil); err != nil {
				return err
			}
			v := map[string]string{"id": id, "peer_name": match.PeerName, "state": "approved"}
			return c.Emit(v, func(w io.Writer) {
				fmt.Fprintf(w, "Paired with %s.\n", match.PeerName)
			})
		},
	})
	add(&Command{
		Name:    "pair reject",
		Args:    []Arg{{Name: "ID", Help: "incoming request ID"}},
		Summary: "reject an incoming pairing request",
		Output:  `{"id", "state": "rejected"}`,
		Mutates: true,
		Examples: []string{
			"messh pair reject abc123",
		},
		Run: func(c *Context) error {
			cl, _, err := c.Node()
			if err != nil {
				return err
			}
			id := c.Args[0]
			if err := cl.Do(context.Background(), http.MethodPost, "/v1/pair/incoming/"+id, control.Decision{Accept: false}, nil); err != nil {
				return err
			}
			return c.Emit(map[string]string{"id": id, "state": "rejected"}, func(w io.Writer) {
				fmt.Fprintln(w, "Rejected.")
			})
		},
	})
	add(&Command{
		Name:    "pair confirm",
		Args:    []Arg{{Name: "ID", Help: "outgoing pairing ID"}},
		Summary: "confirm the code on an outgoing pairing",
		Output:  `{"id", "peer_name", "state": "paired"}`,
		Mutates: true,
		Person:  "compare the code with the one shown on the other device",
		Waits:   "up to --wait for the other device to accept",
		Flags: func(fs *flag.FlagSet) {
			fs.String("code", "", "code shown on the other device in `CODE`")
			fs.Duration("wait", 2*time.Minute, "wait up to `DURATION` for the other device to accept (0 returns at once)")
		},
		Required: []string{"code"},
		Examples: []string{
			"messh pair confirm abc123 --code 441-354",
		},
		Run: func(c *Context) error {
			if strings.TrimSpace(c.String("code")) == "" {
				return usageErrorf("--code is empty: pass the code shown on the other device")
			}
			cl, _, err := c.Node()
			if err != nil {
				return err
			}
			ctx := context.Background()
			id := c.Args[0]
			var og control.Outgoing
			if err := cl.Do(ctx, http.MethodGet, "/v1/pair/outgoing/"+id, nil, &og); err != nil {
				return err
			}
			if !sameCode(c.String("code"), og.Code) {
				return withHint(errors.New("codes do not match; the pairing is still pending"), fmt.Sprintf("messh pair cancel %s", id))
			}
			if err := cl.Do(ctx, http.MethodPost, "/v1/pair/outgoing/"+id, control.Decision{Accept: true}, &og); err != nil {
				return err
			}
			if wait := c.Duration("wait"); wait <= 0 {
				v := map[string]string{"id": og.ID, "peer_name": og.PeerName, "state": og.State}
				return c.Emit(v, func(w io.Writer) {
					fmt.Fprintf(w, "Confirmed pairing with %s; waiting for it to accept (state %s).\n", og.PeerName, og.State)
				})
			}
			deadline := time.Now().Add(c.Duration("wait"))
			for {
				switch og.State {
				case control.OutPaired:
					v := map[string]string{"id": og.ID, "peer_name": og.PeerName, "state": "paired"}
					return c.Emit(v, func(w io.Writer) {
						fmt.Fprintf(w, "Paired with %s.\n", og.PeerName)
					})
				case control.OutAwaitingRemote, control.OutAwaitingConfirmation:
					if !time.Now().Before(deadline) {
						return needsPerson(fmt.Sprintf("%s has not accepted yet", og.PeerName),
							fmt.Sprintf("on %s run messh pair requests, then messh pair approve ID --code %s", og.PeerName, og.Code))
					}
				default:
					if err := pairOutcome(og); err != nil {
						return err
					}
				}
				time.Sleep(500 * time.Millisecond)
				if err := cl.Do(ctx, http.MethodGet, "/v1/pair/outgoing/"+id, nil, &og); err != nil {
					return err
				}
			}
		},
	})
	add(&Command{
		Name:    "pair cancel",
		Args:    []Arg{{Name: "ID", Help: "outgoing pairing ID"}},
		Summary: "cancel an outgoing pairing",
		Output:  `{"id", "state": "cancelled"}`,
		Mutates: true,
		Examples: []string{
			"messh pair cancel abc123",
		},
		Run: func(c *Context) error {
			cl, _, err := c.Node()
			if err != nil {
				return err
			}
			id := c.Args[0]
			if err := cl.Do(context.Background(), http.MethodPost, "/v1/pair/outgoing/"+id, control.Decision{Accept: false}, nil); err != nil {
				return err
			}
			return c.Emit(map[string]string{"id": id, "state": "cancelled"}, func(w io.Writer) {
				fmt.Fprintln(w, "Pairing cancelled.")
			})
		},
	})
	add(&Command{
		Name:    "pair status",
		Args:    []Arg{{Name: "ID", Help: "outgoing pairing ID"}},
		Summary: "show an outgoing pairing's state",
		Output:  `{"id", "peer_name", "peer_id", "addr", "code", "state"}`,
		Examples: []string{
			"messh pair status abc123",
		},
		Run: func(c *Context) error {
			cl, _, err := c.Node()
			if err != nil {
				return err
			}
			var og control.Outgoing
			if err := cl.Do(context.Background(), http.MethodGet, "/v1/pair/outgoing/"+c.Args[0], nil, &og); err != nil {
				return err
			}
			return c.Emit(outgoingJSON(og), func(w io.Writer) {
				fmt.Fprintf(w, "%s\t%q (id %s) at %s\tcode %s\t%s\n", og.ID, og.PeerName, identity.Short(og.PeerID), og.Addr, og.Code, og.State)
			})
		},
	})
}

// sameCode compares pairing codes by digits only, so "441-354", "441354"
// and "441 354" are equal.
func sameCode(a, b string) bool {
	da, db := digitsOnly(a), digitsOnly(b)
	if len(da) == 0 || len(da) != len(db) {
		return false
	}
	return da == db
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func outgoingJSON(og control.Outgoing) map[string]string {
	return map[string]string{
		"id": og.ID, "peer_name": og.PeerName, "peer_id": og.PeerID,
		"addr": og.Addr, "code": og.Code, "state": og.State,
	}
}

func incomingListJSON(list []control.Incoming) []map[string]string {
	out := make([]map[string]string, 0, len(list))
	for _, in := range list {
		m := map[string]string{
			"id": in.ID, "peer_name": in.PeerName, "peer_id": in.PeerID,
			"addr": in.Addr, "code": in.Code,
		}
		if !in.Created.IsZero() {
			m["created"] = in.Created.Format(time.RFC3339)
		}
		out = append(out, m)
	}
	return out
}

// pairOutcome turns terminal failure states into errors.
func pairOutcome(og control.Outgoing) error {
	switch og.State {
	case control.OutRejected:
		return fmt.Errorf("%s rejected the pairing", og.PeerName)
	case control.OutFailed:
		return fmt.Errorf("pairing failed: %s", og.Error)
	case control.OutCancelled:
		return errors.New("pairing cancelled")
	}
	return nil
}
