package schedule

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"messh/internal/state"
)

const (
	MaxPerAgent = 200       // schedules one agent may keep, finished one-shots included
	MaxRuns     = 20        // run records kept per schedule, newest last
	MaxResult   = 64 << 10  // stored bytes of one run's result
	maxLabel    = 200       // characters of a label
	maxArgs     = 256 << 10 // bytes of a call's arguments
)

// Missed-run rules; see Due.
const (
	LateTolerance = 2 * time.Minute // a fire this late still counts as on time (timer slack, a short stall)
	OneShotGrace  = time.Hour       // a later one-shot still runs, marked missed; older ones do not run
)

var (
	ErrNotFound = errors.New("no such schedule")
	ErrRunning  = errors.New("a run of this schedule is already in progress")
	ErrCorrupt  = errors.New("unreadable schedules file") // wraps a schedules file that exists but does not parse
)

// Schedule is one stored call.
type Schedule struct {
	ID         string          `json:"id"`
	Agent      string          `json:"agent"`
	Label      string          `json:"label,omitempty"`
	Device     string          `json:"device"`      // target device ID
	DeviceName string          `json:"device_name"` // its name when the schedule was made
	Tool       string          `json:"tool"`
	Arguments  json.RawMessage `json:"arguments,omitempty"` // passed to the tool unchanged
	Spec       Spec            `json:"spec"`
	Wake       bool            `json:"wake"`
	Created    time.Time       `json:"created"`
	Next       time.Time       `json:"next,omitzero"` // zero once a one-shot has fired
	Paused     bool            `json:"paused,omitempty"`
	Enabled    bool            `json:"enabled"` // false once a one-shot has fired or been missed
	Runs       []Run           `json:"runs,omitempty"`
	Running    bool            `json:"running,omitempty"` // a run is in progress; set on copies only, never persisted
}

