package gateway

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type responseCapture struct {
	mu   sync.Mutex
	body string
}
type captureAuth struct {
	agent   string
	capture *responseCapture
}

func (c captureAuth) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer tok-"+c.agent)
	resp, err := http.DefaultTransport.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if resp.Body != nil {
		b, e := io.ReadAll(resp.Body)
		if e == nil {
			resp.Body.Close()
			resp.Body = io.NopCloser(strings.NewReader(string(b)))
			c.capture.mu.Lock()
			c.capture.body = string(b)
			c.capture.mu.Unlock()
		}
	}
	return resp, nil
}
func (c *responseCapture) last() string { c.mu.Lock(); defer c.mu.Unlock(); return c.body }

type taskGetTestParams struct {
	mcp.ParamsBase
	TaskID string `json:"taskId"`
}
type taskGetTestResult struct {
	mcp.ResultBase
	ResultType    string `json:"resultType"`
	TaskID        string `json:"taskId"`
	Status        string `json:"status"`
	StatusMessage string `json:"statusMessage,omitempty"`
}
type taskCancelTestParams struct {
	mcp.ParamsBase
	TaskID string `json:"taskId"`
}
type taskCancelTestResult struct {
	mcp.ResultBase
	ResultType string `json:"resultType"`
}

func taskMeta() mcp.Meta {
	return mcp.Meta{"io.modelcontextprotocol/clientCapabilities": map[string]any{"extensions": map[string]any{tasksExtension: map[string]any{}}}}
}
func taskSession(t *testing.T, h *harness, agent string, capture *responseCapture) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "task-test", Version: "1"}, nil)
	if err := mcp.AddSendingCustomMethod[*taskGetTestParams, *taskGetTestResult](c, "tasks/get"); err != nil {
		t.Fatal(err)
	}
	if err := mcp.AddSendingCustomMethod[*taskCancelTestParams, *taskCancelTestResult](c, "tasks/cancel"); err != nil {
		t.Fatal(err)
	}
	s, err := c.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: h.url, HTTPClient: &http.Client{Transport: captureAuth{agent, capture}}, MaxRetries: -1, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func taskIDFromResponse(t *testing.T, raw string) string {
	t.Helper()
	var envelope struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		t.Fatalf("decode wire response: %v: %s", err, raw)
	}
	if envelope.Result["resultType"] != "task" {
		t.Fatalf("response was not a task: %s", raw)
	}
	id, _ := envelope.Result["taskId"].(string)
	if id == "" {
		t.Fatalf("taskId missing: %s", raw)
	}
	return id
}
func TestTasksAreNegotiatedPerCallInBothToolModes(t *testing.T) {
	h := newHarness(t)
	h.be.mu.Lock()
	h.be.taskMode = true
	h.be.mu.Unlock()
	// Full-mode direct tool call: extension support is in this request's _meta.
	fullCapture := &responseCapture{}
	full := taskSession(t, h, "wide", fullCapture)
	_, err := full.CallTool(t.Context(), &mcp.CallToolParams{Meta: taskMeta(), Name: "desktop-pc__job_submit", Arguments: map[string]any{"command": "echo"}})
	if err != nil {
		t.Fatal(err)
	}
	taskID := taskIDFromResponse(t, fullCapture.last())
	get, err := mcp.CallCustomMethod[*taskGetTestParams, *taskGetTestResult](t.Context(), full, "tasks/get", &taskGetTestParams{ParamsBase: mcp.ParamsBase{Meta: taskMeta()}, TaskID: taskID})
	if err != nil {
		t.Fatal(err)
	}
	if get.ResultType != "complete" || get.Status != "working" || get.TaskID != taskID {
		t.Fatalf("task get = %+v", get)
	}
	unnegotiatedCapture := &responseCapture{}
	unnegotiated := taskSession(t, h, "wide", unnegotiatedCapture)
	if _, err := mcp.CallCustomMethod[*taskGetTestParams, *taskGetTestResult](t.Context(), unnegotiated, "tasks/get", &taskGetTestParams{TaskID: taskID}); err == nil {
		t.Fatal("unnegotiated Tasks method was accepted")
	}
	var rpcError struct {
		Error struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(unnegotiatedCapture.last()), &rpcError); err != nil || rpcError.Error.Code != -32021 {
		t.Fatalf("unnegotiated Tasks error = %s, %v", unnegotiatedCapture.last(), err)
	}
	outsiderCapture := &responseCapture{}
	outsider := taskSession(t, h, "outsider", outsiderCapture)
	if _, err := mcp.CallCustomMethod[*taskGetTestParams, *taskGetTestResult](t.Context(), outsider, "tasks/get", &taskGetTestParams{ParamsBase: mcp.ParamsBase{Meta: taskMeta()}, TaskID: taskID}); err == nil {
		t.Fatal("another agent retrieved the task")
	}
	canceled, err := mcp.CallCustomMethod[*taskCancelTestParams, *taskCancelTestResult](t.Context(), full, "tasks/cancel", &taskCancelTestParams{ParamsBase: mcp.ParamsBase{Meta: taskMeta()}, TaskID: taskID})
	if err != nil || canceled.ResultType != "complete" {
		t.Fatalf("task cancel = %+v, %v", canceled, err)
	}
	// Compact mesh_call is also task eligible; a call without per-request
	// negotiation remains an ordinary CallToolResult.
	h.setMode("compacter", ModeCompact)
	compactCapture := &responseCapture{}
	compact := taskSession(t, h, "compacter", compactCapture)
	_, err = compact.CallTool(t.Context(), &mcp.CallToolParams{Meta: taskMeta(), Name: "mesh_call", Arguments: map[string]any{"device": "desktop-pc", "tool": "job_submit", "arguments": map[string]any{"command": "echo"}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = taskIDFromResponse(t, compactCapture.last())
	h.setMode("normal", ModeFull)
	normalCapture := &responseCapture{}
	normal := taskSession(t, h, "normal", normalCapture)
	normalRes, err := normal.CallTool(t.Context(), &mcp.CallToolParams{Name: "desktop-pc__job_submit", Arguments: map[string]any{"command": "echo"}})
	if err != nil || normalRes == nil {
		t.Fatalf("unnegotiated call: %v", err)
	}
	var envelope struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal([]byte(normalCapture.last()), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Result["resultType"] == "task" {
		t.Fatalf("unnegotiated call returned task: %s", normalCapture.last())
	}
}
