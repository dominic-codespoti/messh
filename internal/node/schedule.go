package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/gateway"
	"messh/internal/identity"
	"messh/internal/provider"
	"messh/internal/schedule"
)

const (
	scheduleWakeWait    = 3 * time.Minute  // how long a fire waits for a woken device
	scheduleCallTimeout = 10 * time.Minute // per call, including an approval prompt on the host
	scheduleRecheck     = 30 * time.Second // re-evaluate even if the timer is off (clock jumps, suspend)
	scheduleParallel    = 4                // calls running at once
	listResultMax       = 1 << 10          // bytes of a result shown in a list
)

// scheduleStoreOptions configures the store; tests shorten MinIn.
var scheduleStoreOptions schedule.Options

// scheduler runs the stored calls of this node's agents. The schedules live
// on the calling node (the always-on device); at fire time it wakes the
// target and calls the tool as the agent that made the schedule, so the
// target's approval gate and saved rules apply exactly as for a direct call.
type scheduler struct {
	n     *Node
	store *schedule.Store
	sem   chan struct{}
	// wake is n.Wake; tests replace it so they do not depend on Wake-on-LAN.
	wake func(ctx context.Context, deviceID string, wait time.Duration) error
}

// startSchedules loads the schedules, adds the mesh_schedule* tools and
// starts the runner. It needs n.gateway and n.files. An unreadable file is
// moved aside so the owner can inspect it; scheduling stays off only when
// even that fails.
func (n *Node) startSchedules() {
	path := n.paths.SchedulesFile()
	st, err := schedule.Open(path, scheduleStoreOptions)
	if errors.Is(err, schedule.ErrCorrupt) {
		aside := path + ".unreadable-" + time.Now().Format("20060102-150405")
		if rerr := os.Rename(path, aside); rerr == nil {
			n.log.Error("schedules file is unreadable; moved it aside and started with no schedules", "file", aside, "error", err)
			st, err = schedule.Open(path, scheduleStoreOptions)
		}
	}
	if err != nil {
		n.log.Error("scheduling is off: cannot load schedules", "file", path, "error", err)
		return
	}
	sc := &scheduler{n: n, store: st, sem: make(chan struct{}, scheduleParallel), wake: n.Wake}
	n.sched = sc
	n.gateway.AddTool(meshScheduleTool(), sc.toolSchedule)
	n.gateway.AddTool(meshSchedulesTool(), sc.toolSchedules)
	n.gateway.AddTool(meshUnscheduleTool(), sc.toolUnschedule)
	n.gateway.AddTool(meshSchedulePauseTool(), sc.toolPause)
	n.gateway.AddTool(meshScheduleRunNowTool(), sc.toolRunNow)
	n.goRun(sc.run)
}

// run fires due schedules. Besides the timer to the earliest fire it
// re-checks every scheduleRecheck, because timers follow the monotonic clock
// and fall behind wall-clock time across a system suspend or clock change.
func (sc *scheduler) run() {
	ctx := sc.n.ctx
	recheck := time.NewTicker(scheduleRecheck)
	defer recheck.Stop()
	timer := time.NewTimer(scheduleRecheck)
	defer timer.Stop()
	for {
		admitted := sc.runDue()
		wait := scheduleRecheck
		if admitted {
			wait = time.Hour
			if next, ok := sc.store.NextWake(); ok {
				wait = max(min(time.Until(next), wait), 0)
			}
		}
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-recheck.C:
		case <-sc.store.Changed():
		}
	}
}

// Claiming a due fire is work too: hold admission until each claimed fire has
// acquired its own lifetime reference, including time waiting for the semaphore.
func (sc *scheduler) runDue() bool {
	if !sc.n.beginWork() {
		return false
	}
	defer sc.n.endWork()
	fires, err := sc.store.Due(time.Now())
	if err != nil {
		sc.n.log.Error("save schedules", "error", err)
	}
	for _, f := range fires {
		sc.start(f)
	}
	return true
}

func (sc *scheduler) start(f schedule.Fire) {
	// All callers retain admission while claiming and handing off the fire.
	sc.n.maintenance.mu.Lock()
	sc.n.maintenance.active++
	sc.n.maintenance.mu.Unlock()
	sc.n.goRun(func() {
		defer sc.n.endWork()
		sc.execute(f)
	})
}

