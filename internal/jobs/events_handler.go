package jobs

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"messh/internal/provider"
)

func (p *Provider) callEvents(ctx context.Context, raw json.RawMessage, c provider.Caller) (*mcp.CallToolResult, error) {
	var a struct {
		Cursor string `json:"cursor"`
		Limit  int    `json:"limit"`
		WaitMS int    `json:"wait_ms"`
	}
	if err := decodeArgs(raw, &a); err != nil {
		return nil, err
	}
	if a.WaitMS < 0 || a.WaitMS > 30000 {
		return nil, errors.New("wait_ms must be between 0 and 30000")
	}
	page, err := p.Events(ctx, c, a.Cursor, a.Limit, time.Duration(a.WaitMS)*time.Millisecond)
	if err != nil {
		var expired *CursorExpiredError
		if errors.As(err, &expired) {
			result, e := provider.JSONResult(struct {
				Error    string    `json:"error"`
				Code     string    `json:"code"`
				Snapshot []Summary `json:"snapshot"`
				Stale    bool      `json:"stale"`
			}{Error: err.Error(), Code: "cursor_expired", Snapshot: expired.Snapshot, Stale: page.Stale})
			if e == nil {
				result.IsError = true
			}
			return result, e
		}
		return nil, err
	}
	return provider.JSONResult(page)
}
