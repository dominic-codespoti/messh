package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const tasksExtension = "io.modelcontextprotocol/tasks"

type taskRef struct {
	Device    string    `json:"d"`
	Job       string    `json:"j"`
	Agent     string    `json:"a"`
	Submitted time.Time `json:"s"`
}
type taskBase struct {
	TaskID         string `json:"taskId"`
	Status         string `json:"status"`
	StatusMessage  string `json:"statusMessage,omitempty"`
	CreatedAt      string `json:"createdAt"`
	LastUpdatedAt  string `json:"lastUpdatedAt"`
	TTLMS          *int64 `json:"ttlMs"`
	PollIntervalMS int    `json:"pollIntervalMs,omitempty"`
}
type createTaskResult struct {
	mcp.ResultBase
	ResultType string `json:"resultType"`
	taskBase
}
type getTaskParams struct {
	mcp.ParamsBase
	TaskID string `json:"taskId"`
}
type updateTaskParams struct {
	mcp.ParamsBase
	TaskID         string                     `json:"taskId"`
	InputResponses map[string]json.RawMessage `json:"inputResponses,omitempty"`
}
type cancelTaskParams struct {
	mcp.ParamsBase
	TaskID string `json:"taskId"`
}
type taskReply struct {
	mcp.ResultBase
	ResultType string `json:"resultType"`
	taskBase
	Result *mcp.CallToolResult `json:"result,omitempty"`
}
type emptyTaskReply struct {
	mcp.ResultBase
	ResultType string `json:"resultType"`
}
type taskAgentKey struct{}

