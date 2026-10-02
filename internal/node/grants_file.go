package node

import (
	"context"
	"messh/internal/files"
	"messh/internal/grants"
	"messh/internal/provider"
	"net/http"
	"strings"
)

type fileAgentKey struct{}

func withFileAgent(ctx context.Context, agent string) context.Context {
	return context.WithValue(ctx, fileAgentKey{}, agent)
}
func fileAgent(ctx context.Context) string { v, _ := ctx.Value(fileAgentKey{}).(string); return v }
func (n *Node) fileGrantDecision(deviceID, agent, ref, action string) grants.Decision {
	if deviceID == n.id.ID && agent == cliAgent {
		return grants.Decision{Allowed: true, Code: "owner"}
	}
	return n.grants.Decide(grants.Subject{DeviceID: deviceID, Agent: agent}, grants.Request{Kind: "file", Path: ref, Action: action})
}
func (n *Node) fileGrantVisible(deviceID, agent, requested, entry, action string) bool {
	if deviceID == n.id.ID && agent == cliAgent {
		return true
	}
	return n.grants.CanSee(grants.Subject{DeviceID: deviceID, Agent: agent}, requested, entry, action)
}
func (n *Node) fileProviderDecision(c provider.Caller, ref, action string) grants.Decision {
	return n.fileGrantDecision(c.DeviceID, c.Agent, ref, action)
}
func (n *Node) fileHTTPDecision(r *http.Request, ref, action string) (bool, string) {
	d := n.fileGrantDecision(r.Header.Get(hdrPeerID), r.Header.Get(files.HeaderAgent), ref, action)
	return d.Allowed, d.Code
}
func (n *Node) authorizeLocalCopy(agent, ref, action string) error {
	ref = strings.TrimSuffix(ref, "/")
	if ref == "" {
		return grants.ErrInvalid
	}
	if _, err := files.ParseRef(ref); err != nil {
		return err
	}
	d := n.fileGrantDecision(n.id.ID, agent, ref, action)
	if !d.Allowed {
		return &files.CapabilityDeniedError{Code: d.Code}
	}
	return nil
}
