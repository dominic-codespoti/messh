package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"messh/internal/schedule"
)

// scheduleOutput describes one schedule.Schedule as --json prints it.
const scheduleOutput = "{id, agent, label, device, device_name, tool, arguments, spec{at, in, every, daily, weekdays}, wake, created, next, paused, enabled, runs[{scheduled, started, finished, ok, error, result, woke, missed, manual}], running} (times RFC 3339)"

func init() {
	add(&Command{
		Name:     "schedule",
		Summary:  "list every agent's scheduled calls on this node",
		Output:   "[" + scheduleOutput + "]",
		Examples: []string{"messh schedule", "messh schedule --json"},
		Run: func(c *Context) error {
			return listSchedules(c)
		},
	})
	add(&Command{
		Name:     "schedule ls",
		Summary:  "list every agent's scheduled calls on this node",
		Output:   "[" + scheduleOutput + "]",
		Examples: []string{"messh schedule ls"},
		Run: func(c *Context) error {
			return listSchedules(c)
		},
	})
	add(&Command{
		Name:     "schedule show",
		Args:     []Arg{{Name: "ID", Help: "schedule ID from `messh schedule ls`"}},
		Summary:  "show one schedule: its call, arguments and recent runs",
		Output:   scheduleOutput,
		Examples: []string{"messh schedule show 3fa85f64"},
		Run: func(c *Context) error {
			cl, _, err := c.Node()
			if err != nil {
				return err
			}
			var s schedule.Schedule
			if err := cl.Do(context.Background(), http.MethodGet, "/v1/schedules/"+url.PathEscape(c.Args[0]), nil, &s); err != nil {
				return err
			}
			return c.Emit(s, func(w io.Writer) { writeScheduleShow(w, s) })
		},
	})
	add(&Command{
		Name:     "schedule rm",
		Args:     []Arg{{Name: "ID", Help: "schedule ID from `messh schedule ls`"}},
		Summary:  "delete a schedule",
		Output:   "the deleted " + scheduleOutput,
		Mutates:  true,
		Examples: []string{"messh schedule rm 3fa85f64"},
		Run: func(c *Context) error {
			cl, _, err := c.Node()
			if err != nil {
				return err
			}
			var s schedule.Schedule
			if err := cl.Do(context.Background(), http.MethodDelete, "/v1/schedules/"+url.PathEscape(c.Args[0]), nil, &s); err != nil {
				return err
			}
			return c.Emit(s, func(w io.Writer) {
				fmt.Fprintf(w, "Removed schedule %s (%s %s on %s, agent %s)\n", s.ID, s.Spec, s.Tool, s.DeviceName, s.Agent)
			})
		},
	})
	add(&Command{
		Name:     "schedule pause",
		Args:     []Arg{{Name: "ID", Help: "schedule ID from `messh schedule ls`"}},
		Summary:  "stop a schedule firing until resumed",
		Output:   "the updated " + scheduleOutput,
		Mutates:  true,
		Examples: []string{"messh schedule pause 3fa85f64"},
		Run: func(c *Context) error {
			return setSchedulePaused(c, true)
		},
	})
	add(&Command{
		Name:     "schedule resume",
		Args:     []Arg{{Name: "ID", Help: "schedule ID from `messh schedule ls`"}},
		Summary:  "fire a paused schedule again from its next future time",
		Output:   "the updated " + scheduleOutput,
		Mutates:  true,
		Examples: []string{"messh schedule resume 3fa85f64"},
		Run: func(c *Context) error {
			return setSchedulePaused(c, false)
		},
	})
	add(&Command{
		Name:     "schedule run",
		Args:     []Arg{{Name: "ID", Help: "schedule ID from `messh schedule ls`"}},
		Summary:  "run a schedule now, in addition to its normal times",
		Output:   "the updated " + scheduleOutput,
		Mutates:  true,
		Examples: []string{"messh schedule run 3fa85f64"},
		Run: func(c *Context) error {
			cl, _, err := c.Node()
			if err != nil {
				return err
			}
			id := c.Args[0]
			var s schedule.Schedule
			if err := cl.Do(context.Background(), http.MethodPost, "/v1/schedules/"+url.PathEscape(id)+"/run", nil, &s); err != nil {
				return err
			}
			return c.Emit(s, func(w io.Writer) {
				fmt.Fprintf(w, "Started %s (%s on %s); `messh schedule show %s` shows the result when it finishes.\n", s.ID, s.Tool, s.DeviceName, s.ID)
			})
		},
	})
}

