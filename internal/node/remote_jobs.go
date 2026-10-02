package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/jobs"
	"messh/internal/provider"
	"messh/internal/state"
)

const remoteJobWorkers = 4

type remoteJobRecord struct {
	ID               string
	DeviceID         string
	Agent            string
	AgentFingerprint string
	RequestID        string
	Args             json.RawMessage
	State            string
	Workspace        string
	Cached           json.RawMessage
	Deleted          bool
	Delete           bool
	Cancel           bool
	Attempt          int
	Attempted        bool
	NextTry          time.Time
	Updated          time.Time
	OriginSubmitted  time.Time
	OriginEvents     []jobs.Event
	OriginEventFloor uint64
	OriginEventState string
}

type remoteJobFile struct {
	Records                 []remoteJobRecord
	NextOriginEventSequence uint64
}

type remoteJobOutbox struct {
	mu                      sync.Mutex
	path                    string
	records                 map[string]*remoteJobRecord
	busy                    map[string]bool
	wake                    chan struct{}
	n                       *Node
	callPeer                func(context.Context, string, string, json.RawMessage, string) (*mcp.CallToolResult, error)
	nextOriginEventSequence uint64
}

func newRemoteJobOutbox(n *Node) (*remoteJobOutbox, error) {
	o := &remoteJobOutbox{
		n: n, path: filepath.Join(n.paths.Root, "remote-jobs.json"),
		records: make(map[string]*remoteJobRecord), busy: make(map[string]bool),
		wake: make(chan struct{}, 1),
	}
	data, err := os.ReadFile(o.path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		var file remoteJobFile
		if err := json.Unmarshal(data, &file); err != nil {
			return nil, fmt.Errorf("load remote job outbox: %w", err)
		}
		o.nextOriginEventSequence = file.NextOriginEventSequence
		legacyTimestamps := false
		for i := range file.Records {
			record := file.Records[i]
			if record.ID == "" || record.DeviceID == "" {
				return nil, errors.New("remote job outbox contains invalid record")
			}
			if record.OriginSubmitted.IsZero() {
				for _, event := range record.OriginEvents {
					if !event.At.IsZero() && (record.OriginSubmitted.IsZero() || event.At.Before(record.OriginSubmitted)) {
						record.OriginSubmitted = event.At
					}
				}
				if record.OriginSubmitted.IsZero() {
					record.OriginSubmitted = record.Updated
				}
				legacyTimestamps = legacyTimestamps || !record.OriginSubmitted.IsZero()
			}
			if bytes.Equal(bytes.TrimSpace(record.Cached), []byte("null")) {
				// A nil RawMessage is persisted as JSON null; normalize it to the
				// absent-cache state expected by all cached-result consumers.
				record.Cached = nil
			}
			o.records[record.ID] = &record
		}
		if legacyTimestamps {
			if err := o.saveLocked(); err != nil {
				return nil, fmt.Errorf("persist remote job admission timestamps: %w", err)
			}
		}
	}
	return o, nil
}

func (o *remoteJobOutbox) saveLocked() error {
	rollbackEvents := o.appendOriginEventsLocked()
	file := remoteJobFile{Records: make([]remoteJobRecord, 0, len(o.records)), NextOriginEventSequence: o.nextOriginEventSequence}
	for _, record := range o.records {
		file.Records = append(file.Records, *record)
	}
	data, err := json.Marshal(file)
	if err != nil {
		rollbackEvents()
		return err
	}
	if err := state.WriteFileAtomic(o.path, data, 0600); err != nil {
		rollbackEvents()
		return err
	}
	return nil
}

func (o *remoteJobOutbox) start() {
	for i := 0; i < remoteJobWorkers; i++ {
		o.n.goRun(o.worker)
	}
	o.signal()
}

func (o *remoteJobOutbox) signal() {
	select {
	case o.wake <- struct{}{}:
	default:
	}
}

