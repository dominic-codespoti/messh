package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/provider"
)

type openaiArgs struct {
	Service     string        `json:"service"`
	Model       string        `json:"model"`
	Messages    []chatMessage `json:"messages"`
	Temperature *float64      `json:"temperature"`
	MaxTokens   *int          `json:"max_tokens"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// planOpenAI parses openai_models and openai_chat.
func (p *Provider) planOpenAI(tool string, args json.RawMessage) (*plan, error) {
	var a openaiArgs
	if err := decodeArgs(args, &a); err != nil {
		return nil, err
	}
	e, err := p.lookup(a.Service)
	if err != nil {
		return nil, err
	}
	if e.cfg.Kind != KindOpenAI {
		return nil, fmt.Errorf("service %s is kind %s, not openai", e.cfg.Name, e.cfg.Kind)
	}
	pl := &plan{p: p, e: e, svc: e.cfg, tool: tool, op: tool}
	if tool == toolModels {
		if a.Model != "" || len(a.Messages) > 0 {
			return nil, fmt.Errorf("openai_models takes only service")
		}
		pl.readOnly = true
		pl.exact = map[string]any{}
		pl.autoMatch = func(entry string) bool { return entry == toolModels || strings.EqualFold(entry, "GET /v1/models") }
		pl.run = func(ctx context.Context) (*mcp.CallToolResult, error) { return p.runModels(ctx, e) }
		return pl, nil
	}
	if a.Model == "" || len(a.Messages) == 0 {
		return nil, fmt.Errorf("openai_chat needs model and messages")
	}
	for i, m := range a.Messages {
		if m.Role != "system" && m.Role != "user" && m.Role != "assistant" {
			return nil, fmt.Errorf("messages[%d].role must be system, user or assistant", i)
		}
	}
	last := a.Messages[len(a.Messages)-1]
	pl.details = []provider.Detail{
		{Label: "Model", Value: clip(a.Model, 80)},
		{Label: "Messages", Value: fmt.Sprintf("%d (last %s: %s)", len(a.Messages), last.Role, clip(strings.ReplaceAll(last.Content, "\n", " "), 160))},
	}
	if a.MaxTokens != nil {
		pl.details = append(pl.details, provider.Detail{Label: "max_tokens", Value: fmt.Sprint(*a.MaxTokens)})
	}
	canonMsgs := make([]any, len(a.Messages))
	for i, m := range a.Messages {
		canonMsgs[i] = map[string]any{"role": m.Role, "content": m.Content}
	}
	pl.exact = map[string]any{"model": a.Model, "messages": canonMsgs}
	if a.Temperature != nil {
		pl.exact["temperature"] = *a.Temperature
	}
	if a.MaxTokens != nil {
		pl.exact["max_tokens"] = *a.MaxTokens
	}
	pl.autoMatch = func(entry string) bool {
		return entry == toolChat || strings.EqualFold(entry, "POST /v1/chat/completions")
	}
	pl.run = func(ctx context.Context) (*mcp.CallToolResult, error) { return p.runChat(ctx, e, a) }
	return pl, nil
}

// doJSON performs a JSON request against an openai-kind service and returns
// the status and a bounded body.
func (e *entry) doJSON(ctx context.Context, method, url string, body any, limit int64) (int, []byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.http.Do(req)
	if err != nil {
		e.requestProbe()
		return 0, nil, unwrapURLError(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return 0, nil, err
	}
	if int64(len(data)) > limit {
		return resp.StatusCode, nil, fmt.Errorf("response over %d bytes", limit)
	}
	return resp.StatusCode, data, nil
}

// upstreamMessage extracts an OpenAI-style error message, if any.
func upstreamMessage(data []byte) string {
	var env struct {
		Error any `json:"error"`
	}
	if json.Unmarshal(data, &env) == nil {
		switch t := env.Error.(type) {
		case string:
			return t
		case map[string]any:
			if m, ok := t["message"].(string); ok {
				return m
			}
		}
	}
	return clip(strings.TrimSpace(string(data)), 300)
}

func (p *Provider) runModels(ctx context.Context, e *entry) (*mcp.CallToolResult, error) {
	secrets := e.secret.secrets()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	status, data, err := e.doJSON(ctx, http.MethodGet, e.cfg.apiBase()+"/v1/models", nil, 4<<20)
	if err != nil {
		return provider.ErrorResult("service %s: %s", e.cfg.Name, redactString(describeErr(err), secrets)), nil
	}
	if status >= 400 {
		return provider.ErrorResult("service %s: HTTP %d: %s", e.cfg.Name, status, redactString(upstreamMessage(data), secrets)), nil
	}
	var env struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return provider.ErrorResult("service %s: unexpected /v1/models reply", e.cfg.Name), nil
	}
	type model struct {
		ID      string `json:"id"`
		OwnedBy string `json:"owned_by,omitempty"`
	}
	models := make([]model, 0, len(env.Data))
	for _, m := range env.Data {
		models = append(models, model{ID: m.ID, OwnedBy: m.OwnedBy})
	}
	res, err := provider.JSONResult(map[string]any{"service": e.cfg.Name, "models": models})
	if err != nil {
		return nil, err
	}
	return redactResult(res, secrets), nil
}

func (p *Provider) runChat(ctx context.Context, e *entry, a openaiArgs) (*mcp.CallToolResult, error) {
	secrets := e.secret.secrets()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	req := map[string]any{"model": a.Model, "messages": a.Messages, "stream": false}
	if a.Temperature != nil {
		req["temperature"] = *a.Temperature
	}
	if a.MaxTokens != nil {
		req["max_tokens"] = *a.MaxTokens
	}
	status, data, err := e.doJSON(ctx, http.MethodPost, e.cfg.apiBase()+"/v1/chat/completions", req, 16<<20)
	if err != nil {
		return provider.ErrorResult("service %s: %s", e.cfg.Name, redactString(describeErr(err), secrets)), nil
	}
	if status >= 400 {
		return provider.ErrorResult("service %s: HTTP %d: %s", e.cfg.Name, status, redactString(upstreamMessage(data), secrets)), nil
	}
	var env struct {
		Model   string `json:"model"`
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(data, &env); err != nil || len(env.Choices) == 0 {
		return provider.ErrorResult("service %s: unexpected chat reply", e.cfg.Name), nil
	}
	text, _ := env.Choices[0].Message.Content.(string)
	out := map[string]any{"service": e.cfg.Name, "model": env.Model, "text": limitText(text), "finish_reason": env.Choices[0].FinishReason}
	if len(env.Usage) > 0 && string(env.Usage) != "null" {
		out["usage"] = env.Usage
	}
	res, err := provider.JSONResult(out)
	if err != nil {
		return nil, err
	}
	return redactResult(res, secrets), nil
}
