package node

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/schedule"
)

// shortSchedules lets tests use "in: 1s" instead of the 10 s minimum.
func shortSchedules(t *testing.T) {
	t.Helper()
	prev := scheduleStoreOptions
	scheduleStoreOptions.MinIn = time.Second
	t.Cleanup(func() { scheduleStoreOptions = prev })
}

// pairedAndOnline starts a desktop and a raspi, pairs them, and waits until
// the raspi has verified contact with the desktop and knows its tools.
func pairedAndOnline(t *testing.T) (desktop, raspi *Node) {
	t.Helper()
	desktop = startNode(t, "desktop")
	raspi = startNode(t, "raspi")
	pair(t, desktop, raspi)
	waitFor(t, "the desktop to be online on the raspi", func() bool {
		for _, d := range raspi.Nodes() {
			if d.ID == desktop.ID() && d.Online && slices.ContainsFunc(d.Tools, func(tool *mcp.Tool) bool { return tool.Name == "node_info" }) {
				return true
			}
		}
		return false
	})
	return desktop, raspi
}

// agentAs connects to n's agent endpoint with a registered agent's token.
func agentAs(t *testing.T, n *Node, agent string) *mcp.ClientSession {
	t.Helper()
	tok, err := n.paths.AddAgent(agent)
	if err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: agent, Version: "1"}, nil)
	s, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:             "http://" + n.LocalAddr() + "/mcp",
		HTTPClient:           &http.Client{Transport: authHeader("Bearer " + tok)},
		MaxRetries:           -1,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// callJSON calls a tool and, unless it reports an error, decodes its text into out.
func callJSON(t *testing.T, s *mcp.ClientSession, tool string, args map[string]any, out any) *mcp.CallToolResult {
	t.Helper()
	res, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if !res.IsError && out != nil {
		if err := json.Unmarshal([]byte(resultText(res)), out); err != nil {
			t.Fatalf("%s: decode %q: %v", tool, resultText(res), err)
		}
	}
	return res
}

func waitForRuns(t *testing.T, n *Node, id string, runs int) schedule.Schedule {
	t.Helper()
	var s schedule.Schedule
	waitFor(t, "schedule "+id+" to run", func() bool {
		var err error
		s, err = n.sched.store.Get("", id)
		return err == nil && len(s.Runs) >= runs && !s.Running
	})
	return s
}

func answeredBy(t *testing.T, r schedule.Run, want string) {
	t.Helper()
	if !r.OK || r.Error != "" {
		t.Fatalf("run failed: %+v", r)
	}
	var info struct{ ID string }
	if err := json.Unmarshal(r.Result, &info); err != nil || info.ID != want {
		t.Fatalf("run result %s (%v), want node_info of %s", r.Result, err, want)
	}
}

func TestScheduledCallRunsOnPeer(t *testing.T) {
	shortSchedules(t)
	desktop, raspi := pairedAndOnline(t)
	omp := agentAs(t, raspi, "omp")

	var added struct {
		ID   string
		Next time.Time
		When string
	}
	res := callJSON(t, omp, "mesh_schedule", map[string]any{"device": "desktop", "tool": "node_info", "in": "1s", "wake": false}, &added)
	if res.IsError || added.ID == "" || time.Until(added.Next) > 2*time.Second || added.When != "in 1s" {
		t.Fatalf("mesh_schedule: %s", resultText(res))
	}

	s := waitForRuns(t, raspi, added.ID, 1)
	answeredBy(t, s.Runs[0], desktop.ID())
	if r := s.Runs[0]; r.Woke || r.Missed || r.Manual || r.Started.IsZero() || r.Finished.Before(r.Started) {
		t.Fatalf("run record %+v", r)
	}
	if s.Enabled || !s.Next.IsZero() || s.Agent != "omp" || s.Device != desktop.ID() {
		t.Fatalf("one-shot after its run: %+v", s)
	}

	var view scheduleView
	callJSON(t, omp, "mesh_schedules", map[string]any{"id": added.ID}, &view)
	if view.State != "done" || len(view.RecentRuns) != 1 || !view.RecentRuns[0].OK || view.Device != "desktop" {
		t.Fatalf("mesh_schedules {id}: %+v", view)
	}

	// run_now repeats it on demand and is recorded as manual.
	if res := callJSON(t, omp, "mesh_schedule_run_now", map[string]any{"id": added.ID}, nil); res.IsError {
		t.Fatalf("run_now: %s", resultText(res))
	}
	s = waitForRuns(t, raspi, added.ID, 2)
	answeredBy(t, s.Runs[1], desktop.ID())
	if !s.Runs[1].Manual {
		t.Fatalf("run_now record %+v", s.Runs[1])
	}
}

