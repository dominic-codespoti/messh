package browser

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// The client must not advertise roots: Playwright MCP would then ask for them
// on the first tool call and stall for 60 s waiting for an answer.
func TestClientDoesNotAdvertiseRoots(t *testing.T) {
	n := newNode(t, baseConfig())
	n.env.fb.addSite("http://a.test/", "A")
	n.allowOnce()
	n.call("browser_navigate", map[string]any{"url": "http://a.test/"})
	fb := n.env.fb
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if len(fb.calls) == 0 {
		t.Fatal("no call reached the fake")
	}
	if fb.rootsAdvertised {
		t.Error("the upstream client advertised the roots capability")
	}
}

// A slow start must not hold a call longer than an agent's MCP client waits:
// the call returns an explanation, the start carries on, and the next call
// gets the browser.
func TestSlowStartDoesNotBlockTheCall(t *testing.T) {
	n := newNode(t, baseConfig())
	n.env.fb.addSite("http://a.test/", "A")
	up := n.env.p.up
	up.timeout.startWait = 150 * time.Millisecond
	release := make(chan struct{})
	var once sync.Once
	fast := up.launch
	up.launch = func(ctx context.Context, cfg Config) (*session, error) {
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return fast(ctx, cfg)
	}
	n.allowOnce()
	t0 := time.Now()
	res := n.call("browser_navigate", map[string]any{"url": "http://a.test/"})
	if took := time.Since(t0); took > 2*time.Second {
		t.Errorf("the call blocked for %s", took)
	}
	if !isErr(res) || !strings.Contains(resultText(res), "still starting") || !strings.Contains(resultText(res), "retry") {
		t.Errorf("slow start result: %s", resultText(res))
	}
	if st := n.env.p.Status(context.Background()); !st.Starting || st.Running {
		t.Errorf("status during the start: %+v", st)
	}
	once.Do(func() { close(release) })
	deadline := time.Now().Add(3 * time.Second)
	for n.env.p.up.running() == nil && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	res = n.call("browser_navigate", map[string]any{"url": "http://a.test/"})
	if isErr(res) {
		t.Fatalf("after the start finished: %s", resultText(res))
	}
	if starts, _ := n.env.launch.counts(); starts != 1 {
		t.Errorf("the background start was repeated: %d starts", starts)
	}
}