// execute performs one fire and records it. Arguments are never logged: they
// may hold prompts or other private text.
func (sc *scheduler) execute(f schedule.Fire) {
	n := sc.n
	run := schedule.Run{Scheduled: f.Scheduled, Missed: f.Missed, Manual: f.Manual}
	select {
	case sc.sem <- struct{}{}:
		defer func() { <-sc.sem }()
		run.Started = time.Now()
		run.Woke, run.OK, run.Error, run.Result = sc.perform(f)
	case <-n.ctx.Done():
		run.Started = time.Now()
		run.Error = "not run: the node stopped"
	}
	run.Finished = time.Now()
	n.log.Info("scheduled call", "schedule", f.ID, "agent", f.Agent, "device", f.DeviceName, "tool", f.Tool,
		"ok", run.OK, "woke", run.Woke, "missed", run.Missed, "manual", run.Manual,
		"took", run.Finished.Sub(run.Started).Round(time.Millisecond), "error", oneLineLimit(run.Error, 200))
	if err := sc.store.Record(f.ID, run); err != nil {
		n.log.Error("save schedules", "error", err)
	}
}

func (sc *scheduler) perform(f schedule.Fire) (woke, ok bool, errText string, result json.RawMessage) {
	n := sc.n
	local := f.Device == n.id.ID
	if !local {
		if _, paired := n.roster.Get(f.Device); !paired {
			return false, false, fmt.Sprintf("device %s (%s) is no longer paired with %s; remove this schedule with mesh_unschedule",
				f.DeviceName, identity.Short(f.Device), n.name), nil
		}
	}
	var wakeErr error
	if f.Wake && !local {
		wasOnline := time.Since(n.peers.lastContact(f.Device)) < onlineWindow
		wakeErr = sc.wake(n.ctx, f.Device, scheduleWakeWait)
		woke = !wasOnline && wakeErr == nil
	}
	// The call is tried even if waking failed: the device may be up anyway.
	ctx, cancel := context.WithTimeout(n.ctx, scheduleCallTimeout)
	defer cancel()
	res, err := n.Call(ctx, f.Device, f.Tool, f.Arguments, f.Agent)
	switch {
	case errors.Is(err, context.DeadlineExceeded) || err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded):
		errText = fmt.Sprintf("%s on %s did not finish within %s", f.Tool, f.DeviceName, scheduleCallTimeout)
	case err != nil && n.ctx.Err() != nil:
		errText = "interrupted: the node stopped"
	case err != nil:
		errText = err.Error()
	case res == nil:
		errText = "the call returned no result"
	case res.IsError:
		errText = oneLineLimit(contentText(res), 4<<10)
	default:
		ok, result = true, resultJSON(res)
	}
	if !ok && wakeErr != nil {
		errText = fmt.Sprintf("waking %s failed (%v); then: %s", f.DeviceName, wakeErr, errText)
	}
	return woke, ok, errText, result
}

// resultJSON keeps what an agent needs from a result: its structured content
// when there is one, otherwise its text.
func resultJSON(res *mcp.CallToolResult) json.RawMessage {
	if res.StructuredContent != nil {
		if data, err := marshalNoHTML(res.StructuredContent); err == nil {
			return data
		}
	}
	data, _ := marshalNoHTML(contentText(res))
	return data
}

func contentText(res *mcp.CallToolResult) string {
	var parts []string
	for _, c := range res.Content {
		switch c := c.(type) {
		case *mcp.TextContent:
			parts = append(parts, c.Text)
		case *mcp.ImageContent:
			parts = append(parts, fmt.Sprintf("[image %s, %d bytes]", c.MIMEType, len(c.Data)))
		case *mcp.AudioContent:
			parts = append(parts, fmt.Sprintf("[audio %s, %d bytes]", c.MIMEType, len(c.Data)))
		case *mcp.ResourceLink:
			parts = append(parts, "[resource "+c.URI+"]")
		case *mcp.EmbeddedResource:
			if c.Resource != nil {
				parts = append(parts, "[resource "+c.Resource.URI+"]")
			}
		}
	}
	return strings.Join(parts, "\n")
}

func marshalNoHTML(v any) (json.RawMessage, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n")), nil
}

func oneLineLimit(s string, limit int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > limit {
		for limit > 0 && !utf8.RuneStart(s[limit]) {
			limit--
		}
		s = s[:limit] + "..."
	}
	return s
}

// Agent tools.