func TestScheduledCallWakesRemoteTargetsOnly(t *testing.T) {
	shortSchedules(t)
	desktop, raspi := pairedAndOnline(t)

	var mu sync.Mutex
	var woken []string
	var waits []time.Duration
	raspi.sched.wake = func(_ context.Context, id string, wait time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		woken, waits = append(woken, id), append(waits, wait)
		return nil
	}

	add := func(device, name string, wake bool) string {
		s, err := raspi.sched.store.Add(schedule.Schedule{Agent: "omp", Device: device, DeviceName: name, Tool: "node_info",
			Spec: schedule.Spec{In: "1s"}, Wake: wake})
		if err != nil {
			t.Fatal(err)
		}
		return s.ID
	}
	wakeRemote := add(desktop.ID(), "desktop", true)
	noWake := add(desktop.ID(), "desktop", false)
	local := add(raspi.ID(), "raspi", true)

	answeredBy(t, waitForRuns(t, raspi, wakeRemote, 1).Runs[0], desktop.ID())
	answeredBy(t, waitForRuns(t, raspi, noWake, 1).Runs[0], desktop.ID())
	answeredBy(t, waitForRuns(t, raspi, local, 1).Runs[0], raspi.ID())

	mu.Lock()
	defer mu.Unlock()
	if len(woken) != 1 || woken[0] != desktop.ID() || waits[0] != scheduleWakeWait {
		t.Fatalf("waker calls %v (waits %v), want one for the desktop with %s", woken, waits, scheduleWakeWait)
	}
}

func TestScheduledCallToUnpairedDeviceFails(t *testing.T) {
	shortSchedules(t)
	raspi := startNode(t, "raspi")
	s, err := raspi.sched.store.Add(schedule.Schedule{Agent: "omp", Device: strings.Repeat("ab", 32), DeviceName: "desktop",
		Tool: "node_info", Spec: schedule.Spec{In: "1s"}, Wake: true})
	if err != nil {
		t.Fatal(err)
	}
	r := waitForRuns(t, raspi, s.ID, 1).Runs[0]
	if r.OK || !strings.Contains(r.Error, "no longer paired") {
		t.Fatalf("run against an unpaired device: %+v", r)
	}
	if got, err := raspi.sched.store.Get("", s.ID); err != nil || got.ID != s.ID {
		t.Fatal("the schedule was dropped after its device went away")
	}
}

