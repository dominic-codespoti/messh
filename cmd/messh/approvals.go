package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"messh/internal/approval"
	"messh/internal/control"
)

// answerHelp is shared by the three commands that decide a waiting request:
// they act for the person at this device, so an agent must not touch them
// unprompted.
const answerHelp = "Allow, always and deny answer a waiting request on behalf of the person at this device: " +
	"no prompt appears for the request once they run. An agent must only run them when that person " +
	"explicitly tells it to, naming the request to answer."

func init() {
	add(&Command{
		Name:    "approvals",
		Summary: "list requests waiting for your approval on this device",
		Help: "Shows every pending request: who asks, what it wants, when it expires, and the " +
			"answer command for each. Answer one with `messh approvals allow|always|deny ID`.",
		Output:   "[{id, device_id, device, agent, tool, class, title, details[{label, value}], scopes[{label, broad}], created, expires, shown}] (times RFC 3339)",
		Examples: []string{"messh approvals", "messh approvals --json"},
		Run: func(c *Context) error {
			cl, _, err := c.Node()
			if err != nil {
				return err
			}
			var pending []control.PendingApproval
			if err := cl.Do(context.Background(), http.MethodGet, "/v1/approvals", nil, &pending); err != nil {
				return err
			}
			if pending == nil {
				pending = []control.PendingApproval{}
			}
			return c.Emit(pending, func(w io.Writer) {
				if len(pending) == 0 {
					fmt.Fprintln(w, "Nothing is waiting for approval.")
					return
				}
				for _, p := range pending {
					caller := p.Device
					if p.Agent != "" {
						caller += fmt.Sprintf(" (agent %s)", p.Agent)
					}
					fmt.Fprintf(w, "%s  %s  %s: %s  [asked %s, expires in %s]\n", p.ID, caller, p.Class, oneLine(p.Title),
						ago(p.Created), time.Until(p.Expires).Round(time.Second))
					for _, d := range p.Details {
						fmt.Fprintf(w, "    %s: %s\n", oneLine(d.Label), oneLine(d.Value))
					}
					for i, s := range p.Scopes {
						mark := ""
						if s.Broad {
							mark = "  [broad]"
						}
						fmt.Fprintf(w, "    scope %d: %s%s\n", i, oneLine(s.Label), mark)
					}
					fmt.Fprintf(w, "    messh approvals allow|always|deny %s\n", p.ID)
				}
			})
		},
	})
	add(&Command{
		Name:     "approvals allow",
		Args:     []Arg{{Name: "ID", Help: "request ID from `messh approvals`"}},
		Summary:  "approve a waiting request, this time only",
		Help:     answerHelp + " Allow approves this request only; nothing is saved.",
		Output:   `{"id", "decision": "allow"}`,
		Mutates:  true,
		Examples: []string{"messh approvals allow 3fa85f64"},
		Run: func(c *Context) error {
			return answerApproval(c, control.DecisionAllow)
		},
	})
	add(&Command{
		Name:    "approvals always",
		Args:    []Arg{{Name: "ID", Help: "request ID from `messh approvals`"}},
		Summary: "approve a waiting request and save a rule for its scope",
		Help: answerHelp + " Always approves this request and saves a rule for scope `N` " +
			"(0 is the exact request; the list shows the rest), so later matching requests are allowed without asking.",
		Flags: func(fs *flag.FlagSet) {
			fs.Int("scope", 0, "which scope `N` to save (see the list; 0 is the exact request)")
		},
		Output:   `{"id", "decision": "always", "scope"}`,
		Mutates:  true,
		Examples: []string{"messh approvals always 3fa85f64", "messh approvals always 3fa85f64 --scope 1"},
		Run: func(c *Context) error {
			return answerApproval(c, control.DecisionAlways)
		},
	})
	add(&Command{
		Name:     "approvals deny",
		Args:     []Arg{{Name: "ID", Help: "request ID from `messh approvals`"}},
		Summary:  "deny a waiting request",
		Help:     answerHelp + " Deny refuses this request; nothing runs.",
		Output:   `{"id", "decision": "deny"}`,
		Mutates:  true,
		Examples: []string{"messh approvals deny 3fa85f64"},
		Run: func(c *Context) error {
			return answerApproval(c, control.DecisionDeny)
		},
	})
	add(&Command{
		Name:    "approvals test",
		Summary: "show a harmless test request the way real ones appear and report the choice",
		Help: "Shows a synthetic request on this node's real approval surface and reports what the person chose. " +
			"Nothing runs and no rule is saved.",
		Flags: func(fs *flag.FlagSet) {
			fs.Duration("timeout", 2*time.Minute, "wait up to `DURATION` for the person to answer")
		},
		Output:   "{id, outcome, allowed, scope, scope_label, surface, via, reason} (control.ApprovalTestResult)",
		Person:   "answer the test prompt on this device",
		Waits:    "up to --timeout for the person to answer",
		Examples: []string{"messh approvals test", "messh approvals test --timeout 30s"},
		Run:      runApprovalsTest,
	})
	add(&Command{
		Name:     "approvals unregister",
		Summary:  "remove the Windows toasts and the registry entries messh made for them",
		Help:     "Removes the notification registrations messh made on this device. On platforms where messh registers nothing it reports that.",
		Output:   `{"message"}`,
		Mutates:  true,
		Examples: []string{"messh approvals unregister"},
		Run: func(c *Context) error {
			msg, err := approval.UnregisterNotifications()
			if err != nil {
				return err
			}
			return c.Emit(map[string]string{"message": msg}, func(w io.Writer) {
				fmt.Fprintln(w, msg)
			})
		},
	})
	add(&Command{
		Name:    "rules",
		Summary: `list saved "always allow" rules`,
		Help: `Rules are saved by answering a request with "always". Each covers one calling device and agent, ` +
			`one class, and the requests matching its key. Delete one with ` + "`messh rules rm ID`.",
		Output:   "[{id, device_id, device, agent, class, key, label, broad, created}] (created RFC 3339)",
		Examples: []string{"messh rules", "messh rules --json"},
		Run: func(c *Context) error {
			cl, _, err := c.Node()
			if err != nil {
				return err
			}
			var rules []control.Rule
			if err := cl.Do(context.Background(), http.MethodGet, "/v1/rules", nil, &rules); err != nil {
				return err
			}
			if rules == nil {
				rules = []control.Rule{}
			}
			return c.Emit(rules, func(w io.Writer) {
				if len(rules) == 0 {
					fmt.Fprintln(w, `No saved rules. "Always allow" in an approval prompt creates one.`)
					return
				}
				tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "ID\tDEVICE\tAGENT\tCLASS\tALLOWS\tSAVED")
				for _, r := range rules {
					label := oneLine(r.Label)
					if r.Broad {
						label += "  [broad]"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.Device, r.Agent, r.Class, label, r.Created.Local().Format(time.DateTime))
				}
				tw.Flush()
			})
		},
	})
	add(&Command{
		Name:     "rules rm",
		Args:     []Arg{{Name: "ID", Help: "rule ID from `messh rules`"}},
		Summary:  "delete a saved rule",
		Output:   `{"id", "removed": true}`,
		Mutates:  true,
		Examples: []string{"messh rules rm 3fa85f64"},
		Run: func(c *Context) error {
			id := c.Args[0]
			cl, _, err := c.Node()
			if err != nil {
				return err
			}
			if err := cl.Do(context.Background(), http.MethodDelete, "/v1/rules/"+url.PathEscape(id), nil, nil); err != nil {
				return err
			}
			return c.Emit(map[string]any{"id": id, "removed": true}, func(w io.Writer) {
				fmt.Fprintln(w, "Removed rule", id)
			})
		},
	})
	add(&Command{
		Name:    "audit",
		Summary: "show recent approval decisions and call results",
		Flags: func(fs *flag.FlagSet) {
			fs.Int("n", 50, "show the last `N` records")
		},
		Output:   "[{time, kind, id, device_id, device, agent, tool, class, title, exact, decision, rule_id, auto, reason, surface, ok, error, duration_ms}] (time RFC 3339)",
		Examples: []string{"messh audit", "messh audit -n 10"},
		Run: func(c *Context) error {
			cl, _, err := c.Node()
			if err != nil {
				return err
			}
			var recs []control.AuditRecord
			if err := cl.Do(context.Background(), http.MethodGet, fmt.Sprintf("/v1/audit?n=%d", c.Int("n")), nil, &recs); err != nil {
				return err
			}
			if recs == nil {
				recs = []control.AuditRecord{}
			}
			return c.Emit(recs, func(w io.Writer) {
				if len(recs) == 0 {
					fmt.Fprintln(w, "The audit log is empty.")
					return
				}
				tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
				fmt.Fprintln(tw, "TIME\tID\tFROM\tTOOL\tRESULT\tDETAIL")
				for _, r := range recs {
					from := r.Device
					if r.Agent != "" {
						from += "/" + r.Agent
					}
					result, detail := r.Decision, r.Reason
					switch {
					case r.Kind == control.AuditCompletion && r.OK != nil && *r.OK:
						result, detail = "done", fmt.Sprintf("ok in %dms", r.DurationMS)
					case r.Kind == control.AuditCompletion:
						result, detail = "failed", r.Error
					case r.Auto != "":
						detail = "auto: " + r.Auto
					case r.RuleID != "":
						detail = "rule " + r.RuleID
					case r.Surface != "" && detail == "":
						detail = "via " + r.Surface
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.Time.Local().Format("01-02 15:04:05"), r.ID, from, r.Tool, result, oneLine(detail))
				}
				tw.Flush()
			})
		},
	})
}

