// Package schedule stores deferred and recurring tool calls and decides when
// each is due. The node runs the calls; this package owns the timing rules
// (local wall-clock days, DST, missed runs) and the persisted state.
package schedule

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Spec says when a schedule fires. Exactly one of At, In, Every and Daily is
// set; Weekdays only goes with Daily.
type Spec struct {
	At       string   `json:"at,omitempty"`       // one-shot: RFC 3339, or "YYYY-MM-DD HH:MM" local time
	In       string   `json:"in,omitempty"`       // one-shot: Go duration from creation, at least MinIn
	Every    string   `json:"every,omitempty"`    // recurring: Go duration, at least MinEvery
	Daily    string   `json:"daily,omitempty"`    // recurring: "HH:MM" local wall-clock time
	Weekdays []string `json:"weekdays,omitempty"` // with Daily: restrict to these days ("mon".."sun")
}

// Shortest accepted "in" and "every" durations.
const (
	MinIn    = 10 * time.Second
	MinEvery = time.Minute
)

var weekdayNames = []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}

var weekdayOf = map[string]time.Weekday{
	"mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday, "thu": time.Thursday,
	"fri": time.Friday, "sat": time.Saturday, "sun": time.Sunday,
}

var weekdayFull = map[string]string{
	"monday": "mon", "tuesday": "tue", "wednesday": "wed", "thursday": "thu",
	"friday": "fri", "saturday": "sat", "sunday": "sun",
}

// Recurring reports whether the spec fires more than once.
func (s Spec) Recurring() bool { return s.Every != "" || s.Daily != "" }

// String renders the spec for listings, e.g. "daily 07:00 (mon,tue)".
func (s Spec) String() string {
	switch {
	case s.At != "":
		return "at " + s.At
	case s.In != "":
		return "in " + s.In
	case s.Every != "":
		return "every " + s.Every
	case s.Daily != "" && len(s.Weekdays) > 0:
		return "daily " + s.Daily + " (" + strings.Join(s.Weekdays, ",") + ")"
	case s.Daily != "":
		return "daily " + s.Daily
	}
	return "never"
}

// normalize checks the syntax of s and returns it trimmed, with weekdays as
// three-letter names in week order. minIn is the shortest accepted "in".
func (s Spec) normalize(minIn time.Duration) (Spec, error) {
	s.At, s.In = strings.TrimSpace(s.At), strings.TrimSpace(s.In)
	s.Every, s.Daily = strings.TrimSpace(s.Every), strings.TrimSpace(s.Daily)
	set := 0
	for _, v := range []string{s.At, s.In, s.Every, s.Daily} {
		if v != "" {
			set++
		}
	}
	if set != 1 {
		return Spec{}, errors.New("give exactly one of at, in, every, daily")
	}
	if len(s.Weekdays) > 0 && s.Daily == "" {
		return Spec{}, errors.New("weekdays only applies to daily")
	}
	switch {
	case s.At != "":
		if _, err := parseAt(s.At, time.UTC); err != nil {
			return Spec{}, err
		}
	case s.In != "":
		d, err := time.ParseDuration(s.In)
		if err != nil {
			return Spec{}, fmt.Errorf("in: %q is not a Go duration (e.g. 90s, 45m, 2h30m)", s.In)
		}
		if d < minIn {
			return Spec{}, fmt.Errorf("in: must be at least %s", minIn)
		}
	case s.Every != "":
		d, err := time.ParseDuration(s.Every)
		if err != nil {
			return Spec{}, fmt.Errorf("every: %q is not a Go duration (e.g. 15m, 6h)", s.Every)
		}
		if d < MinEvery {
			return Spec{}, fmt.Errorf("every: must be at least %s", MinEvery)
		}
	case s.Daily != "":
		if _, _, err := parseClock(s.Daily); err != nil {
			return Spec{}, err
		}
		days, err := parseWeekdays(s.Weekdays)
		if err != nil {
			return Spec{}, err
		}
		s.Weekdays = days
	}
	return s, nil
}