func listSchedules(c *Context) error {
	cl, _, err := c.Node()
	if err != nil {
		return err
	}
	var list []schedule.Schedule
	if err := cl.Do(context.Background(), http.MethodGet, "/v1/schedules", nil, &list); err != nil {
		return err
	}
	if list == nil {
		list = []schedule.Schedule{}
	}
	return c.Emit(list, func(w io.Writer) {
		if len(list) == 0 {
			fmt.Fprintln(w, "No schedules. Agents create them with the mesh_schedule tool.")
			return
		}
		tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tAGENT\tDEVICE\tTOOL\tWHEN\tNEXT\tSTATE\tLAST RUN")
		for _, s := range list {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", s.ID, s.Agent, oneLine(s.DeviceName), s.Tool, s.Spec,
				nextText(s), scheduleState(s), lastRunText(s))
		}
		tw.Flush()
	})
}

func setSchedulePaused(c *Context, paused bool) error {
	cl, _, err := c.Node()
	if err != nil {
		return err
	}
	id := c.Args[0]
	var s schedule.Schedule
	if err := cl.Do(context.Background(), http.MethodPost, "/v1/schedules/"+url.PathEscape(id)+"/pause", schedule.PauseRequest{Paused: paused}, &s); err != nil {
		return err
	}
	return c.Emit(s, func(w io.Writer) {
		if s.Paused {
			fmt.Fprintf(w, "Paused %s; `messh schedule resume %s` continues it.\n", s.ID, s.ID)
		} else {
			fmt.Fprintf(w, "Resumed %s; next run %s.\n", s.ID, nextText(s))
		}
	})
}

func writeScheduleShow(w io.Writer, s schedule.Schedule) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	row := func(k, v string) { fmt.Fprintf(tw, "%s\t%s\n", k, v) }
	row("ID", s.ID)
	row("Agent", s.Agent)
	if s.Label != "" {
		row("Label", oneLine(s.Label))
	}
	row("Device", fmt.Sprintf("%s (%s)", oneLine(s.DeviceName), s.Device))
	row("Tool", s.Tool)
	row("When", s.Spec.String())
	row("Next", nextText(s))
	row("State", scheduleState(s))
	row("Wake", map[bool]string{true: "yes, Wake-on-LAN before each run if offline", false: "no"}[s.Wake])
	row("Created", s.Created.Local().Format(time.DateTime))
	tw.Flush()
	if len(s.Arguments) > 0 {
		var b bytes.Buffer
		if json.Indent(&b, s.Arguments, "  ", "  ") == nil {
			fmt.Fprintf(w, "Arguments:\n  %s\n", b.String())
		}
	}
	if len(s.Runs) == 0 {
		fmt.Fprintln(w, "No runs yet.")
		return
	}
	fmt.Fprintln(w, "Runs, newest first:")
	for i := len(s.Runs) - 1; i >= 0; i-- {
		r := s.Runs[i]
		fmt.Fprintf(w, "  %s  %s\n", r.Started.Local().Format(time.DateTime), runSummary(r))
		if len(r.Result) > 0 {
			// Stored results are JSON, so control characters arrive escaped.
			fmt.Fprintf(w, "    result: %s\n", schedule.TruncateResult(r.Result, 2000))
		}
	}
}

func nextText(s schedule.Schedule) string {
	switch {
	case !s.Enabled || s.Next.IsZero():
		return "-"
	case s.Paused:
		return "paused"
	}
	in := time.Until(s.Next).Round(time.Second)
	if in < 0 {
		in = 0
	}
	return fmt.Sprintf("%s (in %s)", s.Next.Local().Format("2006-01-02 15:04"), in)
}

func scheduleState(s schedule.Schedule) string {
	switch {
	case s.Running:
		return "running"
	case !s.Enabled:
		return "done"
	case s.Paused:
		return "paused"
	}
	return "scheduled"
}

func lastRunText(s schedule.Schedule) string {
	if len(s.Runs) == 0 {
		return "-"
	}
	r := s.Runs[len(s.Runs)-1]
	return ago(r.Finished) + ": " + oneLine(runSummary(r))
}

func runSummary(r schedule.Run) string {
	var tags []string
	if r.Manual {
		tags = append(tags, "run now")
	}
	if r.Woke {
		tags = append(tags, "woke the device")
	}
	if r.Missed {
		tags = append(tags, "missed")
	}
	status := "ok"
	if !r.OK {
		status = "failed: " + r.Error
		if r.Missed && r.Finished.Equal(r.Started) {
			status = r.Error // not run at all
		}
	} else {
		status += " in " + r.Finished.Sub(r.Started).Round(time.Millisecond).String()
	}
	if len(tags) > 0 {
		status += " [" + strings.Join(tags, ", ") + "]"
	}
	return status
}