// answerApproval decides one waiting request with decision (allow, always or
// deny) and reports what it did.
func answerApproval(c *Context, decision string) error {
	id := c.Args[0]
	body := control.ApprovalAnswer{Decision: decision}
	scope := 0
	if decision == control.DecisionAlways {
		scope = c.Int("scope")
		body.Scope = scope
	}
	cl, _, err := c.Node()
	if err != nil {
		return err
	}
	if err := cl.Do(context.Background(), http.MethodPost, "/v1/approvals/"+url.PathEscape(id), body, nil); err != nil {
		return err
	}
	past := map[string]string{
		control.DecisionAllow:  "allowed once",
		control.DecisionAlways: "allowed, rule saved",
		control.DecisionDeny:   "denied",
	}[decision]
	out := map[string]any{"id": id, "decision": decision}
	if decision == control.DecisionAlways {
		out["scope"] = scope
	}
	return c.Emit(out, func(w io.Writer) {
		fmt.Fprintf(w, "%s: %s\n", id, past)
	})
}

// runApprovalsTest shows a synthetic request on the node's real approval
// surface and reports what the person chose. Nothing runs and no rule is
// saved. The wait is bounded by --timeout.
func runApprovalsTest(c *Context) error {
	timeout := c.Duration("timeout")
	if timeout <= 0 {
		return usageErrorf("--timeout must be positive")
	}
	cl, _, err := c.Node()
	if err != nil {
		return err
	}
	cl.HTTP = &http.Client{Timeout: timeout + time.Minute}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if !c.JSON {
		fmt.Fprintf(c.Stdout, "Showing a test approval on this device. Answer it there; waiting up to %s (Ctrl-C withdraws it).\n", timeout)
	}
	var res control.ApprovalTestResult
	if err := cl.Do(ctx, http.MethodPost, "/v1/approvals/test", struct{}{}, &res); err != nil {
		return err
	}
	return c.Emit(res, func(w io.Writer) {
		switch res.Outcome {
		case approval.OutcomeAllowOnce:
			fmt.Fprintln(w, "You chose: Allow once.")
		case approval.OutcomeAllowAlways:
			fmt.Fprintf(w, "You chose: Always allow, scope %d (%s). A real request would have saved a rule; this test saved nothing.\n", res.Scope, oneLine(res.ScopeLabel))
		case approval.OutcomeDeny:
			fmt.Fprintln(w, "You chose: Deny.")
		case approval.OutcomeExpired:
			fmt.Fprintln(w, "No answer: the test request expired.")
		default:
			fmt.Fprintf(w, "The test request ended without an answer (%s: %s).\n", res.Outcome, oneLine(res.Reason))
		}
		if res.Surface != "" {
			fmt.Fprintf(w, "Answered on the %q surface (this node tries: %s).\n", res.Surface, res.Via)
		}
	})
}

// oneLine keeps remote-supplied text from breaking the layout of CLI output.
func oneLine(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}