// Run records one execution, or one occurrence that did not execute.
type Run struct {
	Scheduled time.Time       `json:"scheduled,omitzero"` // the fire time it belongs to; zero for run-now
	Started   time.Time       `json:"started"`
	Finished  time.Time       `json:"finished"`
	OK        bool            `json:"ok"`
	Error     string          `json:"error,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Woke      bool            `json:"woke,omitempty"`   // the target was offline and Wake-on-LAN brought it up
	Missed    bool            `json:"missed,omitempty"` // later than LateTolerance; ran only if a one-shot within OneShotGrace
	Manual    bool            `json:"manual,omitempty"` // started by run-now
}

// Fire is a call the runner should perform now.
type Fire struct {
	Schedule
	Scheduled time.Time // the fire time; zero for run-now
	Missed    bool
	Manual    bool
}

// PauseRequest is the body of the control API's pause endpoint.
type PauseRequest struct {
	Paused bool `json:"paused"`
}

// Options tunes a Store; zero values select the defaults.
type Options struct {
	Now      func() time.Time // default time.Now
	Location *time.Location   // zone for local times and days; default time.Local
	MinIn    time.Duration    // shortest "in"; default MinIn
}

// Store holds every agent's schedules and persists them atomically.
// Methods taking an agent only see that agent's schedules; the empty agent
// means all of them (the owner's CLI).
type Store struct {
	path  string
	now   func() time.Time
	loc   *time.Location
	minIn time.Duration

	mu       sync.Mutex
	list     []*Schedule // in creation order
	inflight map[string]bool
	changed  chan struct{}
}

type fileFormat struct {
	Version   int         `json:"version"`
	Schedules []*Schedule `json:"schedules"`
}

// Open loads the schedules at path; a missing file is an empty store.
func Open(path string, o Options) (*Store, error) {
	st := &Store{
		path: path, now: o.Now, loc: o.Location, minIn: o.MinIn,
		inflight: map[string]bool{}, changed: make(chan struct{}, 1),
	}
	if st.now == nil {
		st.now = time.Now
	}
	if st.loc == nil {
		st.loc = time.Local
	}
	if st.minIn <= 0 {
		st.minIn = MinIn
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	var f fileFormat
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("%w %s: %v", ErrCorrupt, path, err)
	}
	for _, s := range f.Schedules {
		if s == nil || s.ID == "" {
			continue
		}
		// The file is indented for its owner; keep the call's bytes as they were stored.
		s.Running, s.Arguments = false, compactJSON(s.Arguments)
		for i := range s.Runs {
			s.Runs[i].Result = compactJSON(s.Runs[i].Result)
		}
		st.list = append(st.list, s)
	}
	return st, nil
}

// Changed is signalled after any change that may move the earliest fire time.
func (st *Store) Changed() <-chan struct{} { return st.changed }

// Add validates sc's call and spec, then stores it with a fresh ID, its
// creation time and first fire time. Agent, Device, Tool and Spec are
// required; Arguments, when given, must be a JSON object.
func (st *Store) Add(sc Schedule) (Schedule, error) {
	if sc.Agent == "" {
		return Schedule{}, errors.New("schedule has no agent")
	}
	if sc.Device == "" || sc.Tool == "" {
		return Schedule{}, errors.New("device and tool are required")
	}
	if utf8.RuneCountInString(sc.Label) > maxLabel {
		return Schedule{}, fmt.Errorf("label: at most %d characters", maxLabel)
	}
	switch t := bytes.TrimSpace(sc.Arguments); {
	case len(t) == 0 || string(t) == "null":
		sc.Arguments = nil
	case len(t) > maxArgs:
		return Schedule{}, fmt.Errorf("arguments: at most %d KiB", maxArgs>>10)
	case t[0] != '{' || !json.Valid(t):
		return Schedule{}, errors.New("arguments must be a JSON object")
	default:
		sc.Arguments = compactJSON(t)
	}
	spec, err := sc.Spec.normalize(st.minIn)
	if err != nil {
		return Schedule{}, err
	}
	now := st.now().Round(0)
	next, err := spec.first(now, st.loc)
	if err != nil {
		return Schedule{}, err
	}

	st.mu.Lock()
	defer st.mu.Unlock()
	count := 0
	for _, s := range st.list {
		if s.Agent == sc.Agent {
			count++
		}
	}
	if count >= MaxPerAgent {
		return Schedule{}, fmt.Errorf("agent %s already has %d schedules, the limit; remove finished or unneeded ones first", sc.Agent, count)
	}
	sc.ID = st.newID()
	sc.Spec, sc.Created, sc.Next = spec, now, next.Round(0)
	sc.Enabled, sc.Paused, sc.Running, sc.Runs = true, false, false, nil
	s := &sc
	st.list = append(st.list, s)
	if err := st.save(); err != nil {
		st.list = st.list[:len(st.list)-1]
		return Schedule{}, err
	}
	st.signal()
	return st.view(s), nil
}

// List returns agent's schedules (all for ""), oldest first.
func (st *Store) List(agent string) []Schedule {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := []Schedule{}
	for _, s := range st.list {
		if agent == "" || s.Agent == agent {
			out = append(out, st.view(s))
		}
	}
	return out
}

// Get finds one of agent's schedules by ID or unique ID prefix (4+ chars).
func (st *Store) Get(agent, id string) (Schedule, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s, err := st.find(agent, id)
	if err != nil {
		return Schedule{}, err
	}
	return st.view(s), nil
}

// Remove deletes one of agent's schedules. A run in progress finishes but is
// not recorded.
func (st *Store) Remove(agent, id string) (Schedule, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s, err := st.find(agent, id)
	if err != nil {
		return Schedule{}, err
	}
	i := slices.Index(st.list, s)
	st.list = slices.Delete(st.list, i, i+1)
	if err := st.save(); err != nil {
		st.list = slices.Insert(st.list, i, s)
		return Schedule{}, err
	}
	delete(st.inflight, s.ID)
	st.signal()
	return st.view(s), nil
}

// SetPaused pauses or resumes one of agent's schedules. A resumed recurring
// schedule continues at its next future occurrence; the ones that passed while
// paused are not recorded. A resumed one-shot whose time passed is handled by
// the missed-run rules at the next Due.
func (st *Store) SetPaused(agent, id string, paused bool) (Schedule, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s, err := st.find(agent, id)
	if err != nil {
		return Schedule{}, err
	}
	prevPaused, prevNext := s.Paused, s.Next
	s.Paused = paused
	if now := st.now().Round(0); !paused && s.Enabled && s.Spec.Recurring() && !s.Next.After(now) {
		s.Next = s.Spec.next(s.Next, now, st.loc).Round(0)
	}
	if err := st.save(); err != nil {
		s.Paused, s.Next = prevPaused, prevNext
		return Schedule{}, err
	}
	st.signal()
	return st.view(s), nil
}

// Claim starts an extra run of one of agent's schedules now (run-now). It
// works on paused and finished schedules too and leaves their timing alone.
func (st *Store) Claim(agent, id string) (Fire, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	s, err := st.find(agent, id)
	if err != nil {
		return Fire{}, err
	}
	if st.inflight[s.ID] {
		return Fire{}, ErrRunning
	}
	st.inflight[s.ID] = true
	return Fire{Schedule: st.fireCopy(s), Manual: true}, nil
}

// Due applies the timing rules at now and returns the calls to perform. Each
// returned schedule counts as running until Record is called for it, and is
// not considered again before that. For each schedule whose time has come:
//   - on time (at most LateTolerance late): fire; recurring ones move to their
//     next occurrence, one-shots are disabled;
//   - a late one-shot fires marked Missed when at most OneShotGrace late, and
//     is otherwise recorded as missed without running; either way it is done;
//   - a late recurring schedule records one missed run and moves to its next
//     future occurrence without running.
//
// The error reports a failure to persist; the returned fires are still valid.
func (st *Store) Due(now time.Time) ([]Fire, error) {
	now = now.Round(0)
	st.mu.Lock()
	defer st.mu.Unlock()
	var fires []Fire
	dirty := false
	for _, s := range st.list {
		if !s.Enabled || s.Paused || st.inflight[s.ID] || s.Next.IsZero() || s.Next.After(now) {
			continue
		}
		dirty = true
		due := s.Next
		late := now.Sub(due)
		missedRun := Run{Scheduled: due, Started: now, Finished: now, Missed: true}
		if s.Spec.Recurring() {
			s.Next = s.Spec.next(due, now, st.loc).Round(0)
			if late > LateTolerance {
				missedRun.Error = fmt.Sprintf("missed the run due at %s: this node was not running, asleep, or busy with the previous run; next run at %s",
					st.local(due), st.local(s.Next))
				s.addRun(missedRun)
				continue
			}
		} else {
			s.Enabled, s.Next = false, time.Time{}
			if late > OneShotGrace {
				missedRun.Error = fmt.Sprintf("missed: due at %s, more than an hour before this node could run it; not run", st.local(due))
				s.addRun(missedRun)
				continue
			}
		}
		st.inflight[s.ID] = true
		fires = append(fires, Fire{Schedule: st.fireCopy(s), Scheduled: due, Missed: late > LateTolerance})
	}
	if !dirty {
		return nil, nil
	}
	slices.SortStableFunc(fires, func(a, b Fire) int { return a.Scheduled.Compare(b.Scheduled) })
	return fires, st.save()
}

// Record stores the outcome of a fire returned by Due or Claim and ends its
// running state. The result is truncated to MaxResult; the history keeps the
// newest MaxRuns runs. Runs of removed schedules are dropped.
func (st *Store) Record(id string, r Run) error {
	if len(r.Result) > 0 && !json.Valid(r.Result) {
		r.Result = marshalString(string(r.Result)) // one bad result must not make the file unsavable
	}
	r.Result = TruncateResult(compactJSON(r.Result), MaxResult)
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.inflight, id)
	st.signal()
	i := slices.IndexFunc(st.list, func(s *Schedule) bool { return s.ID == id })
	if i < 0 {
		return nil
	}
	st.list[i].addRun(r)
	return st.save()
}

// NextWake returns the earliest fire time among schedules that can fire.
func (st *Store) NextWake() (time.Time, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	var next time.Time
	for _, s := range st.list {
		if !s.Enabled || s.Paused || st.inflight[s.ID] || s.Next.IsZero() {
			continue
		}
		if next.IsZero() || s.Next.Before(next) {
			next = s.Next
		}
	}
	return next, !next.IsZero()
}

// TruncateResult caps raw at limit bytes. A longer result becomes a JSON
// string holding its first bytes and a marker with the original size, so the
// stored value stays valid JSON.
func TruncateResult(raw json.RawMessage, limit int) json.RawMessage {
	if len(raw) <= limit {
		return raw
	}
	marker := fmt.Sprintf(" …[truncated by messh: the result was %d bytes]", len(raw))
	budget := limit - len(marshalString(marker))
	n, size := 0, 0
	for n < len(raw) {
		r, w := utf8.DecodeRune(raw[n:])
		if size += escapedLen(r, w); size > budget {
			break
		}
		n += w
	}
	if budget <= 0 {
		return marshalString(strings.TrimSpace(marker))
	}
	return marshalString(string(raw[:n]) + marker)
}

// escapedLen is the size of r inside a JSON string as marshalString writes
// it, or more (control characters are counted as \u00XX).
func escapedLen(r rune, width int) int {
	switch {
	case r == '"' || r == '\\' || r == '\n' || r == '\r' || r == '\t':
		return 2
	case r < 0x20 || r == '\u2028' || r == '\u2029' || r == utf8.RuneError && width == 1:
		return 6
	}
	return width
}

func marshalString(s string) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(s) // a string always encodes
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}

func compactJSON(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var b bytes.Buffer
	if json.Compact(&b, raw) != nil {
		return raw
	}
	return b.Bytes()
}

func (s *Schedule) addRun(r Run) {
	s.Runs = append(s.Runs, r)
	if extra := len(s.Runs) - MaxRuns; extra > 0 {
		s.Runs = slices.Delete(s.Runs, 0, extra)
	}
}

func (st *Store) find(agent, id string) (*Schedule, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return nil, fmt.Errorf("%w: give a schedule id", ErrNotFound)
	}
	var prefixed []*Schedule
	for _, s := range st.list {
		if agent != "" && s.Agent != agent {
			continue
		}
		if s.ID == id {
			return s, nil
		}
		if len(id) >= 4 && strings.HasPrefix(s.ID, id) {
			prefixed = append(prefixed, s)
		}
	}
	switch len(prefixed) {
	case 1:
		return prefixed[0], nil
	case 0:
		return nil, fmt.Errorf("%w %q", ErrNotFound, id)
	default:
		return nil, fmt.Errorf("%w: %q matches %d schedules; give more of the id", ErrNotFound, id, len(prefixed))
	}
}

func (st *Store) view(s *Schedule) Schedule {
	c := *s
	c.Runs = slices.Clone(s.Runs)
	c.Running = st.inflight[s.ID]
	return c
}

func (st *Store) fireCopy(s *Schedule) Schedule {
	c := *s
	c.Runs = nil
	return c
}

func (st *Store) local(t time.Time) string { return t.In(st.loc).Format("2006-01-02 15:04") }

func (st *Store) newID() string {
	for {
		var b [6]byte
		rand.Read(b[:])
		id := hex.EncodeToString(b[:])
		if !slices.ContainsFunc(st.list, func(s *Schedule) bool { return s.ID == id }) {
			return id
		}
	}
}

func (st *Store) signal() {
	select {
	case st.changed <- struct{}{}:
	default:
	}
}

// save writes the store; callers hold st.mu. The file holds call arguments
// and results, so it is owner-only.
func (st *Store) save() error {
	data, err := json.MarshalIndent(fileFormat{Version: 1, Schedules: st.list}, "", "  ")
	if err != nil {
		return err
	}
	if err := state.WriteFileAtomic(st.path, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("save schedules: %w", err)
	}
	return nil
}