// first returns the initial fire time of a normalized spec created at now.
func (s Spec) first(now time.Time, loc *time.Location) (time.Time, error) {
	switch {
	case s.At != "":
		t, err := parseAt(s.At, loc)
		if err != nil {
			return time.Time{}, err
		}
		if !t.After(now) {
			return time.Time{}, fmt.Errorf("at: %s is in the past (now %s)", t.In(loc).Format(time.DateTime), now.In(loc).Format(time.DateTime))
		}
		return t, nil
	case s.In != "":
		d, _ := time.ParseDuration(s.In)
		return now.Add(d), nil
	case s.Every != "":
		d, _ := time.ParseDuration(s.Every)
		return now.Add(d), nil
	case s.Daily != "":
		return s.nextDaily(now, loc), nil
	}
	return time.Time{}, errors.New("empty schedule spec")
}

// next returns the first occurrence of a recurring spec strictly after now.
// Intervals keep their phase relative to prev, so "every 1h" does not drift
// by the time each run takes. One-shot specs have no next occurrence.
func (s Spec) next(prev, now time.Time, loc *time.Location) time.Time {
	switch {
	case s.Every != "":
		d, err := time.ParseDuration(s.Every)
		if err != nil || d <= 0 {
			return time.Time{}
		}
		if prev.IsZero() || prev.After(now) {
			return now.Add(d)
		}
		t := prev.Add(now.Sub(prev) / d * d)
		for !t.After(now) {
			t = t.Add(d)
		}
		return t
	case s.Daily != "":
		return s.nextDaily(now, loc)
	}
	return time.Time{}
}

// nextDaily finds the first allowed day whose HH:MM local wall-clock time is
// after now. time.Date resolves DST: a time inside a spring-forward gap moves
// forward by the gap (02:30 becomes 03:30), an ambiguous fall-back time is
// used once.
func (s Spec) nextDaily(now time.Time, loc *time.Location) time.Time {
	hh, mm, err := parseClock(s.Daily)
	if err != nil {
		return time.Time{}
	}
	allowed := map[time.Weekday]bool{}
	for _, d := range s.Weekdays {
		allowed[weekdayOf[d]] = true
	}
	base := now.In(loc)
	for i := 0; i <= 8; i++ {
		t := time.Date(base.Year(), base.Month(), base.Day()+i, hh, mm, 0, 0, loc)
		if t.After(now) && (len(allowed) == 0 || allowed[t.Weekday()]) {
			return t
		}
	}
	return time.Time{}
}

// localLayouts are the accepted forms of an `at` time without a zone; they
// are read in the scheduler's local time zone.
var localLayouts = []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02T15:04:05"}

func parseAt(v string, loc *time.Location) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	for _, layout := range localLayouts {
		if t, err := time.ParseInLocation(layout, v, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf(`at: %q is neither RFC 3339 (2026-10-02T02:00:00+02:00) nor "YYYY-MM-DD HH:MM" local time`, v)
}

func parseClock(v string) (hh, mm int, err error) {
	t, perr := time.Parse("15:04", v)
	if perr != nil {
		return 0, 0, fmt.Errorf("daily: %q is not HH:MM (24-hour local time, e.g. 07:00)", v)
	}
	return t.Hour(), t.Minute(), nil
}

func parseWeekdays(in []string) ([]string, error) {
	seen := map[string]bool{}
	for _, d := range in {
		key := strings.ToLower(strings.TrimSpace(d))
		if short, ok := weekdayFull[key]; ok {
			key = short
		}
		if _, ok := weekdayOf[key]; !ok {
			return nil, fmt.Errorf("weekdays: %q is not one of mon, tue, wed, thu, fri, sat, sun", d)
		}
		seen[key] = true
	}
	var out []string
	for _, d := range weekdayNames {
		if seen[d] {
			out = append(out, d)
		}
	}
	return out, nil
}
