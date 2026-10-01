package schedule

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // Europe/Berlin without relying on the host's zone database
)

func berlin(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func openStore(t *testing.T, path string, c *clock, loc *time.Location) *Store {
	t.Helper()
	st, err := Open(path, Options{Now: c.now, Location: loc})
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func newStore(t *testing.T, start time.Time) (*Store, *clock) {
	t.Helper()
	c := &clock{t: start}
	return openStore(t, filepath.Join(t.TempDir(), "schedules.json"), c, start.Location()), c
}

func add(t *testing.T, st *Store, agent string, spec Spec) Schedule {
	t.Helper()
	s, err := st.Add(Schedule{Agent: agent, Device: "dev1", DeviceName: "desktop", Tool: "node_info", Spec: spec})
	if err != nil {
		t.Fatalf("add %+v: %v", spec, err)
	}
	return s
}

func wantTime(t *testing.T, what string, got, want time.Time) {
	t.Helper()
	if !got.Equal(want) {
		t.Fatalf("%s = %s, want %s", what, got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestFirstFire(t *testing.T) {
	loc := berlin(t)
	// Thursday 2026-10-01 12:00 CEST.
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, loc)
	cases := []struct {
		spec Spec
		want time.Time
	}{
		{Spec{At: "2026-10-02T02:00:00+02:00"}, time.Date(2026, 10, 2, 2, 0, 0, 0, loc)},
		{Spec{At: "2026-10-02T00:00:00Z"}, time.Date(2026, 10, 2, 2, 0, 0, 0, loc)},
		{Spec{At: "2026-10-02 02:00"}, time.Date(2026, 10, 2, 2, 0, 0, 0, loc)},
		{Spec{In: "90s"}, now.Add(90 * time.Second)},
		{Spec{In: "2h30m"}, now.Add(150 * time.Minute)},
		{Spec{Every: "15m"}, now.Add(15 * time.Minute)},
		{Spec{Daily: "13:30"}, time.Date(2026, 10, 1, 13, 30, 0, 0, loc)},
		{Spec{Daily: "07:00"}, time.Date(2026, 10, 2, 7, 0, 0, 0, loc)},
		// Exactly now does not count: the next fire is strictly in the future.
		{Spec{Daily: "12:00"}, time.Date(2026, 10, 2, 12, 0, 0, 0, loc)},
		{Spec{Daily: "07:00", Weekdays: []string{"mon"}}, time.Date(2026, 10, 5, 7, 0, 0, 0, loc)},
		{Spec{Daily: "13:00", Weekdays: []string{"Thursday", "sat"}}, time.Date(2026, 10, 1, 13, 0, 0, 0, loc)},
		{Spec{Daily: "11:00", Weekdays: []string{"thu"}}, time.Date(2026, 10, 8, 11, 0, 0, 0, loc)},
	}
	for _, c := range cases {
		st, _ := newStore(t, now)
		s := add(t, st, "omp", c.spec)
		wantTime(t, c.spec.String(), s.Next, c.want)
		if !s.Enabled || s.Created.IsZero() || s.ID == "" {
			t.Fatalf("%s: stored as %+v", c.spec, s)
		}
	}
}

func TestWeekdaysNormalized(t *testing.T) {
	st, _ := newStore(t, time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	s := add(t, st, "omp", Spec{Daily: " 07:00 ", Weekdays: []string{"FRI", "monday", "fri", "Wed"}})
	if got := strings.Join(s.Spec.Weekdays, ","); got != "mon,wed,fri" || s.Spec.Daily != "07:00" {
		t.Fatalf("spec = %+v, want daily 07:00 on mon,wed,fri", s.Spec)
	}
}

func TestDailyAcrossDST(t *testing.T) {
	loc := berlin(t)
	daily := Spec{Daily: "07:00"}

	// Spring forward: the night of 2026-03-28/29 is 23 hours long.
	sat := time.Date(2026, 3, 28, 7, 0, 0, 0, loc)
	sun := daily.next(sat, sat, loc)
	if h, m, _ := sun.Clock(); sun.Day() != 29 || h != 7 || m != 0 {
		t.Fatalf("after %s: next = %s, want 2026-03-29 07:00 local", sat, sun)
	}
	if d := sun.Sub(sat); d != 23*time.Hour {
		t.Fatalf("spring-forward day lasted %s, want 23h", d)
	}

	// Fall back: the night of 2026-10-24/25 is 25 hours long.
	sat = time.Date(2026, 10, 24, 7, 0, 0, 0, loc)
	sun = daily.next(sat, sat, loc)
	if h, _, _ := sun.Clock(); sun.Day() != 25 || h != 7 {
		t.Fatalf("after %s: next = %s, want 2026-10-25 07:00 local", sat, sun)
	}
	if d := sun.Sub(sat); d != 25*time.Hour {
		t.Fatalf("fall-back day lasted %s, want 25h", d)
	}

	// 02:30 does not exist on 2026-03-29; it fires once, at 03:30 CEST.
	gap := Spec{Daily: "02:30"}
	midnight := time.Date(2026, 3, 29, 0, 0, 0, 0, loc)
	wantTime(t, "02:30 on the spring-forward day", gap.next(midnight, midnight, loc), time.Date(2026, 3, 29, 1, 30, 0, 0, time.UTC))
	after := gap.next(midnight, time.Date(2026, 3, 29, 3, 30, 0, 0, loc), loc)
	wantTime(t, "the day after", after, time.Date(2026, 3, 30, 2, 30, 0, 0, loc))

	// 02:30 happens twice on 2026-10-25; it fires once that day.
	start := time.Date(2026, 10, 25, 0, 0, 0, 0, loc)
	first := gap.next(start, start, loc)
	if first.Day() != 25 {
		t.Fatalf("first 02:30 on the fall-back day = %s", first)
	}
	if second := gap.next(first, first, loc); second.In(loc).Day() != 26 {
		t.Fatalf("02:30 fired twice on the fall-back day: %s then %s", first, second)
	}

	// Weekday schedules keep their wall-clock time across the change too.
	mon := Spec{Daily: "07:00", Weekdays: []string{"mon"}}
	before := time.Date(2026, 3, 27, 12, 0, 0, 0, loc)
	wantTime(t, "monday after the change", mon.next(before, before, loc), time.Date(2026, 3, 30, 7, 0, 0, 0, loc))
}

func TestEveryKeepsPhase(t *testing.T) {
	every := Spec{Every: "1h"}
	prev := time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)
	wantTime(t, "next after a 2.5h gap", every.next(prev, prev.Add(150*time.Minute), time.UTC), prev.Add(3*time.Hour))
	wantTime(t, "next exactly on a boundary", every.next(prev, prev.Add(time.Hour), time.UTC), prev.Add(2*time.Hour))
	// "every" is elapsed time, not wall-clock: 24h across spring forward lands an hour later locally.
	loc := berlin(t)
	sat := time.Date(2026, 3, 28, 7, 0, 0, 0, loc)
	if got := (Spec{Every: "24h"}).next(sat, sat, loc).In(loc); got.Hour() != 8 {
		t.Fatalf("every 24h across DST = %s, want 08:00 local", got)
	}
}

func TestValidation(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	bad := map[string]Spec{
		"exactly one":       {},
		"exactly one ":      {In: "1m", Every: "1h"},
		"weekdays only":     {Every: "1h", Weekdays: []string{"mon"}},
		"neither RFC 3339":  {At: "tomorrow 2am"},
		"in the past":       {At: "2026-10-01 11:59"},
		"at least 10s":      {In: "5s"},
		"not a Go duration": {In: "1 day"},
		"at least 1m0s":     {Every: "30s"},
		"not HH:MM":         {Daily: "25:00"},
		"not one of":        {Daily: "07:00", Weekdays: []string{"funday"}},
	}
	for want, spec := range bad {
		st, _ := newStore(t, now)
		_, err := st.Add(Schedule{Agent: "omp", Device: "dev1", Tool: "t", Spec: spec})
		if err == nil || !strings.Contains(err.Error(), strings.TrimSpace(want)) {
			t.Errorf("%+v: error %v, want one mentioning %q", spec, err, want)
		}
	}

	st, _ := newStore(t, now)
	calls := map[string]Schedule{
		"no agent":            {Device: "d", Tool: "t", Spec: Spec{In: "1m"}},
		"device and tool":     {Agent: "omp", Device: "d", Spec: Spec{In: "1m"}},
		"must be a JSON obj":  {Agent: "omp", Device: "d", Tool: "t", Arguments: json.RawMessage(`[1,2]`), Spec: Spec{In: "1m"}},
		"must be a JSON obj ": {Agent: "omp", Device: "d", Tool: "t", Arguments: json.RawMessage(`{"a":`), Spec: Spec{In: "1m"}},
		"label":               {Agent: "omp", Device: "d", Tool: "t", Label: strings.Repeat("x", 201), Spec: Spec{In: "1m"}},
	}
	for want, sc := range calls {
		if _, err := st.Add(sc); err == nil || !strings.Contains(err.Error(), strings.TrimSpace(want)) {
			t.Errorf("%q: error %v", want, err)
		}
	}
	if len(st.List("")) != 0 {
		t.Fatal("a rejected schedule was stored")
	}
	s, err := st.Add(Schedule{Agent: "omp", Device: "d", Tool: "t", Arguments: json.RawMessage(" null "), Spec: Spec{In: "1m"}})
	if err != nil || s.Arguments != nil {
		t.Fatalf("null arguments: %v, %q", err, s.Arguments)
	}
}

func TestMissedRunPolicy(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "schedules.json")
	c := &clock{t: t0}
	st := openStore(t, path, c, time.UTC)
	// Due 12:00:10, 12:30 and 12:01; hourly from 13:00; daily at 15:30.
	onTime := add(t, st, "omp", Spec{In: "10s"})
	late := add(t, st, "omp", Spec{In: "30m"})
	tooLate := add(t, st, "omp", Spec{At: "2026-10-01 12:01"})
	hourly := add(t, st, "omp", Spec{Every: "1h"})
	daily := add(t, st, "omp", Spec{Daily: "15:30"})

	// The node is down until 15:31; it starts and reads its file again.
	c.t = time.Date(2026, 10, 1, 15, 31, 0, 0, time.UTC)
	st = openStore(t, path, c, time.UTC)
	fires, err := st.Due(c.t)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Fire{}
	for _, f := range fires {
		got[f.ID] = f
	}
	if len(fires) != 1 || got[daily.ID].ID == "" || got[daily.ID].Missed {
		t.Fatalf("fires at start = %+v, want only the daily one, on time", fires)
	}
	// Everything one-shot that passed more than an hour ago is done without running.
	for _, id := range []string{onTime.ID, late.ID, tooLate.ID} {
		s, _ := st.Get("", id)
		if s.Enabled || len(s.Runs) != 1 || !s.Runs[0].Missed || s.Runs[0].OK || !strings.Contains(s.Runs[0].Error, "not run") {
			t.Fatalf("one-shot %s after 3h down: %+v", s.Spec, s)
		}
	}
	h, _ := st.Get("", hourly.ID)
	if len(h.Runs) != 1 || !h.Runs[0].Missed || h.Runs[0].OK {
		t.Fatalf("hourly after missing three runs: runs %+v, want one missed record", h.Runs)
	}
	wantTime(t, "hourly next", h.Next, time.Date(2026, 10, 1, 16, 0, 0, 0, time.UTC))
	wantTime(t, "missed run's due time", h.Runs[0].Scheduled, time.Date(2026, 10, 1, 13, 0, 0, 0, time.UTC))
	d, _ := st.Get("", daily.ID)
	wantTime(t, "daily next", d.Next, time.Date(2026, 10, 2, 15, 30, 0, 0, time.UTC))
	if !d.Running {
		t.Fatal("a fired schedule is not marked running")
	}

	// A one-shot later than the tolerance but within the grace runs once,
	// marked missed; one within the tolerance is on time.
	st2, _ := newStore(t, t0)
	recent := add(t, st2, "omp", Spec{In: "10m"})
	slight := add(t, st2, "omp", Spec{In: "11m"})
	fires, _ = st2.Due(t0.Add(13 * time.Minute))
	if len(fires) != 2 {
		t.Fatalf("fires = %+v", fires)
	}
	for _, f := range fires {
		if f.ID != recent.ID && f.ID != slight.ID {
			t.Fatalf("unexpected fire %+v", f)
		}
		if want := f.ID == recent.ID; f.Missed != want {
			t.Fatalf("%s at 12:13: Missed=%v, want %v", f.Spec, f.Missed, want)
		}
	}
	fires, _ = st2.Due(t0.Add(14 * time.Minute))
	if len(fires) != 0 {
		t.Fatalf("one-shots fired twice: %+v", fires)
	}

	st3, _ := newStore(t, t0)
	add(t, st3, "omp", Spec{In: "10m"})
	fires, _ = st3.Due(t0.Add(50 * time.Minute))
	if len(fires) != 1 || !fires[0].Missed {
		t.Fatalf("one-shot 40 min late: fires %+v, want one marked missed", fires)
	}
}

func TestRunningSchedulesAreNotRefired(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	st, _ := newStore(t, t0)
	s := add(t, st, "omp", Spec{Every: "1m"})
	fires, _ := st.Due(t0.Add(time.Minute))
	if len(fires) != 1 {
		t.Fatalf("fires = %+v", fires)
	}
	if _, ok := st.NextWake(); ok {
		t.Fatal("a running schedule still sets the wake-up time")
	}
	if fires, _ := st.Due(t0.Add(2 * time.Minute)); len(fires) != 0 {
		t.Fatal("fired again while the previous run was still going")
	}
	if _, err := st.Claim("omp", s.ID); !errors.Is(err, ErrRunning) {
		t.Fatalf("run-now while running: %v", err)
	}
	if err := st.Record(s.ID, Run{Scheduled: fires[0].Scheduled, OK: true}); err != nil {
		t.Fatal(err)
	}
	next, ok := st.NextWake()
	if !ok {
		t.Fatal("no wake-up time after the run finished")
	}
	wantTime(t, "next", next, t0.Add(2*time.Minute))
	f, err := st.Claim("omp", s.ID)
	if err != nil || !f.Manual {
		t.Fatalf("run-now: %+v, %v", f, err)
	}
	if got, _ := st.Get("omp", s.ID); !got.Next.Equal(next) {
		t.Fatal("run-now moved the schedule")
	}
}

func TestPauseAndResume(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	st, c := newStore(t, t0)
	s := add(t, st, "omp", Spec{Every: "1h"})
	if _, err := st.SetPaused("omp", s.ID, true); err != nil {
		t.Fatal(err)
	}
	if fires, _ := st.Due(t0.Add(3 * time.Hour)); len(fires) != 0 {
		t.Fatal("a paused schedule fired")
	}
	c.t = t0.Add(150 * time.Minute)
	r, err := st.SetPaused("omp", s.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	wantTime(t, "next after resume", r.Next, t0.Add(3*time.Hour))
	if len(r.Runs) != 0 {
		t.Fatalf("pausing recorded runs: %+v", r.Runs)
	}
}

func TestPersistenceRoundTrip(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state", "schedules.json")
	c := &clock{t: t0}
	st := openStore(t, path, c, time.UTC)
	args := json.RawMessage(`{"command":"python","args":["render.py","--prompt","a cat"]}`)
	s, err := st.Add(Schedule{Agent: "omp", Label: "nightly render", Device: "dev1", DeviceName: "desktop",
		Tool: "job_submit", Arguments: args, Spec: Spec{Daily: "02:00", Weekdays: []string{"sat", "sun"}}, Wake: true})
	if err != nil {
		t.Fatal(err)
	}
	run := Run{Scheduled: s.Next, Started: s.Next, Finished: s.Next.Add(time.Second), OK: true, Woke: true,
		Result: json.RawMessage(`{"job_id":"j1"}`)}
	if err := st.Record(s.ID, run); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("schedules file mode %v, want 0600", fi.Mode().Perm())
		}
	}

	again := openStore(t, path, c, time.UTC)
	got, err := again.Get("omp", s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Label != s.Label || got.Device != "dev1" || got.DeviceName != "desktop" || got.Tool != "job_submit" ||
		!got.Wake || !got.Enabled || got.Spec.String() != "daily 02:00 (sat,sun)" || string(got.Arguments) != string(args) ||
		!got.Created.Equal(s.Created) || !got.Next.Equal(s.Next) {
		t.Fatalf("reloaded %+v\nwant %+v", got, s)
	}
	if len(got.Runs) != 1 || !got.Runs[0].OK || !got.Runs[0].Woke || string(got.Runs[0].Result) != `{"job_id":"j1"}` ||
		!got.Runs[0].Finished.Equal(run.Finished) {
		t.Fatalf("reloaded runs %+v", got.Runs)
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, Options{}); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt file: %v, want ErrCorrupt", err)
	}
}

func TestAgentIsolation(t *testing.T) {
	st, _ := newStore(t, time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	mine := add(t, st, "omp", Spec{Every: "1h"})
	theirs := add(t, st, "pi", Spec{Every: "1h"})

	if l := st.List("pi"); len(l) != 1 || l[0].ID != theirs.ID {
		t.Fatalf("pi lists %+v", l)
	}
	if l := st.List(""); len(l) != 2 {
		t.Fatalf("the owner lists %d schedules, want 2", len(l))
	}
	for name, op := range map[string]func(id string) error{
		"get":    func(id string) error { _, err := st.Get("pi", id); return err },
		"remove": func(id string) error { _, err := st.Remove("pi", id); return err },
		"pause":  func(id string) error { _, err := st.SetPaused("pi", id, true); return err },
		"run":    func(id string) error { _, err := st.Claim("pi", id); return err },
	} {
		for _, id := range []string{mine.ID, mine.ID[:6]} {
			if err := op(id); !errors.Is(err, ErrNotFound) {
				t.Fatalf("pi %s of omp's schedule %s: %v, want ErrNotFound", name, id, err)
			}
		}
	}
	if s, _ := st.Get("omp", mine.ID); s.Paused {
		t.Fatal("another agent paused the schedule")
	}
	if _, err := st.Get("", mine.ID[:6]); err != nil {
		t.Fatalf("owner lookup by prefix: %v", err)
	}

	for range MaxPerAgent - 1 {
		add(t, st, "omp", Spec{In: "1h"})
	}
	if _, err := st.Add(Schedule{Agent: "omp", Device: "d", Tool: "t", Spec: Spec{In: "1h"}}); err == nil {
		t.Fatalf("schedule %d for one agent was accepted", MaxPerAgent+1)
	}
	add(t, st, "pi", Spec{In: "1h"}) // the limit is per agent
}

func TestRunHistoryCapAndTruncation(t *testing.T) {
	st, _ := newStore(t, time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	s := add(t, st, "omp", Spec{Every: "1h"})
	for i := range MaxRuns + 5 {
		res, _ := json.Marshal(map[string]int{"n": i})
		if err := st.Record(s.ID, Run{OK: true, Result: res}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := st.Get("omp", s.ID)
	if len(got.Runs) != MaxRuns || string(got.Runs[0].Result) != `{"n":5}` || string(got.Runs[MaxRuns-1].Result) != `{"n":24}` {
		t.Fatalf("history: %d runs, first %s, last %s", len(got.Runs), got.Runs[0].Result, got.Runs[len(got.Runs)-1].Result)
	}

	big := json.RawMessage(`{"text":"` + strings.Repeat(`é<\"x`, 40<<10) + `"}`)
	if err := st.Record(s.ID, Run{OK: true, Result: big}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.Get("omp", s.ID)
	res := got.Runs[len(got.Runs)-1].Result
	var text string
	if len(res) > MaxResult || json.Unmarshal(res, &text) != nil {
		t.Fatalf("stored result is %d bytes (max %d) or not a JSON string", len(res), MaxResult)
	}
	marker := fmt.Sprintf("truncated by messh: the result was %d bytes", len(big))
	if !strings.HasPrefix(text, `{"text":"é<\"x`) || !strings.Contains(text, marker) || len(res) < MaxResult-16 {
		t.Fatalf("truncated result (%d bytes) lacks its start or the marker %q: %q", len(res), marker, text[len(text)-80:])
	}
	if strings.ContainsRune(text, '\uFFFD') {
		t.Fatal("truncation split a UTF-8 sequence")
	}

	// Results that are not JSON are kept as a string, never break the file.
	if err := st.Record(s.ID, Run{Error: "x", Result: json.RawMessage("not json")}); err != nil {
		t.Fatal(err)
	}
	got, _ = st.Get("omp", s.ID)
	if r := got.Runs[len(got.Runs)-1].Result; string(r) != `"not json"` {
		t.Fatalf("non-JSON result stored as %s", r)
	}
}