func TestScheduleToolsIsolateAgents(t *testing.T) {
	raspi := startNode(t, "raspi")
	omp := agentAs(t, raspi, "omp")
	pi := agentAs(t, raspi, "pi")

	var added struct{ ID string }
	res := callJSON(t, omp, "mesh_schedule", map[string]any{"device": "raspi", "tool": "raspi__node_info", "every": "1h", "label": "hourly info"}, &added)
	if res.IsError {
		t.Fatalf("mesh_schedule: %s", resultText(res))
	}
	if s, _ := raspi.sched.store.Get("omp", added.ID); s.Tool != "node_info" || s.Device != raspi.ID() || !s.Wake {
		t.Fatalf("stored %+v, want node_info on the raspi with wake defaulting to true", s)
	}

	var list struct{ Schedules []scheduleView }
	callJSON(t, pi, "mesh_schedules", nil, &list)
	if len(list.Schedules) != 0 {
		t.Fatalf("pi sees %+v", list.Schedules)
	}
	for tool, args := range map[string]map[string]any{
		"mesh_schedules":        {"id": added.ID},
		"mesh_unschedule":       {"id": added.ID},
		"mesh_schedule_pause":   {"id": added.ID, "paused": true},
		"mesh_schedule_run_now": {"id": added.ID},
	} {
		if res := callJSON(t, pi, tool, args, nil); !res.IsError {
			t.Fatalf("pi's %s on omp's schedule succeeded: %s", tool, resultText(res))
		}
	}
	s, err := raspi.sched.store.Get("omp", added.ID)
	if err != nil || s.Paused || len(s.Runs) != 0 {
		t.Fatalf("omp's schedule after pi's attempts: %+v, %v", s, err)
	}
	callJSON(t, omp, "mesh_schedules", nil, &list)
	if len(list.Schedules) != 1 || list.Schedules[0].ID != added.ID || list.Schedules[0].Label != "hourly info" || list.Schedules[0].State != "scheduled" {
		t.Fatalf("omp sees %+v", list.Schedules)
	}

	for what, args := range map[string]map[string]any{
		"exactly one":     {"device": "raspi", "tool": "node_info", "in": "1m", "every": "1h"},
		"has no tool":     {"device": "raspi", "tool": "no_such_tool", "in": "1m"},
		"no device":       {"device": "toaster", "tool": "node_info", "in": "1m"},
		"unknown field":   {"device": "raspi", "tool": "node_info", "when": "1m"},
		"JSON object":     {"device": "raspi", "tool": "node_info", "in": "1m", "arguments": "{}"},
		"at least 10s":    {"device": "raspi", "tool": "node_info", "in": "1s"},
		"device and tool": {"tool": "node_info", "in": "1m"},
	} {
		res := callJSON(t, omp, "mesh_schedule", args, nil)
		if !res.IsError || !strings.Contains(resultText(res), what) {
			t.Errorf("mesh_schedule %v: %s, want an error about %q", args, resultText(res), what)
		}
	}

	// The owner's control API sees every agent's schedules.
	callJSON(t, pi, "mesh_schedule", map[string]any{"device": "raspi", "tool": "node_info", "daily": "07:00"}, nil)
	var all []schedule.Schedule
	controlAPI(t, raspi, http.MethodGet, "/v1/schedules", nil, &all)
	if len(all) != 2 {
		t.Fatalf("control API lists %d schedules, want both agents' 2", len(all))
	}
	var paused schedule.Schedule
	controlAPI(t, raspi, http.MethodPost, "/v1/schedules/"+added.ID+"/pause", schedule.PauseRequest{Paused: true}, &paused)
	if !paused.Paused || paused.ID != added.ID {
		t.Fatalf("control pause: %+v", paused)
	}

	if res := callJSON(t, omp, "mesh_unschedule", map[string]any{"id": added.ID}, nil); res.IsError {
		t.Fatalf("mesh_unschedule: %s", resultText(res))
	}
	callJSON(t, omp, "mesh_schedules", nil, &list)
	if len(list.Schedules) != 0 {
		t.Fatalf("omp still sees %+v", list.Schedules)
	}
}

// controlAPI calls n's control API with the control token.
func controlAPI(t *testing.T, n *Node, method, path string, in, out any) {
	t.Helper()
	var body bytes.Buffer
	if in != nil {
		json.NewEncoder(&body).Encode(in)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+n.LocalAddr()+path, &body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+n.controlToken)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		t.Fatalf("%s %s: %s", method, path, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		t.Fatalf("%s %s: decode: %v", method, path, err)
	}
}

func TestScheduledJobRequestIDIsOccurrenceScoped(t *testing.T) {
	args := json.RawMessage(`{"command":"echo","request_id":"static-key"}`)
	first, err := scheduledJobArguments(schedule.Fire{Schedule: schedule.Schedule{Arguments: args}, OperationID: "op-one"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := scheduledJobArguments(schedule.Fire{Schedule: schedule.Schedule{Arguments: args}, OperationID: "op-two"})
	if err != nil {
		t.Fatal(err)
	}
	var a, b map[string]json.RawMessage
	if err := json.Unmarshal(first, &a); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(second, &b); err != nil {
		t.Fatal(err)
	}
	var firstID, secondID string
	if err := json.Unmarshal(a["request_id"], &firstID); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b["request_id"], &secondID); err != nil {
		t.Fatal(err)
	}
	if firstID != "schedule-op-one" || secondID != "schedule-op-two" || firstID == secondID {
		t.Fatalf("request ids = %q, %q", firstID, secondID)
	}
}
