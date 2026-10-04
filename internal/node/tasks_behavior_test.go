package node

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/state"
)

const taskCapabilityName = "io.modelcontextprotocol/tasks"

type taskBehaviorCapture struct {
	mu   sync.Mutex
	body string
}

func (c *taskBehaviorCapture) store(body string) {
	c.mu.Lock()
	c.body = body
	c.mu.Unlock()
}

func (c *taskBehaviorCapture) last() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.body
}

type taskBehaviorTransport struct {
	token   string
	capture *taskBehaviorCapture
}

func (t taskBehaviorTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+t.token)
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if resp.Body != nil {
		body, err := io.ReadAll(resp.Body)
		if err == nil {
			resp.Body.Close()
			resp.Body = io.NopCloser(strings.NewReader(string(body)))
			t.capture.store(string(body))
		}
	}
	return resp, nil
}

type taskBehaviorGetParams struct {
	mcp.ParamsBase
	TaskID string `json:"taskId"`
}

type taskBehaviorGetResult struct {
	mcp.ResultBase
	ResultType    string `json:"resultType"`
	TaskID        string `json:"taskId"`
	Status        string `json:"status"`
	StatusMessage string `json:"statusMessage,omitempty"`
	CreatedAt     string `json:"createdAt"`
	LastUpdatedAt string `json:"lastUpdatedAt"`
}

func taskBehaviorCreatedAt(t *testing.T, raw string) string {
	t.Helper()
	var response struct {
		Result struct {
			ResultType string `json:"resultType"`
			CreatedAt  string `json:"createdAt"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		t.Fatalf("decode task create response %q: %v", raw, err)
	}
	if response.Result.ResultType != "task" || response.Result.CreatedAt == "" {
		t.Fatalf("task create response has no admission time: %s", raw)
	}
	return response.Result.CreatedAt
}

type taskBehaviorCancelParams struct {
	mcp.ParamsBase
	TaskID string `json:"taskId"`
}

type taskBehaviorCancelResult struct {
	mcp.ResultBase
	ResultType string `json:"resultType"`
}

func taskBehaviorMeta() mcp.Meta {
	return mcp.Meta{
		"io.modelcontextprotocol/clientCapabilities": map[string]any{
			"extensions": map[string]any{
				taskCapabilityName: map[string]any{},
			},
		},
	}
}

func taskBehaviorSession(t *testing.T, n *Node, token string, capture *taskBehaviorCapture) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "node-task-behavior-test", Version: "1"}, nil)
	if err := mcp.AddSendingCustomMethod[*taskBehaviorGetParams, *taskBehaviorGetResult](client, "tasks/get"); err != nil {
		t.Fatal(err)
	}
	if err := mcp.AddSendingCustomMethod[*taskBehaviorCancelParams, *taskBehaviorCancelResult](client, "tasks/cancel"); err != nil {
		t.Fatal(err)
	}
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint: "http://" + n.LocalAddr() + "/mcp",
		HTTPClient: &http.Client{Transport: taskBehaviorTransport{
			token: token, capture: capture,
		}},
		MaxRetries:           -1,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func taskBehaviorID(t *testing.T, raw string) string {
	t.Helper()
	var response struct {
		Result struct {
			ResultType string `json:"resultType"`
			TaskID     string `json:"taskId"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(raw), &response); err != nil {
		t.Fatalf("decode tools/call task response %q: %v", raw, err)
	}
	if response.Result.ResultType != "task" || response.Result.TaskID == "" {
		t.Fatalf("tools/call returned no task union: %s", raw)
	}
	return response.Result.TaskID
}

func callTaskBehaviorTool(t *testing.T, ctx context.Context, session *mcp.ClientSession, name string, args map[string]any, meta mcp.Meta, capture *taskBehaviorCapture) string {
	t.Helper()
	_, err := session.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args, Meta: meta})
	if err != nil {
		t.Fatal(err)
	}
	return taskBehaviorID(t, capture.last())
}

