package node

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/approval"
	"messh/internal/control"
	"messh/internal/grants"
	"messh/internal/provider"
)

// newApprovalEngine builds the node's approval engine on the surface the
// caller injected (tests) or the platform's default.
func newApprovalEngine(opts Options, log *slog.Logger) (*approval.Engine, error) {
	surface := opts.ApprovalSurface
	if surface == nil {
		surface = approval.Default()
	}
	return approval.New(approval.Options{
		Paths:   opts.Paths,
		Surface: surface,
		Log:     log.With("component", "approval"),
	})
}

// registerApprovalAPI adds the approval endpoints to the control API. They sit
// behind requireControl: only the local user's CLI holds the control token,
// so neither a remote peer nor an agent token can answer a prompt.
func (n *Node) registerApprovalAPI(api *http.ServeMux) {
	api.HandleFunc("GET /v1/approvals", n.apiApprovals)
	api.HandleFunc("POST /v1/approvals/test", n.apiApprovalTest)
	api.HandleFunc("POST /v1/approvals/{id}", n.apiApprovalAnswer)
	api.HandleFunc("GET /v1/rules", n.apiRules)
	api.HandleFunc("DELETE /v1/rules/{id}", n.apiRuleRemove)
	api.HandleFunc("GET /v1/audit", n.apiAudit)
}

// mountRespond serves the notification buttons' endpoint. It is deliberately
// outside requireControl: the handler process behind a Windows toast button
// has no control token. It is authenticated by the request's own one-time
// nonce instead (approval.Engine.Activate) and can do nothing but answer
// that request. It also tells prompts which port to call back on.
func (n *Node) mountRespond(mux *http.ServeMux) {
	if a, ok := n.localLn.Addr().(*net.TCPAddr); ok && a.IP.To4() != nil { // the helper dials 127.0.0.1
		n.approvals.SetPort(a.Port)
	}
	mux.HandleFunc("/v1/approvals/{id}/respond", n.apiRespond)
}

func (n *Node) apiRespond(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "POST only")
		return
	}
	// Browsers always send Origin on cross-site POSTs; the toast helper never does.
	if r.Header.Get("Origin") != "" {
		writeError(w, http.StatusForbidden, "not available to web pages")
		return
	}
	if host, _, err := net.SplitHostPort(r.Host); err != nil || !isLoopbackHost(host) {
		writeError(w, http.StatusForbidden, "loopback only")
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}
	var in approval.RespondBody
	dec := json.NewDecoder(io.LimitReader(r.Body, 2<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	switch err := n.approvals.Activate(r.PathValue("id"), in.Nonce, in.Decision); {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, approval.ErrNotPending):
		writeError(w, http.StatusNotFound, "this approval request is no longer waiting for an answer")
	case errors.Is(err, approval.ErrBadNonce):
		writeError(w, http.StatusForbidden, err.Error())
	case errors.Is(err, approval.ErrNonceUsed), errors.Is(err, approval.ErrNotShown):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, approval.ErrBadAction):
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		n.log.Warn("approval response failed", "error", err)
		writeError(w, http.StatusInternalServerError, "could not apply the response")
	}
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// apiApprovalTest shows a synthetic request through the real surface and
// answers with what the person chose. Closing the CLI withdraws it.
func (n *Node) apiApprovalTest(w http.ResponseWriter, r *http.Request) {
	d, err := n.approvals.Test(r.Context(), n.name)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "test request failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, control.ApprovalTestResult{
		ID: d.ID, Outcome: d.Outcome, Allowed: d.Allowed, Scope: d.Scope, ScopeLabel: d.ScopeLabel,
		Surface: d.Surface, Via: n.approvals.SurfaceName(), Reason: d.Reason,
	})
}

func (n *Node) apiApprovals(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, n.approvals.Pending())
}

func (n *Node) apiApprovalAnswer(w http.ResponseWriter, r *http.Request) {
	var in control.ApprovalAnswer
	if !readJSON(w, r, &in) {
		return
	}
	var ans approval.Answer
	switch in.Decision {
	case control.DecisionAllow:
		ans = approval.Answer{Allow: true}
	case control.DecisionAlways:
		ans = approval.Answer{Allow: true, Always: true, Scope: in.Scope}
	case control.DecisionDeny:
	default:
		writeError(w, http.StatusBadRequest, `decision must be "allow", "always" or "deny"`)
		return
	}
	switch err := n.approvals.Resolve(r.PathValue("id"), ans); {
	case errors.Is(err, approval.ErrNotPending):
		writeError(w, http.StatusNotFound, err.Error())
	case err != nil:
		writeError(w, http.StatusBadRequest, err.Error())
	default:
		writeJSON(w, http.StatusOK, in)
	}
}

