package browser

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/approval"
	"messh/internal/provider"
	"messh/internal/state"
)

// scriptSurface answers prompts from a function and records them.
type scriptSurface struct {
	mu      sync.Mutex
	prompts []approval.Prompt
	answer  func(approval.Prompt) approval.Answer
}

func (s *scriptSurface) Name() string    { return "script" }
func (s *scriptSurface) Available() bool { return true }
func (s *scriptSurface) Ask(ctx context.Context, p approval.Prompt) (approval.Answer, error) {
	s.mu.Lock()
	s.prompts = append(s.prompts, p)
	f := s.answer
	s.mu.Unlock()
	if f == nil {
		return approval.Answer{}, nil
	}
	return f(p), nil
}

func (s *scriptSurface) asked() []approval.Prompt {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]approval.Prompt(nil), s.prompts...)
}

func (s *scriptSurface) reset() {
	s.mu.Lock()
	s.prompts = nil
	s.mu.Unlock()
}

// node mimics the node's gate with the real approval engine: Approval, then
// Decide, then Call; the provider's Asker also goes through the engine.
type node struct {
	t      *testing.T
	env    *testEnv
	eng    *approval.Engine
	surf   *scriptSurface
	caller provider.Caller
}

func newNode(t *testing.T, cfg Config) *node {
	t.Helper()
	surf := &scriptSurface{}
	n := &node{t: t, surf: surf, caller: provider.Caller{DeviceID: "dev-raspi", DeviceName: "raspi", Agent: "omp"}}
	// The provider needs the engine's Asker, the engine needs a state dir:
	// build the env first and swap the asker in.
	n.env = newEnv(t, cfg)
	eng, err := approval.New(approval.Options{Paths: state.Paths{Root: t.TempDir()}, Surface: surf, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(eng.Close)
	n.eng = eng
	n.env.p.ask = func(ctx context.Context, caller provider.Caller, tool string, ap provider.Approval) (bool, string, error) {
		dec, err := eng.Decide(ctx, approval.Request{Caller: caller, Tool: tool, Class: provider.ClassBrowser, Approval: ap})
		if err != nil {
			return false, err.Error(), nil
		}
		return dec.Allowed, dec.Reason, nil
	}
	return n
}

// call runs a tool the way the node's dispatch does.
func (n *node) call(tool string, args any) *mcp.CallToolResult {
	n.t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		n.t.Fatal(err)
	}
	p := n.env.p
	ap, err := p.Approval(context.Background(), tool, raw, n.caller)
	if err != nil {
		return provider.ErrorResult("%s refused: %v", tool, err)
	}
	dec, err := n.eng.Decide(context.Background(), approval.Request{Caller: n.caller, Tool: tool, Class: provider.ClassBrowser, Approval: ap})
	if err != nil {
		return provider.ErrorResult("approval failed: %v", err)
	}
	if !dec.Allowed {
		return provider.ErrorResult("denied: %s", dec.Reason)
	}
	res, err := p.Call(context.Background(), tool, raw, n.caller)
	if err != nil {
		return provider.ErrorResult("%v", err)
	}
	return res
}

// allowOnce answers every prompt with "allow once".
func (n *node) allowOnce() {
	n.surf.mu.Lock()
	n.surf.answer = func(approval.Prompt) approval.Answer { return approval.Answer{Allow: true} }
	n.surf.mu.Unlock()
}

// denyAll answers every prompt with "deny".
func (n *node) denyAll() {
	n.surf.mu.Lock()
	n.surf.answer = func(approval.Prompt) approval.Answer { return approval.Answer{} }
	n.surf.mu.Unlock()
}

// alwaysScope answers by saving the scope whose label contains want.
func (n *node) alwaysScope(want string) {
	n.surf.mu.Lock()
	n.surf.answer = func(p approval.Prompt) approval.Answer {
		for i, s := range p.Scopes {
			if strings.Contains(s.Label, want) {
				return approval.Answer{Allow: true, Always: true, Scope: i}
			}
		}
		n.t.Errorf("no scope containing %q in %+v", want, p.Scopes)
		return approval.Answer{}
	}
	n.surf.mu.Unlock()
}
