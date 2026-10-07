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
	if n.trust.Allows(deviceID) {
		parsed, err := files.ParseRef(ref)
		if err != nil || (action != "read" && action != "write") {
			return grants.Decision{Code: "invalid_request"}
		}
		if parsed.Root() == files.RootArtifacts && action != "read" {
			return grants.Decision{Code: "read_only_artifact"}
		}
		return grants.Decision{Allowed: true, Code: "trusted_device"}
	}
	return n.grants.Decide(grants.Subject{DeviceID: deviceID, Agent: agent}, grants.Request{Kind: "file", Path: ref, Action: action})
}
func (n *Node) fileGrantVisible(deviceID, agent, requested, entry, action string) bool {
	if deviceID == n.id.ID && agent == cliAgent {
		return true
	}
	if n.trust.Allows(deviceID) {
		if action != "read" && action != "write" {
			return false
		}
		entryRef, err := files.ParseRef(entry)
		if err != nil || (entryRef.Root() == files.RootArtifacts && action != "read") {
			return false
		}
		if requested == "" {
			return true
		}
		requestedRef, err := files.ParseRef(requested)
		return err == nil && refWithin(requestedRef, entryRef)
	}
	return n.grants.CanSee(grants.Subject{DeviceID: deviceID, Agent: agent}, requested, entry, action)
}

func refWithin(parent, child files.Ref) bool {
	p, c := string(parent), string(child)
	return c == p || len(c) > len(p) && strings.HasPrefix(c, p) && c[len(p)] == '/'
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