func getTaskBehavior(t *testing.T, ctx context.Context, session *mcp.ClientSession, id string) *taskBehaviorGetResult {
	t.Helper()
	result, err := mcp.CallCustomMethod[*taskBehaviorGetParams, *taskBehaviorGetResult](ctx, session, "tasks/get", &taskBehaviorGetParams{
		ParamsBase: mcp.ParamsBase{Meta: taskBehaviorMeta()},
		TaskID:     id,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// TestTasksReportOfflineDeliveryAndUnconfirmedCancellationAsWorkingTasks runs
// through two real nodes. An origin task remains useful while the target is
// offline, and its identity survives reloading the origin's durable outbox.
func TestTasksReportOfflineDeliveryAndUnconfirmedCancellationAsWorkingTasks(t *testing.T) {
	target := startNode(t, "task-target")
	origin := startNode(t, "task-origin")
	pair(t, target, origin)

	token, err := origin.paths.AddAgent("worker")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.paths.AddAgent("worker"); err != nil {
		t.Fatal(err)
	}
	if err := origin.paths.SetAgentMode("worker", state.ToolsCompact); err != nil {
		t.Fatal(err)
	}
	if err := origin.peers.refresh(t.Context(), target.ID()); err != nil {
		t.Fatalf("refresh paired target: %v", err)
	}
	target.Close()

	capture := &taskBehaviorCapture{}
	session := taskBehaviorSession(t, origin, token, capture)
	args := map[string]any{
		"device": "task-target",
		"tool":   "job_submit",
		"arguments": map[string]any{
			"command":    os.Args[0],
			"args":       []string{"offline"},
			"request_id": "tasks-offline-state",
		},
	}
	firstID := callTaskBehaviorTool(t, t.Context(), session, "mesh_call", args, taskBehaviorMeta(), capture)
	firstCreatedAt := taskBehaviorCreatedAt(t, capture.last())
	createdTime, err := time.Parse(time.RFC3339Nano, firstCreatedAt)
	if err != nil || createdTime.IsZero() {
		t.Fatalf("offline task createdAt = %q, err=%v; want immutable non-zero admission time", firstCreatedAt, err)
	}
	firstJobID := taskIDFromOpaqueForTest(t, firstID)
	waitFor(t, "offline outbox delivery attempt", func() bool {
		record := origin.remoteJobs.find(target.ID(), "worker", firstJobID)
		return record != nil && record.Attempted
	})

	pending := getTaskBehavior(t, t.Context(), session, firstID)
	if pending.ResultType != "complete" || pending.TaskID != firstID || pending.Status != "working" ||
		!strings.Contains(strings.ToLower(pending.StatusMessage), "delivery") ||
		!strings.Contains(strings.ToLower(pending.StatusMessage), "not confirmed") {
		t.Fatalf("offline delivery task status = %+v; want working with honest unconfirmed delivery message", pending)
	}
	if pending.CreatedAt != firstCreatedAt || pending.LastUpdatedAt != firstCreatedAt {
		t.Fatalf("offline task times = created %q updated %q, create returned %q", pending.CreatedAt, pending.LastUpdatedAt, firstCreatedAt)
	}

	cancelled, err := mcp.CallCustomMethod[*taskBehaviorCancelParams, *taskBehaviorCancelResult](t.Context(), session, "tasks/cancel", &taskBehaviorCancelParams{
		ParamsBase: mcp.ParamsBase{Meta: taskBehaviorMeta()},
		TaskID:     firstID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.ResultType != "complete" {
		t.Fatalf("tasks/cancel result = %+v", cancelled)
	}
	waitFor(t, "durable unconfirmed task cancellation", func() bool {
		record := origin.remoteJobs.find(target.ID(), "worker", firstJobID)
		return record != nil && record.Cancel && record.State != "cancelled"
	})

	cancelling := getTaskBehavior(t, t.Context(), session, firstID)
	if cancelling.ResultType != "complete" || cancelling.TaskID != firstID || cancelling.Status != "working" ||
		!strings.Contains(strings.ToLower(cancelling.StatusMessage), "cancellation") ||
		!strings.Contains(strings.ToLower(cancelling.StatusMessage), "not confirmed") {
		t.Fatalf("unconfirmed cancellation task status = %+v; want working with honest cancellation message", cancelling)
	}
	if cancelling.CreatedAt != firstCreatedAt {
		t.Fatalf("cancellation changed task CreatedAt from %q to %q", firstCreatedAt, cancelling.CreatedAt)
	}

	origin.Close()
	restarted := restartRemoteJobsNode(t, origin)
	restartedToken, err := restarted.paths.AgentToken("worker")
	if err != nil {
		t.Fatal(err)
	}
	restartedCapture := &taskBehaviorCapture{}
	restartedSession := taskBehaviorSession(t, restarted, restartedToken, restartedCapture)
	secondID := callTaskBehaviorTool(t, t.Context(), restartedSession, "mesh_call", args, taskBehaviorMeta(), restartedCapture)
	if secondID != firstID {
		t.Fatalf("task identity changed after origin restart: before=%q after=%q", firstID, secondID)
	}
	afterRestart := getTaskBehavior(t, t.Context(), restartedSession, firstID)
	if afterRestart.ResultType != "complete" || afterRestart.TaskID != firstID || afterRestart.Status != "working" ||
		!strings.Contains(strings.ToLower(afterRestart.StatusMessage), "cancellation") ||
		!strings.Contains(strings.ToLower(afterRestart.StatusMessage), "not confirmed") {
		t.Fatalf("replayed task status after origin restart = %+v; want the same working, cancellation-unconfirmed task", afterRestart)
	}
	if afterRestart.CreatedAt != firstCreatedAt {
		t.Fatalf("origin restart changed task CreatedAt from %q to %q", firstCreatedAt, afterRestart.CreatedAt)
	}

	// A separate accepted task demonstrates that delivery and target status
	// do not replace the origin admission time encoded in the same task handle.
	targetOptions := target.opts
	targetOptions.MeshAddr = target.MeshAddr()
	targetOptions.LocalAddr = target.LocalAddr()
	restartedTarget, err := Start(t.Context(), targetOptions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restartedTarget.Close)
	if restartedTarget.ID() != target.ID() {
		t.Fatalf("restarted target identity changed from %q to %q", target.ID(), restartedTarget.ID())
	}
	if err := restarted.roster.Seen(restartedTarget.ID(), restartedTarget.MeshAddr(), time.Now()); err != nil {
		t.Fatalf("update restarted target address: %v", err)
	}
	deliveryArgs := map[string]any{
		"device": "task-target",
		"tool":   "job_submit",
		"arguments": map[string]any{
			"command":    os.Args[0],
			"args":       []string{"delivered"},
			"request_id": "tasks-delivered-state",
		},
	}
	deliveredID := callTaskBehaviorTool(t, t.Context(), restartedSession, "mesh_call", deliveryArgs, taskBehaviorMeta(), restartedCapture)
	deliveredCreatedAt := taskBehaviorCreatedAt(t, restartedCapture.last())
	if createdAt, err := time.Parse(time.RFC3339Nano, deliveredCreatedAt); err != nil || createdAt.IsZero() {
		t.Fatalf("delivered task createdAt = %q, err=%v; want non-zero admission time", deliveredCreatedAt, err)
	}
	deliveredJobID := taskIDFromOpaqueForTest(t, deliveredID)
	waitFor(t, "target job acceptance", func() bool {
		record := restarted.remoteJobs.find(restartedTarget.ID(), "worker", deliveredJobID)
		return record != nil && record.State != "pending_delivery" && record.State != "failed"
	})
	delivered := getTaskBehavior(t, t.Context(), restartedSession, deliveredID)
	if delivered.CreatedAt != deliveredCreatedAt {
		t.Fatalf("target delivery changed task CreatedAt from %q to %q", deliveredCreatedAt, delivered.CreatedAt)
	}
}

// The opaque gateway task reference carries the job ID used by the durable
// outbox; decode only that field to wait for real state transitions.
func taskIDFromOpaqueForTest(t *testing.T, id string) string {
	t.Helper()
	var ref struct {
		Job string `json:"j"`
	}
	raw, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		t.Fatalf("decode opaque task ID: %v", err)
	}
	if err := json.Unmarshal(raw, &ref); err != nil {
		t.Fatalf("decode opaque task reference: %v", err)
	}
	if ref.Job == "" {
		t.Fatalf("opaque task ID has no job reference: %q", id)
	}
	return ref.Job
}