func (n *Node) apiRules(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, n.approvals.Rules())
}

func (n *Node) apiRuleRemove(w http.ResponseWriter, r *http.Request) {
	switch err := n.approvals.RemoveRule(r.PathValue("id")); {
	case errors.Is(err, approval.ErrNoRule):
		writeError(w, http.StatusNotFound, err.Error())
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (n *Node) apiAudit(w http.ResponseWriter, r *http.Request) {
	count := 50
	if s := r.URL.Query().Get("n"); s != "" {
		v, err := strconv.Atoi(s)
		if err != nil || v < 1 {
			writeError(w, http.StatusBadRequest, "n must be a positive number")
			return
		}
		count = v
	}
	recs, err := n.approvals.Audit(count)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if recs == nil {
		recs = []approval.AuditRecord{}
	}
	writeJSON(w, http.StatusOK, recs)
}

// gateState carries one gated call from approval to completion.
type gateState struct {
	n      *Node
	req    approval.Request
	dec    approval.Decision // set for calls decided before they ran
	ticket provider.Ticket   // set for deferred calls, which the provider waits on itself
}

// gate asks the provider to describe a call and gets it approved. It
// returns the context to call the provider with, or a result that refuses the
// call (denied, expired, cancelled, or not gateable). Nothing is prompted for
// a call whose description fails.
func (n *Node) gate(ctx context.Context, lt localTool, name string, args json.RawMessage, caller provider.Caller) (context.Context, *gateState, *mcp.CallToolResult) {
	gated, ok := lt.provider.(provider.Gated)
	if !ok {
		return ctx, nil, provider.ErrorResult("%s is a %q tool but its provider has no approval gate; refused on %s", name, lt.tool.Class, n.name)
	}
	ap, err := gated.Approval(ctx, name, args, caller)
	if err != nil {
		return ctx, nil, provider.ErrorFrom(err, name+" refused on "+n.name)
	}
	// Authorization never skips native request preparation or launch validation.
	if n.trust.Allows(caller.DeviceID) {
		ap.Auto = "trusted device"
	} else {
		decision := n.grants.Decide(grants.Subject{DeviceID: caller.DeviceID, Agent: caller.Agent},
			grants.Request{Kind: "tool", Tool: name, ArgsHash: ap.Exact})
		if decision.Allowed {
			ap.Auto = "capability grant " + decision.GrantID
		}
	}
	g := &gateState{n: n, req: approval.Request{Caller: caller, Tool: name, Class: lt.tool.Class, Approval: ap}}
	if ap.Deferred {
		g.ticket = n.approvals.Submit(g.req)
		return provider.WithTicket(ctx, g.ticket), g, nil
	}
	g.dec, err = n.approvals.Decide(ctx, g.req)
	switch {
	case err != nil:
		return ctx, nil, provider.ErrorResult("approval for %s failed on %s: %v", name, n.name, err)
	case !g.dec.Allowed:
		return ctx, nil, provider.ErrorResult("denied on %s: %s", n.name, g.dec.Reason)
	}
	return ctx, g, nil
}

// finish records how an allowed call ended. A deferred call whose provider
// gave up without using the ticket withdraws it, closing the prompt.
func (g *gateState) finish(res *mcp.CallToolResult, err error, took time.Duration) {
	if g == nil {
		return
	}
	failed := err != nil || (res != nil && res.IsError)
	if g.ticket != nil {
		if failed {
			g.ticket.Cancel()
		}
		return
	}
	var msg string
	switch {
	case err != nil:
		msg = err.Error()
	case failed:
		msg = "tool reported an error"
		for _, c := range res.Content {
			if t, ok := c.(*mcp.TextContent); ok {
				msg = t.Text
				break
			}
		}
	}
	if len(msg) > 300 {
		msg = msg[:300] + "..."
	}
	g.n.approvals.Complete(g.dec, g.req, !failed, msg, took)
}
