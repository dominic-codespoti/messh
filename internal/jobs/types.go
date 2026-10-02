package jobs

import (
	"encoding/json"
	"time"

	"messh/internal/provider"
)

type State string

const (
	StateSubmitted        State = "submitted"
	StateAwaitingApproval State = "awaiting_approval"
	StateQueued           State = "queued"
	StateRunning          State = "running"
	StateSucceeded        State = "succeeded"
	StateFailed           State = "failed"
	StateCancelled        State = "cancelled"
	StateInterrupted      State = "interrupted"
)

func (s State) Terminal() bool {
	switch s {
	case StateSucceeded, StateFailed, StateCancelled, StateInterrupted:
		return true
	}
	return false
}
func (s State) valid() bool {
	switch s {
	case StateSubmitted, StateAwaitingApproval, StateQueued, StateRunning, StateSucceeded, StateFailed, StateCancelled, StateInterrupted:
		return true
	}
	return false
}

type Claims struct {
	GPUs   []int `json:"gpus,omitempty"`
	VRAMMB int64 `json:"vram_mb,omitempty"`
	MemMB  int64 `json:"mem_mb,omitempty"`
	CPUs   int   `json:"cpus,omitempty"`
}

func (c Claims) empty() bool { return len(c.GPUs) == 0 && c.VRAMMB == 0 && c.MemMB == 0 && c.CPUs == 0 }

type Owner struct {
	DeviceID   string `json:"device_id"`
	DeviceName string `json:"device_name,omitempty"`
	Agent      string `json:"agent,omitempty"`
}

func ownerOf(c provider.Caller) Owner {
	return Owner{DeviceID: c.DeviceID, DeviceName: c.DeviceName, Agent: c.Agent}
}
func (o Owner) same(c provider.Caller) bool { return o.DeviceID == c.DeviceID && o.Agent == c.Agent }

type Input struct {
	Ref      string `json:"ref"`
	Rel      string `json:"rel"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
	ModTime  int64  `json:"mod_time"`
	HashedAt int64  `json:"hashed_at,omitempty"`
	InPlace  bool   `json:"in_place"`
}
type Recovery struct {
	Checkpoint string   `json:"checkpoint"`
	Args       []string `json:"args"`
}
type Job struct {
	ID                 string            `json:"id"`
	Label              string            `json:"label,omitempty"`
	Owner              Owner             `json:"owner"`
	State              State             `json:"state"`
	Reason             string            `json:"reason,omitempty"`
	Submitted          time.Time         `json:"submitted"`
	Queued             *time.Time        `json:"queued,omitempty"`
	Started            *time.Time        `json:"started,omitempty"`
	Finished           *time.Time        `json:"finished,omitempty"`
	ExitCode           *int              `json:"exit_code,omitempty"`
	Path               string            `json:"path"`
	Args               []string          `json:"args,omitempty"`
	Shell              bool              `json:"shell,omitempty"`
	Line               string            `json:"line,omitempty"`
	Workspace          string            `json:"workspace"`
	Cwd                string            `json:"cwd,omitempty"`
	Env                map[string]string `json:"env,omitempty"`
	Claims             Claims            `json:"resources"`
	TimeoutSec         int               `json:"timeout_seconds,omitempty"`
	Inputs             []Input           `json:"inputs,omitempty"`
	Exact              string            `json:"exact"`
	RequestID          string            `json:"request_id,omitempty"`
	SubmissionHash     string            `json:"submission_hash,omitempty"`
	Submission         json.RawMessage   `json:"submission,omitempty"`
	Approved           bool              `json:"approved,omitempty"`
	Recovery           *Recovery         `json:"recovery,omitempty"`
	Attempt            int               `json:"attempt,omitempty"`
	CheckpointSHA256   string            `json:"checkpoint_sha256,omitempty"`
	CheckpointSnapshot string            `json:"checkpoint_snapshot,omitempty"`
	PID                int               `json:"pid,omitempty"`
	PIDToken           string            `json:"pid_token,omitempty"`
	Unit               string            `json:"unit,omitempty"`
	Notes              []string          `json:"notes,omitempty"`
}

func (j *Job) Argv() []string { return displayArgv(j.Path, j.Args, j.Shell, j.Line) }
