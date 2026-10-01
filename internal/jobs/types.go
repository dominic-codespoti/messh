package jobs

import (
	"time"

	"messh/internal/provider"
)

// State is a job's lifecycle position.
type State string

const (
	StateSubmitted        State = "submitted"         // accepted; inputs are being snapshotted
	StateAwaitingApproval State = "awaiting_approval" // the device owner has not decided yet
	StateQueued           State = "queued"            // approved; waiting for resources
	StateRunning          State = "running"
	StateSucceeded        State = "succeeded"
	StateFailed           State = "failed"
	StateCancelled        State = "cancelled"
	StateInterrupted      State = "interrupted" // the node stopped or restarted underneath it
)

// Terminal reports whether the job has finished for good.
func (s State) Terminal() bool {
	switch s {
	case StateSucceeded, StateFailed, StateCancelled, StateInterrupted:
		return true
	}
	return false
}

func (s State) valid() bool {
	switch s {
	case StateSubmitted, StateAwaitingApproval, StateQueued, StateRunning,
		StateSucceeded, StateFailed, StateCancelled, StateInterrupted:
		return true
	}
	return false
}

// Claims are the resources a job asks for. GPUs are exclusive per index; the
// rest are budgets checked against the device when the job starts.
type Claims struct {
	GPUs   []int `json:"gpus,omitempty"`
	VRAMMB int64 `json:"vram_mb,omitempty"`
	MemMB  int64 `json:"mem_mb,omitempty"`
	CPUs   int   `json:"cpus,omitempty"`
}

func (c Claims) empty() bool {
	return len(c.GPUs) == 0 && c.VRAMMB == 0 && c.MemMB == 0 && c.CPUs == 0
}

// Owner is the (device, agent) that submitted a job; only it may see or
// change the job.
type Owner struct {
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name,omitempty"`
	Agent      string `json:"agent,omitempty"`
}

func ownerOf(c provider.Caller) Owner {
	return Owner{DeviceID: c.DeviceID, DeviceName: c.DeviceName, Agent: c.Agent}
}

func (o Owner) same(c provider.Caller) bool {
	return o.DeviceID == c.DeviceID && o.Agent == c.Agent
}

// Input is a file snapshotted into the job's workspace.
type Input struct {
	Ref      string `json:"ref"`                 // the ref the agent named, e.g. ws/prep/data.csv
	Rel      string `json:"rel"`                 // where it sits inside the job workspace
	SHA256   string `json:"sha256"`              // content hash that was approved
	Size     int64  `json:"size"`                // bytes
	ModTime  int64  `json:"mod_time"`            // UnixNano of the snapshot file when taken
	HashedAt int64  `json:"hashed_at,omitempty"` // UnixNano when SHA256 was read; 0 (older job.json) = unknown, always re-hash
	InPlace  bool   `json:"in_place"`            // already inside the job workspace; not copied
}

// Job is the persisted record (job.json).
type Job struct {
	ID    string `json:"id"`
	Label string `json:"label,omitempty"`
	Owner Owner  `json:"owner"`

	State  State  `json:"state"`
	Reason string `json:"reason,omitempty"`

	Submitted time.Time  `json:"submitted"`
	Queued    *time.Time `json:"queued,omitempty"`
	Started   *time.Time `json:"started,omitempty"`
	Finished  *time.Time `json:"finished,omitempty"`
	ExitCode  *int       `json:"exit_code,omitempty"`

	Path       string            `json:"path"`            // resolved executable
	Args       []string          `json:"args,omitempty"`  // argv[1:] (not used for shell jobs)
	Shell      bool              `json:"shell,omitempty"` // Path is the shell and Line the command line
	Line       string            `json:"line,omitempty"`  // shell command line
	Workspace  string            `json:"workspace"`       // directory name under ws/
	Cwd        string            `json:"cwd,omitempty"`   // slash path inside the workspace
	Env        map[string]string `json:"env,omitempty"`   // overrides only
	Claims     Claims            `json:"resources"`       //
	TimeoutSec int               `json:"timeout_seconds,omitempty"`
	Inputs     []Input           `json:"inputs,omitempty"`
	Exact      string            `json:"exact"` // approved request hash

	PID      int      `json:"pid,omitempty"`
	PIDToken string   `json:"pid_token,omitempty"` // process start time, guards against PID reuse
	Unit     string   `json:"unit,omitempty"`      // systemd scope holding the tree (Linux)
	Notes    []string `json:"notes,omitempty"`     // limits that could not be enforced
}

// Argv returns the command line as it will be executed, for display.
func (j *Job) Argv() []string { return displayArgv(j.Path, j.Args, j.Shell, j.Line) }