func (g *Gateway) installTasks() {
	for _, s := range []*mcp.Server{g.full, g.compact} {
		if err := mcp.AddReceivingCustomMethod(s, "tasks/get", g.getTask); err != nil {
			panic(err)
		}
		if err := mcp.AddReceivingCustomMethod(s, "tasks/cancel", g.cancelTask); err != nil {
			panic(err)
		}
		if err := mcp.AddReceivingCustomMethod(s, "tasks/update", g.updateTask); err != nil {
			panic(err)
		}
		s.AddReceivingMiddleware(g.taskContextMiddleware, g.taskMiddleware)
	}
}
func (g *Gateway) taskContextMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method == "tasks/get" || method == "tasks/cancel" || method == "tasks/update" {
			negotiated, ok := req.(interface {
				ClientCapabilities() *mcp.ClientCapabilities
			})
			if !ok || !supportsTasks(negotiated.ClientCapabilities()) {
				data := json.RawMessage(`{"requiredCapabilities":{"extensions":{"io.modelcontextprotocol/tasks":{}}}}`)
				return nil, &jsonrpc.Error{Code: -32021, Message: "Missing required client capability", Data: data}
			}
		}
		if e := req.GetExtra(); e != nil && e.TokenInfo != nil {
			ctx = context.WithValue(ctx, taskAgentKey{}, e.TokenInfo.UserID)
		}
		return next(ctx, method, req)
	}
}
func (g *Gateway) taskMiddleware(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method != "tools/call" {
			return next(ctx, method, req)
		}
		call, ok := req.(*mcp.CallToolRequest)
		if !ok || !supportsTasks(call.ClientCapabilities()) {
			return next(ctx, method, req)
		}
		ref, eligible := g.taskTarget(call)
		if !eligible {
			return next(ctx, method, req)
		}
		result, err := next(ctx, method, req)
		if err != nil {
			return result, err
		}
		cr, ok := result.(*mcp.CallToolResult)
		if !ok || cr == nil || cr.IsError {
			return result, nil
		}
		jobID := resultJobID(cr)
		if jobID == "" {
			return result, nil
		}
		ref.Job = jobID
		ref.Agent = agentOf(call)
		var admission struct {
			Submitted       time.Time `json:"submitted"`
			OriginSubmitted time.Time `json:"origin_submitted"`
		}
		if cr.StructuredContent != nil {
			b, _ := json.Marshal(cr.StructuredContent)
			_ = json.Unmarshal(b, &admission)
		}
		if !admission.OriginSubmitted.IsZero() {
			ref.Submitted = admission.OriginSubmitted
		} else {
			ref.Submitted = admission.Submitted
		}
		taskID := encodeTaskRef(ref)
		created := ref.Submitted.UTC().Format(time.RFC3339Nano)
		msg := "Job accepted by the target device."
		if cr.StructuredContent != nil {
			b, _ := json.Marshal(cr.StructuredContent)
			var v struct {
				State   string `json:"state"`
				Message string `json:"message"`
			}
			if json.Unmarshal(b, &v) == nil {
				msg = v.Message
				if v.State == "awaiting_approval" {
					msg = "Waiting for device owner approval."
				}
			}
		}
		return &createTaskResult{ResultType: "task", taskBase: taskBase{TaskID: taskID, Status: "working", StatusMessage: msg, CreatedAt: created, LastUpdatedAt: created, PollIntervalMS: 1000}}, nil
	}
}
func supportsTasks(c *mcp.ClientCapabilities) bool {
	if c == nil {
		return false
	}
	_, ok := c.Extensions[tasksExtension]
	return ok
}
func (g *Gateway) taskTarget(req *mcp.CallToolRequest) (taskRef, bool) {
	if req.Params == nil {
		return taskRef{}, false
	}
	if req.Params.Name != "mesh_call" {
		g.mu.Lock()
		r, ok := g.tools[req.Params.Name]
		g.mu.Unlock()
		if ok && r.tool == "job_submit" {
			return taskRef{Device: r.deviceID}, true
		}
		return taskRef{}, false
	}
	var a struct {
		Device string `json:"device"`
		Tool   string `json:"tool"`
	}
	if json.Unmarshal(req.Params.Arguments, &a) != nil || a.Tool != "job_submit" {
		return taskRef{}, false
	}
	d, err := resolve(g.snapshot(), a.Device)
	if err != nil {
		return taskRef{}, false
	}
	for _, t := range d.tools {
		if t.Name == a.Tool {
			return taskRef{Device: d.id}, true
		}
	}
	return taskRef{}, false
}
func resultJobID(r *mcp.CallToolResult) string {
	if r.StructuredContent != nil {
		b, _ := json.Marshal(r.StructuredContent)
		var v struct {
			JobID string `json:"job_id"`
		}
		if json.Unmarshal(b, &v) == nil && v.JobID != "" {
			return v.JobID
		}
	}
	for _, c := range r.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			var v struct {
				JobID string `json:"job_id"`
			}
			if json.Unmarshal([]byte(t.Text), &v) == nil && v.JobID != "" {
				return v.JobID
			}
		}
	}
	return ""
}
func encodeTaskRef(r taskRef) string {
	b, _ := json.Marshal(r)
	return base64.RawURLEncoding.EncodeToString(b)
}
func decodeTaskRef(id, agent string) (taskRef, error) {
	var r taskRef
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil || json.Unmarshal(b, &r) != nil || r.Device == "" || r.Job == "" || r.Agent == "" || r.Agent != agent {
		return taskRef{}, errors.New("unknown task")
	}
	return r, nil
}
func taskAgent(ctx context.Context) string { v, _ := ctx.Value(taskAgentKey{}).(string); return v }
func (g *Gateway) getTask(ctx context.Context, _ *mcp.ServerSession, p *getTaskParams) (*taskReply, error) {
	r, err := decodeTaskRef(p.TaskID, taskAgent(ctx))
	if err != nil {
		return nil, err
	}
	arg, _ := json.Marshal(map[string]string{"job_id": r.Job})
	res, err := g.backend.Call(ctx, r.Device, "job_status", arg, r.Agent)
	if err != nil {
		return nil, err
	}
	if res == nil || res.IsError {
		return nil, errors.New("task target has no readable job status")
	}
	var st struct {
		JobID           string     `json:"job_id"`
		State           string     `json:"state"`
		Reason          string     `json:"reason"`
		Stale           bool       `json:"stale"`
		Submitted       time.Time  `json:"submitted"`
		OriginSubmitted time.Time  `json:"origin_submitted"`
		Finished        *time.Time `json:"finished,omitempty"`
	}
	raw, _ := json.Marshal(res.StructuredContent)
	if len(raw) == 0 || json.Unmarshal(raw, &st) != nil || st.JobID != r.Job {
		return nil, errors.New("target returned an invalid job status")
	}
	admitted := r.Submitted
	if admitted.IsZero() {
		if !st.OriginSubmitted.IsZero() {
			admitted = st.OriginSubmitted
		} else {
			admitted = st.Submitted
		}
	}
	created := admitted.UTC().Format(time.RFC3339Nano)
	updated := created
	if st.Finished != nil {
		updated = st.Finished.UTC().Format(time.RFC3339Nano)
	}
	base := taskBase{TaskID: p.TaskID, Status: "working", CreatedAt: created, LastUpdatedAt: updated, PollIntervalMS: 1000}
	if st.Reason != "" {
		base.StatusMessage = st.Reason
	}
	reply := &taskReply{ResultType: "complete", taskBase: base}
	switch st.State {
	case "awaiting_approval":
		reply.StatusMessage = "Waiting for device owner approval."
	case "pending_delivery":
		reply.StatusMessage = "Accepted at the origin; target delivery is not confirmed."
	case "cancellation_pending":
		reply.StatusMessage = "Cancellation is saved at the origin; the target has not confirmed the job stopped."
	case "succeeded", "failed", "interrupted":
		reply.Status = "completed"
		reply.Result = res
	case "cancelled":
		reply.Status = "cancelled"
	case "submitted", "queued", "running":
	default:
		return nil, fmt.Errorf("target returned unknown job state %q", st.State)
	}
	if st.Stale {
		reply.StatusMessage = "Cached target status; not live confirmation. " + reply.StatusMessage
	}
	return reply, nil
}
func (g *Gateway) cancelTask(ctx context.Context, _ *mcp.ServerSession, p *cancelTaskParams) (*emptyTaskReply, error) {
	r, err := decodeTaskRef(p.TaskID, taskAgent(ctx))
	if err != nil {
		return nil, err
	}
	arg, _ := json.Marshal(map[string]string{"job_id": r.Job})
	cancelResult, err := g.backend.Call(ctx, r.Device, "job_cancel", arg, r.Agent)
	if err != nil {
		return nil, err
	}
	if cancelResult == nil || cancelResult.IsError {
		return nil, errors.New("task target rejected cancellation")
	}
	return &emptyTaskReply{ResultType: "complete"}, nil
}
func (g *Gateway) updateTask(ctx context.Context, _ *mcp.ServerSession, p *updateTaskParams) (*emptyTaskReply, error) {
	if _, err := decodeTaskRef(p.TaskID, taskAgent(ctx)); err != nil {
		return nil, err
	}
	return nil, errors.New("this task has no outstanding client input requests")
}
