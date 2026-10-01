package node

import (
	"context"
	"net/http"

	"messh/internal/approval"
	"messh/internal/browser"
	"messh/internal/provider"
)

// browserControl is what the control API needs from the browser provider.
type browserControl interface {
	Status(ctx context.Context) browser.Status
	Stop() bool
}

// startBrowser registers the browser provider: tools that let an agent drive a
// web browser on this device, approved per website. It publishes nothing until
// the owner enables it in browser.json. The browser is started on the first
// call and stopped (with everything it spawned) when the node stops. A
// failure only costs the browser tools, never the node.
func (n *Node) startBrowser() {
	b, err := browser.New(n.ctx, browser.Options{
		Paths: n.paths,
		Log:   n.log.With("component", "browser"),
		Ask:   n.askBrowser,
	})
	if err != nil {
		n.log.Warn("browser unavailable", "error", err)
		return
	}
	n.browser = b
	n.register(b)
	// shutdown waits for goRun goroutines after the servers stop, so the
	// browser and its child processes are gone by the time Done closes.
	n.goRun(func() {
		<-n.ctx.Done()
		b.Close()
	})
}

// askBrowser is the approval a browser call needs mid-call, when the page
// ended up on a site the first approval did not cover. It goes through the
// same engine as the gate: saved rules, the prompt surface, the audit log.
func (n *Node) askBrowser(ctx context.Context, caller provider.Caller, tool string, ap provider.Approval) (bool, string, error) {
	dec, err := n.approvals.Decide(ctx, approval.Request{Caller: caller, Tool: tool, Class: provider.ClassBrowser, Approval: ap})
	if err != nil {
		return false, err.Error(), nil
	}
	return dec.Allowed, dec.Reason, nil
}

// registerBrowserAPI adds the browser endpoints to the control API (behind
// requireControl, like the approval endpoints).
func (n *Node) registerBrowserAPI(api *http.ServeMux) {
	api.HandleFunc("GET /v1/browser", n.apiBrowserStatus)
	api.HandleFunc("POST /v1/browser/stop", n.apiBrowserStop)
}

func (n *Node) apiBrowserStatus(w http.ResponseWriter, r *http.Request) {
	if n.browser == nil {
		writeJSON(w, http.StatusOK, browser.Status{})
		return
	}
	writeJSON(w, http.StatusOK, n.browser.Status(r.Context()))
}

// BrowserStopped is the answer to POST /v1/browser/stop.
type BrowserStopped struct {
	Stopped bool `json:"stopped"`
}

func (n *Node) apiBrowserStop(w http.ResponseWriter, _ *http.Request) {
	stopped := false
	if n.browser != nil {
		stopped = n.browser.Stop()
	}
	writeJSON(w, http.StatusOK, BrowserStopped{Stopped: stopped})
}