const scheduleApprovalNote = " The call still passes the target device's approval gate when it runs, exactly like a direct call: " +
	"unless its owner has chosen 'Always allow' for that operation, a prompt appears on that device at run time and the run " +
	"waits for it (and fails if nobody answers). For runs while nobody is at the device, ask the owner to 'Always allow' " +
	"the operation once, for example by making the same call directly now."

func meshScheduleTool() *mcp.Tool {
	return &mcp.Tool{
		Name: "mesh_schedule",
		Description: "Schedule a tool call on a mesh device for later, once or repeatedly. The schedule lives on this " +
			"device, which stays on; when it is due it wakes the target with Wake-on-LAN if needed (wake, default true), " +
			"then calls the tool as you and keeps the result: read it later with mesh_schedules. Give exactly one of " +
			"at, in, every, daily. Returns {id, next, when}." + scheduleApprovalNote,
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"device":{"type":"string","description":"Target device: handle, name or ID prefix from mesh_nodes (this device works too)."},` +
			`"tool":{"type":"string","description":"Tool name on that device, e.g. job_submit; the \"<device>__\" prefix is optional."},` +
			`"arguments":{"type":"object","description":"The tool's arguments, exactly as for a direct call. Stored as given."},` +
			`"at":{"type":"string","description":"Once, at a time: RFC 3339 (2026-10-02T02:00:00+02:00) or \"YYYY-MM-DD HH:MM\" in this device's local time."},` +
			`"in":{"type":"string","description":"Once, after a Go duration from now, at least 10s (90s, 45m, 2h30m)."},` +
			`"every":{"type":"string","description":"Repeatedly, every Go duration of at least 1m (15m, 6h); the first run is one interval from now. Elapsed time, not wall clock."},` +
			`"daily":{"type":"string","description":"Repeatedly, every day at HH:MM local wall-clock time (follows daylight saving)."},` +
			`"weekdays":{"type":"array","items":{"type":"string","enum":["mon","tue","wed","thu","fri","sat","sun"]},"description":"With daily: only on these days."},` +
			`"wake":{"type":"boolean","description":"Wake the device with Wake-on-LAN first if it is offline (default true)."},` +
			`"label":{"type":"string","description":"Short note shown in listings."}` +
			`},"required":["device","tool"],"additionalProperties":false}`),
		Annotations: &mcp.ToolAnnotations{Title: "Schedule a call", OpenWorldHint: new(true)},
	}
}

func meshSchedulesTool() *mcp.Tool {
	return &mcp.Tool{
		Name: "mesh_schedules",
		Description: "List your schedules (oldest first) with their next run and last result (shortened), or give id for " +
			"one schedule with its arguments and up to 20 recent runs, newest first, with full results (up to 64 KiB each). " +
			"Runs record ok/error, whether the device had to be woken, and missed runs (this device was off or asleep at " +
			"the time). Only your own schedules are visible.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"id":{"type":"string","description":"A schedule id from mesh_schedule or this list; omit to list all of yours."}` +
			`},"additionalProperties":false}`),
		Annotations: &mcp.ToolAnnotations{Title: "Scheduled calls", ReadOnlyHint: true, IdempotentHint: true},
	}
}

func meshUnscheduleTool() *mcp.Tool {
	return &mcp.Tool{
		Name:        "mesh_unschedule",
		Description: "Delete one of your schedules and its run history. A run in progress finishes but is not recorded.",
		InputSchema: idSchema(""),
		Annotations: &mcp.ToolAnnotations{Title: "Delete a schedule", DestructiveHint: new(true), IdempotentHint: true},
	}
}

func meshSchedulePauseTool() *mcp.Tool {
	return &mcp.Tool{
		Name: "mesh_schedule_pause",
		Description: "Pause (paused: true) or resume (paused: false) one of your schedules. A resumed repeating schedule " +
			"continues at its next future time; times that passed while paused are skipped. A one-shot resumed after its " +
			"time runs at once if that time is less than an hour ago, otherwise it is recorded as missed.",
		InputSchema: idSchema(`,"paused":{"type":"boolean","description":"true to pause, false to resume."}`, "paused"),
		Annotations: &mcp.ToolAnnotations{Title: "Pause or resume a schedule", DestructiveHint: new(false), IdempotentHint: true},
	}
}