func (o *remoteJobOutbox) worker() {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-o.n.ctx.Done():
			return
		case <-o.wake:
		case <-ticker.C:
		}
		now := time.Now()
		o.mu.Lock()
		ids := make([]string, 0)
		for id, record := range o.records {
			if (record.State == "pending_delivery" || record.Cancel || record.Delete) && !record.NextTry.After(now) {
				ids = append(ids, id)
			}
		}
		o.mu.Unlock()
		for _, id := range ids {
			if o.n.ctx.Err() != nil {
				return
			}
			o.deliver(id)
		}
	}
}

func (o *remoteJobOutbox) deliver(id string) {
	o.mu.Lock()
	if o.busy[id] {
		o.mu.Unlock()
		return
	}
	o.busy[id] = true
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		delete(o.busy, id)
		o.mu.Unlock()
	}()
	o.deliverOne(id)
}

func (o *remoteJobOutbox) deliverOne(id string) {
	o.mu.Lock()
	record := o.records[id]
	if record == nil || (record.Deleted && !record.Cancel && !record.Delete) || record.State == "cancelled" {
		o.mu.Unlock()
		return
	}
	rec := *record
	o.mu.Unlock()
	if !o.n.beginWork() {
		return
	}
	defer o.n.endWork()
	if !o.agentAuthorized(rec.Agent, rec.AgentFingerprint) {
		o.removeAgent(rec.Agent)
		return
	}
	if _, paired := o.n.roster.Get(rec.DeviceID); !paired {
		return
	}
	ctx, cancel := context.WithTimeout(o.n.ctx, 30*time.Second)
	defer cancel()

	if rec.State == "pending_delivery" {
		o.mu.Lock()
		record = o.records[id]
		if record == nil || record.State != "pending_delivery" {
			o.mu.Unlock()
			return
		}
		old := *record
		record.Attempted = true
		record.Updated = time.Now()
		if err := o.saveLocked(); err != nil {
			*record = old
			o.mu.Unlock()
			o.n.log.Error("persist remote job attempt", "job_id", id, "error", err)
			return
		}
		rec = *record
		o.mu.Unlock()

		result, err := o.peerCall(ctx, rec.DeviceID, "job_submit", rec.Args, rec.Agent)
		if err != nil {
			if retryableRemote(err) {
				o.retry(id)
			} else {
				o.cache(id, provider.ErrorResult("target rejected job submission: %v", err), "failed")
			}
			return
		}
		if result != nil && result.IsError {
			o.cache(id, result, "failed")
			return
		}
		o.cache(id, result, resultState(result))
	}

	o.mu.Lock()
	record = o.records[id]
	if record == nil {
		o.mu.Unlock()
		return
	}
	rec = *record
	o.mu.Unlock()
	if rec.Cancel {
		result, err := o.peerCall(ctx, rec.DeviceID, "job_cancel", mustJSON(map[string]string{"job_id": id}), rec.Agent)
		if err != nil {
			if retryableRemote(err) {
				o.retry(id)
			} else {
				o.markCancellationUnconfirmed(id)
			}
			return
		}
		if result != nil && result.IsError {
			o.cache(id, result, "")
			o.clearCancel(id)
			return
		}
		stateName := resultState(result)
		o.cache(id, result, stateName)
		if rec.Delete && !isFinalRemoteJob(stateName) {
			o.retry(id)
			return
		}
		o.clearCancel(id)
	}

	o.mu.Lock()
	record = o.records[id]
	if record == nil {
		o.mu.Unlock()
		return
	}
	rec = *record
	o.mu.Unlock()
	if rec.Delete {
		o.deleteRemote(ctx, id, rec)
	}
}

func retryableRemote(err error) bool {
	var callErr *peerCallError
	if errors.As(err, &callErr) {
		return !callErr.response && !strings.Contains(err.Error(), "Forbidden")
	}
	var rpcErr *jsonrpc.Error
	if errors.As(err, &rpcErr) {
		return false
	}
	return !strings.Contains(err.Error(), "Forbidden")
}

func resultState(result *mcp.CallToolResult) string {
	if result == nil || len(result.Content) == 0 {
		return ""
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		return ""
	}
	var value struct {
		State string `json:"state"`
	}
	_ = json.Unmarshal([]byte(text.Text), &value)
	return value.State
}

