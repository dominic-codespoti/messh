package browser

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestLiveFirstCall starts the real Playwright MCP (npx) and a real headless
// browser and requires the first navigation, which includes starting both, to
// finish well inside an agent's MCP timeout. It is skipped unless
// MESSH_BROWSER_LIVE=1 because it needs Node, network access for npm, and
// Chrome or Edge.
//
// It guards a real regression: the Go MCP client advertises the roots
// capability by default, Playwright MCP then asks the client for its roots on
// the first tool call and waits 60 s for an answer that cannot arrive (the
// request would travel on the standalone stream, which the client disables).
func TestLiveFirstCall(t *testing.T) {
	if os.Getenv("MESSH_BROWSER_LIVE") != "1" {
		t.Skip("set MESSH_BROWSER_LIVE=1 to run against the real Playwright MCP")
	}
	cfg := Config{Enabled: true, Headless: true, Channel: os.Getenv("MESSH_BROWSER_CHANNEL")}
	e := newEnv(t, cfg)
	e.p.up.launch = e.p.up.launchProcess
	for _, name := range []string{"first", "second"} {
		start := time.Now()
		res, err := e.p.up.call(context.Background(), cfg, "browser_navigate", mustJSON(map[string]any{"url": "about:blank"}))
		if err != nil {
			t.Fatal(err)
		}
		took := time.Since(start)
		t.Logf("%s navigate: %s (%s)", name, took.Round(time.Millisecond), clip(textOf(res), 60))
		if took > 10*time.Second {
			t.Fatalf("%s navigate took %s; the first call must not stall", name, took)
		}
	}
}
