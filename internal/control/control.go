// Package control defines the loopback API between the messh CLI and a
// running node, plus the client the CLI uses to call it.
package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"messh/internal/approval"
	"messh/internal/buildinfo"
	"messh/internal/state"
)

// Status describes the running node.
type Status struct {
	ID      string         `json:"id"`
	Name    string         `json:"name"`
	Version string         `json:"version"`
	Build   buildinfo.Info `json:"build"`
	Mesh    string         `json:"mesh"`
	Local   string         `json:"local"`
	Started time.Time      `json:"started"`
}

// PeerStatus is a paired device as seen by this node.
type PeerStatus struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Online   bool      `json:"online"`
	LastSeen time.Time `json:"last_seen,omitzero"`
	Addrs    []string  `json:"addrs"`
	PairedAt time.Time `json:"paired_at"`
	Tools    []string  `json:"tools"`
	Asleep   bool      `json:"asleep,omitempty"`    // it said it is going to sleep; probes paused until it is heard again
	AutoWake bool      `json:"auto_wake,omitempty"` // tool calls wake it first (messh wake DEVICE --auto on)
}

// Sighting is a device heard via discovery, paired or not.
type Sighting struct {
	ID     string    `json:"id"`
	Name   string    `json:"name"`
	Addr   string    `json:"addr"`
	Seen   time.Time `json:"seen"`
	Paired bool      `json:"paired"`
}

// PairAcceptRequest opens the pairing window.
type PairAcceptRequest struct {
	TimeoutSeconds int `json:"timeout_seconds"`
}

// PairWindow reports when the pairing window closes.
type PairWindow struct {
	Until time.Time `json:"until"`
}

// Incoming is a pairing request waiting for this device's user.
type Incoming struct {
	ID       string    `json:"id"`
	PeerID   string    `json:"peer_id"`
	PeerName string    `json:"peer_name"`
	Addr     string    `json:"addr"`
	Code     string    `json:"code"`
	Created  time.Time `json:"created"`
}

// Decision answers an incoming request or confirms an outgoing one.
type Decision struct {
	Accept bool `json:"accept"`
}

// PairRequest starts pairing with target: a discovered name, an ID prefix, host, or host:port.
type PairRequest struct {
	Target string `json:"target"`
}

// Outgoing pairing states.
const (
	OutAwaitingConfirmation = "awaiting_confirmation" // local user has not compared codes yet
	OutAwaitingRemote       = "awaiting_remote"       // local user confirmed; remote user has not
	OutPaired               = "paired"
	OutRejected             = "rejected"
	OutCancelled            = "cancelled"
	OutFailed               = "failed"
)

// Outgoing is a pairing this device initiated.
type Outgoing struct {
	ID       string `json:"id"`
	Addr     string `json:"addr"`
	PeerID   string `json:"peer_id"`
	PeerName string `json:"peer_name"`
	Code     string `json:"code"`
	State    string `json:"state"`
	Error    string `json:"error,omitempty"`
}

// Hello is the body a pairing initiator sends to the acceptor's mesh port.
type Hello struct {
	Name string `json:"name"`
	Port int    `json:"port"` // initiator's mesh port, so the acceptor can reach it later
}

// HelloReply is the acceptor's answer.
type HelloReply struct {
	Accepted bool   `json:"accepted"`
	ID       string `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
}

// Error is the JSON body of non-2xx control responses.
type Error struct {
	Error string `json:"error"`
}

// Approval API: requests waiting for this device's person, saved rules, and
// the audit log. The shapes are the approval package's own.
type (
	PendingApproval = approval.Pending
	Rule            = approval.Rule
	AuditRecord     = approval.AuditRecord
)

// Approval decisions for ApprovalAnswer.Decision.
const (
	DecisionAllow  = "allow"  // this request only
	DecisionAlways = "always" // and save a rule for the chosen scope
	DecisionDeny   = "deny"
)

// Audit record kinds.
const (
	AuditDecision   = approval.KindDecision
	AuditCompletion = approval.KindCompletion
)

// ApprovalAnswer answers a pending approval (POST /v1/approvals/{id}).
type ApprovalAnswer struct {
	Decision string `json:"decision"`
	Scope    int    `json:"scope,omitempty"` // for "always": index into the request's scopes; 0 is the exact request
}

// ApprovalTestResult is the answer to a synthetic request shown by
// POST /v1/approvals/test: what the person chose. Nothing ran and no rule was saved.
type ApprovalTestResult struct {
	ID         string `json:"id"`
	Outcome    string `json:"outcome"` // allow-once, allow-always, deny, expired or cancelled
	Allowed    bool   `json:"allowed"`
	Scope      int    `json:"scope,omitempty"` // for allow-always
	ScopeLabel string `json:"scope_label,omitempty"`
	Surface    string `json:"surface,omitempty"` // which surface the person answered on
	Via        string `json:"via"`               // the surface chain the node uses
	Reason     string `json:"reason,omitempty"`
}

// Client calls a running node's control API.
type Client struct {
	Base  string // http://127.0.0.1:7520
	Token string
	HTTP  *http.Client
}

// Dial locates the node owning the state directory.
func Dial(p state.Paths) (*Client, error) {
	run, err := p.LoadRunInfo()
	if err != nil {
		return nil, err
	}
	tok, err := p.ControlToken()
	if err != nil {
		return nil, err
	}
	return &Client{Base: "http://" + run.Local, Token: tok, HTTP: &http.Client{Timeout: 30 * time.Second}}, nil
}

// Do sends in as JSON (when non-nil) and decodes the response into out (when non-nil).
func (c *Client) Do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		data, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.Base+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		if op := (*net.OpError)(nil); errors.As(err, &op) && op.Op == "dial" {
			return fmt.Errorf("%w: nothing answers at %s: %v", state.ErrNodeNotRunning, c.Base, err)
		}
		return fmt.Errorf("node at %s: %w", c.Base, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e Error
		if json.Unmarshal(data, &e) == nil && e.Error != "" {
			return fmt.Errorf("%s", e.Error)
		}
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(data)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}