func meshScheduleRunNowTool() *mcp.Tool {
	return &mcp.Tool{
		Name: "mesh_schedule_run_now",
		Description: "Run one of your schedules now, in addition to its normal times (which do not change); works on paused " +
			"and finished schedules too. Returns at once; read the result with mesh_schedules {id}. Waking and approval " +
			"work as for a scheduled run.",
		InputSchema: idSchema(""),
		Annotations: &mcp.ToolAnnotations{Title: "Run a schedule now", OpenWorldHint: new(true)},
	}
}

// idSchema is an object schema with a required schedule id plus extra
// properties (a JSON fragment starting with a comma) and required names.
func idSchema(extra string, required ...string) json.RawMessage {
	req, _ := json.Marshal(append([]string{"id"}, required...))
	return json.RawMessage(`{"type":"object","properties":{"id":{"type":"string","description":"The schedule id."}` +
		extra + `},"required":` + string(req) + `,"additionalProperties":false}`)
}

type scheduleArgs struct {
	Device    string          `json:"device"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	At        string          `json:"at"`
	In        string          `json:"in"`
	Every     string          `json:"every"`
	Daily     string          `json:"daily"`
	Weekdays  []string        `json:"weekdays"`
	Wake      *bool           `json:"wake"`
	Label     string          `json:"label"`
}

type scheduleAdded struct {
	ID      string    `json:"id"`
	Next    time.Time `json:"next"`
	When    string    `json:"when"`
	Warning string    `json:"warning,omitempty"`
}

// scheduleView is a schedule as agents see it.
type scheduleView struct {
	ID         string          `json:"id"`
	Label      string          `json:"label,omitempty"`
	Device     string          `json:"device"` // current handle, or the stored name if it is no longer paired
	DeviceID   string          `json:"device_id"`
	Tool       string          `json:"tool"`
	When       string          `json:"when"`
	Next       time.Time       `json:"next,omitzero"`
	State      string          `json:"state"` // scheduled, running, paused, done
	Wake       bool            `json:"wake"`
	Created    time.Time       `json:"created"`
	Runs       int             `json:"runs"`
	LastRun    *schedule.Run   `json:"last_run,omitempty"`
	Arguments  json.RawMessage `json:"arguments,omitempty"`
	RecentRuns []schedule.Run  `json:"recent_runs,omitempty"`
}

func (sc *scheduler) view(s schedule.Schedule, handles map[string]string, detail bool) scheduleView {
	v := scheduleView{
		ID: s.ID, Label: s.Label, DeviceID: s.Device, Tool: s.Tool, When: s.Spec.String(), Next: s.Next,
		Wake: s.Wake, Created: s.Created, Runs: len(s.Runs),
	}
	v.Device = handles[s.Device]
	if v.Device == "" {
		v.Device = s.DeviceName + " (no longer paired)"
	}
	switch {
	case s.Running:
		v.State = "running"
	case !s.Enabled:
		v.State = "done"
	case s.Paused:
		v.State = "paused"
	default:
		v.State = "scheduled"
	}
	if detail {
		v.Arguments = s.Arguments
		v.RecentRuns = slices.Clone(s.Runs)
		slices.Reverse(v.RecentRuns)
	} else if len(s.Runs) > 0 {
		last := s.Runs[len(s.Runs)-1]
		last.Result = schedule.TruncateResult(last.Result, listResultMax)
		v.LastRun = &last
	}
	return v
}

func (sc *scheduler) handles() map[string]string { return gateway.Handles(sc.n.Nodes()) }

func requestAgent(req *mcp.CallToolRequest) string {
	if req.Extra != nil && req.Extra.TokenInfo != nil {
		return req.Extra.TokenInfo.UserID
	}
	return ""
}

// toolArgs decodes a tool's arguments strictly, so a misspelt field (say
// "when" for "at") is an error rather than silently ignored.
func toolArgs(req *mcp.CallToolRequest, v any) error {
	raw := bytes.TrimSpace(req.Params.Arguments)
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

// agentCall checks the caller and decodes its arguments; a non-nil result is
// the error to return.
func agentCall(name string, req *mcp.CallToolRequest, v any) (string, *mcp.CallToolResult) {
	agent := requestAgent(req)
	if agent == "" {
		return "", provider.ErrorResult("%s: the calling agent is not identified", name)
	}
	if err := toolArgs(req, v); err != nil {
		return "", provider.ErrorResult("%s: invalid arguments: %v", name, err)
	}
	return agent, nil
}

func (sc *scheduler) toolSchedule(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a scheduleArgs
	agent, bad := agentCall("mesh_schedule", req, &a)
	if bad != nil {
		return bad, nil
	}
	if strings.TrimSpace(a.Device) == "" || strings.TrimSpace(a.Tool) == "" {
		return provider.ErrorResult("mesh_schedule: device and tool are required (see mesh_nodes)"), nil
	}
	wake := a.Wake == nil || *a.Wake
	target, tool, warning, err := sc.target(a.Device, a.Tool, wake)
	if err != nil {
		return provider.ErrorResult("mesh_schedule: %v", err), nil
	}
	s, err := sc.store.Add(schedule.Schedule{
		Agent: agent, Label: strings.TrimSpace(a.Label), Device: target.ID, DeviceName: target.Name, Tool: tool,
		Arguments: a.Arguments, Wake: wake,
		Spec: schedule.Spec{At: a.At, In: a.In, Every: a.Every, Daily: a.Daily, Weekdays: a.Weekdays},
	})
	if err != nil {
		return provider.ErrorResult("mesh_schedule: %v", err), nil
	}
	sc.n.log.Info("schedule added", "schedule", s.ID, "agent", agent, "device", target.Name, "tool", tool,
		"when", s.Spec.String(), "next", s.Next)
	return provider.JSONResult(scheduleAdded{ID: s.ID, Next: s.Next, When: s.Spec.String(), Warning: warning})
}

// target resolves the device like the gateway names it (handle, name, ID
// prefix) and checks that it offers tool. An offline device is checked
// against the tool list it last reported, with a warning.
func (sc *scheduler) target(device, tool string, wake bool) (gateway.Node, string, string, error) {
	id, handle, err := sc.n.files.resolveDevice(strings.TrimSpace(device))
	if err != nil {
		return gateway.Node{}, "", "", err
	}
	var node gateway.Node
	for _, d := range sc.n.Nodes() {
		if d.ID == id {
			node = d
		}
	}
	tool = strings.TrimPrefix(strings.TrimSpace(tool), handle+gateway.Separator)
	has := slices.ContainsFunc(node.Tools, func(t *mcp.Tool) bool { return t.Name == tool })
	switch {
	case has && node.Online:
		return node, tool, "", nil
	case has && wake:
		return node, tool, fmt.Sprintf("%s is offline; its last known tool list has %s. It will be woken at run time.", node.Name, tool), nil
	case has:
		return node, tool, fmt.Sprintf("%s is offline; its last known tool list has %s. wake is off, so runs fail unless it is back by then.", node.Name, tool), nil
	case node.Online:
		return gateway.Node{}, "", "", fmt.Errorf("%s has no tool %q (see mesh_nodes for its tools)", node.Name, tool)
	default:
		return gateway.Node{}, "", "", fmt.Errorf("%s is offline and the tool list it last reported has no %q", node.Name, tool)
	}
}

func (sc *scheduler) toolSchedules(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		ID string `json:"id"`
	}
	agent, bad := agentCall("mesh_schedules", req, &a)
	if bad != nil {
		return bad, nil
	}
	handles := sc.handles()
	if strings.TrimSpace(a.ID) != "" {
		s, err := sc.store.Get(agent, a.ID)
		if err != nil {
			return provider.ErrorResult("mesh_schedules: %v", err), nil
		}
		return provider.JSONResult(sc.view(s, handles, true))
	}
	out := struct {
		Schedules []scheduleView `json:"schedules"`
	}{Schedules: []scheduleView{}}
	for _, s := range sc.store.List(agent) {
		out.Schedules = append(out.Schedules, sc.view(s, handles, false))
	}
	return provider.JSONResult(out)
}

func (sc *scheduler) toolUnschedule(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		ID string `json:"id"`
	}
	agent, bad := agentCall("mesh_unschedule", req, &a)
	if bad != nil {
		return bad, nil
	}
	s, err := sc.store.Remove(agent, a.ID)
	if err != nil {
		return provider.ErrorResult("mesh_unschedule: %v", err), nil
	}
	sc.n.log.Info("schedule removed", "schedule", s.ID, "agent", agent)
	return provider.JSONResult(map[string]string{"removed": s.ID})
}

func (sc *scheduler) toolPause(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a struct {
		ID     string `json:"id"`
		Paused *bool  `json:"paused"`
	}
	agent, bad := agentCall("mesh_schedule_pause", req, &a)
	if bad != nil {
		return bad, nil
	}
	if a.Paused == nil {
		return provider.ErrorResult("mesh_schedule_pause: paused (true or false) is required"), nil
	}
	s, err := sc.store.SetPaused(agent, a.ID, *a.Paused)
	if err != nil {
		return provider.ErrorResult("mesh_schedule_pause: %v", err), nil
	}
	sc.n.log.Info("schedule paused", "schedule", s.ID, "agent", agent, "paused", s.Paused)
	return provider.JSONResult(sc.view(s, sc.handles(), false))
}

func (sc *scheduler) toolRunNow(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	if !sc.n.beginWork() {
		return provider.ErrorResult("node is preparing for an update"), nil
	}
	defer sc.n.endWork()
	var a struct {
		ID string `json:"id"`
	}
	agent, bad := agentCall("mesh_schedule_run_now", req, &a)
	if bad != nil {
		return bad, nil
	}
	f, err := sc.store.Claim(agent, a.ID)
	if err != nil {
		return provider.ErrorResult("mesh_schedule_run_now: %v", err), nil
	}
	sc.start(f)
	return provider.JSONResult(map[string]any{
		"id": f.ID, "started": true, "note": "read the result with mesh_schedules {id} once it finishes",
	})
}

// Control API: the owner's CLI sees and manages every agent's schedules.

func (n *Node) registerScheduleAPI(api *http.ServeMux) {
	api.HandleFunc("GET /v1/schedules", n.apiSchedules)
	api.HandleFunc("GET /v1/schedules/{id}", n.apiSchedule)
	api.HandleFunc("DELETE /v1/schedules/{id}", n.apiUnschedule)
	api.HandleFunc("POST /v1/schedules/{id}/pause", n.apiSchedulePause)
	api.HandleFunc("POST /v1/schedules/{id}/run", n.apiScheduleRun)
}

func (n *Node) schedulerOr503(w http.ResponseWriter) *scheduler {
	if n.sched == nil {
		writeError(w, http.StatusServiceUnavailable, "scheduling is off: the schedules file could not be loaded (see the node log)")
	}
	return n.sched
}

func writeScheduleError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, schedule.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, schedule.ErrRunning):
		writeError(w, http.StatusConflict, err.Error())
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

func (n *Node) apiSchedules(w http.ResponseWriter, _ *http.Request) {
	if sc := n.schedulerOr503(w); sc != nil {
		writeJSON(w, http.StatusOK, sc.store.List(""))
	}
}

func (n *Node) apiSchedule(w http.ResponseWriter, r *http.Request) {
	sc := n.schedulerOr503(w)
	if sc == nil {
		return
	}
	s, err := sc.store.Get("", r.PathValue("id"))
	if err != nil {
		writeScheduleError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s)
}

func (n *Node) apiUnschedule(w http.ResponseWriter, r *http.Request) {
	sc := n.schedulerOr503(w)
	if sc == nil {
		return
	}
	s, err := sc.store.Remove("", r.PathValue("id"))
	if err != nil {
		writeScheduleError(w, err)
		return
	}
	n.log.Info("schedule removed", "schedule", s.ID, "agent", s.Agent, "by", cliAgent)
	writeJSON(w, http.StatusOK, s)
}

func (n *Node) apiSchedulePause(w http.ResponseWriter, r *http.Request) {
	sc := n.schedulerOr503(w)
	if sc == nil {
		return
	}
	var body schedule.PauseRequest
	if !readJSON(w, r, &body) {
		return
	}
	s, err := sc.store.SetPaused("", r.PathValue("id"), body.Paused)
	if err != nil {
		writeScheduleError(w, err)
		return
	}
	n.log.Info("schedule paused", "schedule", s.ID, "agent", s.Agent, "paused", s.Paused, "by", cliAgent)
	writeJSON(w, http.StatusOK, s)
}

func (n *Node) apiScheduleRun(w http.ResponseWriter, r *http.Request) {
	if !n.beginWork() {
		writeError(w, http.StatusServiceUnavailable, "node is preparing for an update")
		return
	}
	defer n.endWork()
	sc := n.schedulerOr503(w)
	if sc == nil {
		return
	}
	f, err := sc.store.Claim("", r.PathValue("id"))
	if err != nil {
		writeScheduleError(w, err)
		return
	}
	sc.start(f)
	s, err := sc.store.Get("", f.ID)
	if err != nil {
		writeScheduleError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, s)
}