func (o *remoteJobOutbox) cache(id string, result *mcp.CallToolResult, stateName string) {
	var text string
	if result != nil && len(result.Content) > 0 {
		if content, ok := result.Content[0].(*mcp.TextContent); ok {
			text = content.Text
		}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	record := o.records[id]
	if record == nil {
		return
	}
	old := *record
	if stateName != "" {
		record.State = stateName
	}
	workspace := remoteJobWorkspace(record)
	switch {
	case result != nil && result.IsError:
		reason := text
		if reason == "" {
			reason = "remote job operation failed"
		}
		if stateName == "failed" {
			record.State = "failed"
		}
		receipt := map[string]any{
			"job_id": record.ID, "state": record.State, "workspace": "ws/" + workspace, "error": reason,
		}
		if structured, ok := result.StructuredContent.(map[string]any); ok {
			if code, ok := structured["code"].(string); ok && code != "" {
				receipt["code"] = code
			}
		}
		record.Cached = mustJSON(receipt)
	case json.Valid([]byte(text)):
		record.Cached = append(json.RawMessage(nil), text...)
	case text != "":
		record.Cached = mustJSON(map[string]any{
			"job_id": record.ID, "state": record.State, "workspace": "ws/" + workspace, "message": text,
		})
	default:
		record.Cached = mustJSON(map[string]any{
			"job_id": record.ID, "state": record.State, "workspace": "ws/" + workspace,
		})
	}
	record.Attempt = 0
	record.NextTry = time.Time{}
	record.Updated = time.Now()
	if err := o.saveLocked(); err != nil {
		*record = old
		record.NextTry = time.Now().Add(time.Second)
		o.n.log.Error("persist remote job result", "job_id", id, "error", err)
	}
}

func (o *remoteJobOutbox) clearCancel(id string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if record := o.records[id]; record != nil {
		old := *record
		record.Cancel = false
		record.Updated = time.Now()
		if err := o.saveLocked(); err != nil {
			*record = old
			o.n.log.Error("persist remote job cancellation result", "job_id", id, "error", err)
		}
	}
}

func (o *remoteJobOutbox) retry(id string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if record := o.records[id]; record != nil {
		old := *record
		record.Attempt++
		delay := time.Second << min(record.Attempt, 8)
		record.NextTry = time.Now().Add(delay)
		record.Updated = time.Now()
		if err := o.saveLocked(); err != nil {
			*record = old
			o.n.log.Error("persist remote job retry", "job_id", id, "error", err)
		}
	}
}

func (o *remoteJobOutbox) submit(_ context.Context, deviceID, agent string, args json.RawMessage) (*mcp.CallToolResult, error) {
	fingerprint, authorized := o.agentFingerprint(agent)
	if !authorized {
		return provider.ErrorResult("agent is no longer authorized to submit jobs"), nil
	}
	if err := jobs.ValidateSubmission(args); err != nil {
		return provider.ErrorResult("invalid job submission: %v", err), nil
	}
	var input map[string]json.RawMessage
	if err := json.Unmarshal(args, &input); err != nil {
		return provider.ErrorResult("invalid job submission: %v", err), nil
	}
	var requestID string
	if raw, ok := input["request_id"]; ok {
		_ = json.Unmarshal(raw, &requestID)
	}
	if requestID == "" {
		requestID = makeRequestID()
		input["request_id"], _ = json.Marshal(requestID)
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	id := jobs.SubmissionID(o.n.id.ID, agent, requestID)
	var workspace string
	if raw, ok := input["workspace"]; ok {
		_ = json.Unmarshal(raw, &workspace)
	}
	if workspace == "" {
		workspace = id
	}
	o.mu.Lock()
	if existing := o.records[id]; existing != nil {
		if existing.DeviceID != deviceID || existing.Agent != agent || existing.AgentFingerprint != fingerprint {
			o.mu.Unlock()
			return provider.ErrorResult("job request_id belongs to a different device or agent credential"), nil
		}
		if !sameSubmission(existing.Args, encoded) {
			o.mu.Unlock()
			return provider.ErrorResult("request_id was already used for a different job submission"), nil
		}
		if existing.Deleted {
			o.mu.Unlock()
			return provider.ErrorResult("job request_id was deleted and cannot be resubmitted"), nil
		}
		copy := *existing
		o.mu.Unlock()
		return submissionResult(&copy), nil
	}
	now := time.Now().UTC()
	record := &remoteJobRecord{
		ID: id, DeviceID: deviceID, Agent: agent, AgentFingerprint: fingerprint,
		RequestID: requestID, Args: encoded, State: "pending_delivery", Workspace: workspace, Updated: now, OriginSubmitted: now,
	}
	if _, ok := o.records[id]; ok {
		o.mu.Unlock()
		return provider.ErrorResult("job request_id conflicts with an existing submission"), nil
	}
	o.records[id] = record
	if err := o.saveLocked(); err != nil {
		delete(o.records, id)
		o.mu.Unlock()
		return provider.ErrorResult("could not durably queue job submission: %v", err), nil
	}
	copy := *record
	o.mu.Unlock()
	o.signal()
	return submissionResult(&copy), nil
}

func sameSubmission(a, b json.RawMessage) bool {
	ha, errA := jobs.SubmissionHash(a)
	hb, errB := jobs.SubmissionHash(b)
	return errA == nil && errB == nil && ha == hb
}

func makeRequestID() string { return randomID() }

func remoteJobWorkspace(record *remoteJobRecord) string {
	if record.Workspace != "" {
		return record.Workspace
	}
	return record.ID
}
func submissionResult(record *remoteJobRecord) *mcp.CallToolResult {
	if record.Cancel && !isFinalRemoteJob(record.State) {
		result, err := provider.JSONResult(map[string]any{
			"job_id": record.ID, "state": "cancellation_pending", "workspace": "ws/" + remoteJobWorkspace(record),
			"message": "cancellation request is saved; target has not confirmed the job stopped",
			"origin_submitted": record.OriginSubmitted,
		})
		if err == nil {
			return result
		}
	}
	if len(record.Cached) > 0 {
		var value map[string]any
		// A nil RawMessage is persisted as JSON null. It is an absent cache, not
		// a receipt; after reload RawMessage contains the nonempty bytes "null".
		if json.Unmarshal(record.Cached, &value) == nil && value != nil {
			value["origin_submitted"] = record.OriginSubmitted
			if result, err := provider.JSONResult(value); err == nil {
				return result
			}
		}
	}
	return syntheticStatus(record)
}

func syntheticStatus(record *remoteJobRecord) *mcp.CallToolResult {
	stateName := record.State
	if record.Cancel && !isFinalRemoteJob(stateName) {
		stateName = "cancellation_pending"
	}
	if stateName == "" {
		stateName = "pending_delivery"
	}
	message := "accepted locally; waiting for target delivery"
	if stateName == "cancellation_pending" {
		message = "cancellation request is saved; target has not confirmed the job stopped"
	} else if stateName == "cancelled" {
		message = "cancelled before target delivery"
	} else if stateName == "failed" {
		message = "target rejected job submission"
	}
	result, err := provider.JSONResult(map[string]any{
		"job_id": record.ID, "state": stateName, "workspace": "ws/" + remoteJobWorkspace(record), "message": message, "origin_submitted": record.OriginSubmitted,
	})
	if err != nil {
		return provider.ErrorResult("could not encode job status: %v", err)
	}
	return result
}


func (o *remoteJobOutbox) call(ctx context.Context, deviceID, tool string, args json.RawMessage, agent string) (*mcp.CallToolResult, error) {
	fingerprint, authorized := o.agentFingerprint(agent)
	if !authorized {
		return provider.ErrorResult("agent is no longer authorized"), nil
	}
	var request struct {
		JobID string `json:"job_id"`
	}
	_ = json.Unmarshal(args, &request)
	if request.JobID != "" && o.revokedRecord(deviceID, agent, request.JobID, fingerprint) {
		return provider.ErrorResult("historical job access was revoked with the prior agent token"), nil
	}
	if tool == "job_list" && o.revokedRecord(deviceID, agent, "@list", fingerprint) {
		return provider.ErrorResult("historical job access was revoked with the prior agent token"), nil
	}
	if _, paired := o.n.roster.Get(deviceID); !paired {
		return provider.ErrorResult("device is not paired"), nil
	}
	if tool == "job_events" {
		return o.events(ctx, deviceID, agent, fingerprint, args)
	}
	if tool == "job_submit" {
		return o.submit(ctx, deviceID, agent, args)
	}
	if tool == "job_cancel" && request.JobID != "" {
		if result, handled := o.cancelPending(deviceID, agent, request.JobID); handled {
			return result, nil
		}
	}
	if tool == "job_delete" && request.JobID != "" {
		return o.delete(ctx, deviceID, agent, request.JobID)
	}
	result, err := o.peerCall(ctx, deviceID, tool, args, agent)
	if err != nil {
		return o.stale(deviceID, agent, tool, args, err)
	}
	if result != nil && result.IsError {
		return result, nil
	}
	if tool == "job_list" {
		o.remember(deviceID, agent, "@list", result)
	} else if request.JobID != "" && (tool == "job_status" || tool == "job_wait" || tool == "job_cancel") {
		o.remember(deviceID, agent, request.JobID, result)
	}
	return result, nil
}

func (o *remoteJobOutbox) find(deviceID, agent, id string) *remoteJobRecord {
	fingerprint, ok := o.agentFingerprint(agent)
	if !ok {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	record := o.records[id]
	if record == nil || record.AgentFingerprint != fingerprint || record.DeviceID != deviceID || record.Agent != agent {
		return nil
	}
	copy := *record
	return &copy
}

func (o *remoteJobOutbox) remember(deviceID, agent, id string, result *mcp.CallToolResult) {
	if result == nil || len(result.Content) == 0 {
		return
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || !json.Valid([]byte(text.Text)) {
		return
	}
	fingerprint, authorized := o.agentFingerprint(agent)
	if !authorized {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	record := o.records[id]
	if record == nil {
		record = &remoteJobRecord{ID: id, DeviceID: deviceID, Agent: agent, AgentFingerprint: fingerprint}
		o.records[id] = record
	}
	if record.DeviceID != deviceID || record.Agent != agent || record.AgentFingerprint != fingerprint {
		return
	}
	old := *record
	record.Cached = []byte(text.Text)
	record.State = resultState(result)
	record.Attempted = true
	record.Updated = time.Now()
	if err := o.saveLocked(); err != nil {
		*record = old
		o.n.log.Warn("cache remote job status", "error", err)
	}
}

func (o *remoteJobOutbox) stale(deviceID, agent, tool string, args json.RawMessage, cause error) (*mcp.CallToolResult, error) {
	var request struct {
		JobID string `json:"job_id"`
	}
	_ = json.Unmarshal(args, &request)
	if tool == "job_list" {
		return o.staleList(deviceID, agent, cause)
	}
	if tool == "job_logs" {
		return provider.ErrorResult("target is unreachable; logs are not cached (%v)", cause), nil
	}
	record := o.find(deviceID, agent, request.JobID)
	if record != nil && record.State == "cancelled" && !record.Attempted {
		return provider.JSONResult(map[string]any{"job_id": record.ID, "state": "cancelled", "reason": "cancelled before target delivery"})
	}
	if record != nil && record.Deleted && record.State != "deleted" {
		return provider.JSONResult(map[string]any{"job_id": record.ID, "state": "deletion_pending", "stale": true, "stale_reason": "target unreachable; deletion is not yet confirmed"})
	}
	if record != nil && record.Cancel && record.State != "cancelled" {
		return submissionResult(record), nil
	}
	if record != nil && len(record.Cached) > 0 {
		var value any
		if json.Unmarshal(record.Cached, &value) == nil {
			if object, ok := value.(map[string]any); ok {
				object["stale"] = true
				object["stale_reason"] = "target unreachable; showing last known state"
				return provider.JSONResult(object)
			}
		}
	}
	if record != nil && record.State == "pending_delivery" {
		return syntheticStatus(record), nil
	}
	return provider.ErrorResult("target is unreachable and no cached job status is available (%v)", cause), nil
}

func (o *remoteJobOutbox) staleList(deviceID, agent string, cause error) (*mcp.CallToolResult, error) {
	if record := o.find(deviceID, agent, "@list"); record != nil && len(record.Cached) > 0 {
		var value any
		if json.Unmarshal(record.Cached, &value) == nil {
			if object, ok := value.(map[string]any); ok {
				object["stale"] = true
				object["stale_reason"] = "target unreachable; showing last known state"
				return provider.JSONResult(object)
			}
		}
	}
	o.mu.Lock()
	rows := make([]any, 0)
	for _, record := range o.records {
		if record.DeviceID != deviceID || record.Agent != agent || record.AgentFingerprint == "" || record.Deleted || !o.agentAuthorized(agent, record.AgentFingerprint) {
			continue
		}
		if len(record.Cached) > 0 {
			var value any
			if json.Unmarshal(record.Cached, &value) == nil {
				if object, ok := value.(map[string]any); ok {
					object["stale"] = true
					value = object
				}
				rows = append(rows, value)
			}
		} else if record.State == "pending_delivery" || record.Cancel {
			stateName := record.State
			if record.Cancel {
				stateName = "cancellation_pending"
			}
			rows = append(rows, map[string]any{"job_id": record.ID, "state": stateName, "stale": true})
		}
	}
	o.mu.Unlock()
	return provider.JSONResult(map[string]any{"jobs": rows, "stale": true, "stale_reason": fmt.Sprintf("target unreachable; no cached job list (%v)", cause)})
}

func (o *remoteJobOutbox) cancelPending(deviceID, agent, id string) (*mcp.CallToolResult, bool) {
	fingerprint, authorized := o.agentFingerprint(agent)
	if !authorized {
		return provider.ErrorResult("agent is no longer authorized"), true
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	record := o.records[id]
	if record == nil || record.DeviceID != deviceID || record.Agent != agent || record.AgentFingerprint != fingerprint || record.State == "cancelled" || isFinalRemoteJob(record.State) {
		return nil, false
	}
	record.Cancel = true
	if !record.Attempted && record.State == "pending_delivery" {
		record.State = "cancelled"
		record.Cancel = false
	}
	record.Updated = time.Now()
	if err := o.saveLocked(); err != nil {
		return provider.ErrorResult("cannot persist cancellation: %v", err), true
	}
	o.signal()
	copy := *record
	return submissionResult(&copy), true
}

func (o *remoteJobOutbox) delete(ctx context.Context, deviceID, agent, id string) (*mcp.CallToolResult, error) {
	fingerprint, authorized := o.agentFingerprint(agent)
	if !authorized {
		return provider.ErrorResult("agent is no longer authorized"), nil
	}
	o.mu.Lock()
	record := o.records[id]
	if record != nil && record.DeviceID == deviceID && record.Agent == agent && record.AgentFingerprint != fingerprint {
		o.mu.Unlock()
		return provider.ErrorResult("historical job access was revoked with the prior agent token"), nil
	}
	if record == nil || record.DeviceID != deviceID || record.Agent != agent {
		o.mu.Unlock()
		return o.peerCall(ctx, deviceID, "job_delete", mustJSON(map[string]string{"job_id": id}), agent)
	}
	old := *record
	record.Deleted = true
	record.Delete = true
	if !isFinalRemoteJob(record.State) {
		record.Cancel = true
	}
	if !record.Attempted && record.State == "pending_delivery" {
		record.State = "deleted"
		record.Delete = false
		record.Cancel = false
	}
	record.Updated = time.Now()
	if err := o.saveLocked(); err != nil {
		*record = old
		o.mu.Unlock()
		return provider.ErrorResult("cannot persist deletion request: %v", err), nil
	}
	if !record.Delete {
		o.mu.Unlock()
		return provider.JSONResult(map[string]any{"deleted": id, "workspace_removed": false})
	}
	o.mu.Unlock()
	o.signal()
	return provider.JSONResult(map[string]any{"job_id": id, "state": "deletion_pending", "stale": true, "stale_reason": "deletion request is saved; target has not confirmed removal"})
}

func mustJSON(value any) json.RawMessage {
	data, _ := json.Marshal(value)
	return data
}

func (o *remoteJobOutbox) pairedChanged() {
	paired := make(map[string]bool)
	for _, peer := range o.n.roster.List() {
		paired[peer.ID] = true
	}
	o.mu.Lock()
	changed := false
	for id, record := range o.records {
		if !paired[record.DeviceID] {
			delete(o.records, id)
			changed = true
		}
	}
	if changed {
		if err := o.saveLocked(); err != nil {
			o.n.log.Error("remove unpaired remote jobs", "error", err)
		}
	}
	o.mu.Unlock()
}

func isFinalRemoteJob(stateName string) bool {
	return stateName == "succeeded" || stateName == "failed" || stateName == "cancelled" || stateName == "interrupted"
}

func (o *remoteJobOutbox) deleteRemote(ctx context.Context, id string, record remoteJobRecord) {
	result, err := o.peerCall(ctx, record.DeviceID, "job_delete", mustJSON(map[string]string{"job_id": id}), record.Agent)
	if err != nil {
		if retryableRemote(err) {
			o.retry(id)
			return
		}
		o.resetDelete(id)
		return
	}
	if result != nil && result.IsError {
		o.cache(id, result, "")
		o.resetDelete(id)
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if current := o.records[id]; current != nil {
		old := *current
		current.Cached = nil
		current.Delete = false
		current.Cancel = false
		current.Deleted = true
		current.State = "deleted"
		current.Updated = time.Now()
		if err := o.saveLocked(); err != nil {
			*current = old
			o.n.log.Error("persist remote job deletion result", "job_id", id, "error", err)
		}
	}
}

func (o *remoteJobOutbox) resetDelete(id string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if record := o.records[id]; record != nil {
		old := *record
		record.Delete = false
		record.Cancel = false
		record.Deleted = false
		record.Updated = time.Now()
		if err := o.saveLocked(); err != nil {
			*record = old
			o.n.log.Error("persist remote job delete result", "job_id", id, "error", err)
		}
	}
}

func (o *remoteJobOutbox) markCancellationUnconfirmed(id string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if record := o.records[id]; record != nil {
		old := *record
		record.State = "cancellation_pending"
		record.Cancel = true
		record.NextTry = time.Now().Add(time.Hour)
		record.Updated = time.Now()
		if err := o.saveLocked(); err != nil {
			*record = old
			o.n.log.Error("persist unconfirmed cancellation", "job_id", id, "error", err)
		}
	}
}

func (o *remoteJobOutbox) peerCall(ctx context.Context, id, tool string, args json.RawMessage, agent string) (*mcp.CallToolResult, error) {
	o.mu.Lock()
	callPeer := o.callPeer
	o.mu.Unlock()
	if callPeer != nil {
		return callPeer(ctx, id, tool, args, agent)
	}
	return o.n.peers.callRaw(ctx, id, tool, args, agent)
}

func (o *remoteJobOutbox) setPeerCall(fn func(context.Context, string, string, json.RawMessage, string) (*mcp.CallToolResult, error)) {
	o.mu.Lock()
	o.callPeer = fn
	o.mu.Unlock()
}

func (o *remoteJobOutbox) agentFingerprint(agent string) (string, bool) {
	var token string
	if agent == cliAgent {
		token = o.n.controlToken
	} else {
		var err error
		token, err = o.n.paths.AgentToken(agent)
		if err != nil {
			return "", false
		}
	}
	if token == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:]), true
}

func (o *remoteJobOutbox) agentAuthorized(agent, fingerprint string) bool {
	current, ok := o.agentFingerprint(agent)
	return ok && fingerprint != "" && current == fingerprint
}

func (o *remoteJobOutbox) removeAgent(agent string) {
	o.mu.Lock()
	changed := false
	for id, record := range o.records {
		if record.Agent == agent {
			delete(o.records, id)
			changed = true
		}
	}
	if changed {
		if err := o.saveLocked(); err != nil {
			o.n.log.Error("remove revoked agent outbox records", "agent", agent, "error", err)
		}
	}
	o.mu.Unlock()
}

func (o *remoteJobOutbox) revokedRecord(deviceID, agent, id, fingerprint string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	record := o.records[id]
	return record != nil && record.DeviceID == deviceID && record.Agent == agent && record.AgentFingerprint != fingerprint
}
