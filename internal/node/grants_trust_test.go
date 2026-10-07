package node

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"messh/internal/files"
	"messh/internal/provider"
	"messh/internal/trust"
)

func trustedFileNode(t *testing.T) (*Node, string, string) {
	t.Helper()
	n := startFileNode(t, "trusted-files")
	store, err := trust.New(t.TempDir() + "/trust.json")
	if err != nil {
		t.Fatal(err)
	}
	n.trust = store
	if _, err := store.Add("device-trusted", "trusted"); err != nil {
		t.Fatal(err)
	}
	return n, "device-trusted", "device-other"
}

func TestTrustedFileAuthorizationAcrossAgentsAndRevocation(t *testing.T) {
	n, trusted, other := trustedFileNode(t)
	for _, agent := range []string{"agent-one", "agent-two"} {
		got := n.fileGrantDecision(trusted, agent, "ws/project/file.txt", "read")
		if !got.Allowed || got.Code != "trusted_device" {
			t.Errorf("trusted device agent %q: got %+v", agent, got)
		}
	}
	if got := n.fileGrantDecision(other, "agent-one", "ws/project/file.txt", "read"); got.Allowed {
		t.Fatalf("untrusted device was allowed: %+v", got)
	}
	for _, tc := range []struct{ ref, action string }{
		{"", "read"}, {"ws/../outside", "read"}, {"ws/bad\\name", "read"}, {"ws/project/file.txt", ""}, {"ws/project/file.txt", "delete"},
	} {
		if got := n.fileGrantDecision(trusted, "agent-one", tc.ref, tc.action); got.Allowed {
			t.Errorf("invalid request (%q, %q) was allowed: %+v", tc.ref, tc.action, got)
		}
	}
	if got := n.fileGrantDecision(trusted, "agent-one", "ws/project/../project/file.txt", "read"); !got.Allowed {
		t.Errorf("ParseRef-equivalent valid path denied: %+v", got)
	}
	if got := n.fileGrantDecision(trusted, "agent-one", "artifacts/audio/out.wav", "read"); !got.Allowed || got.Code != "trusted_device" {
		t.Errorf("artifact read denied: %+v", got)
	}
	if got := n.fileGrantDecision(trusted, "agent-one", "artifacts/audio/out.wav", "write"); got.Allowed {
		t.Errorf("artifact write allowed: %+v", got)
	}
	if got := n.fileGrantDecision(trusted, "agent-one", "ws/project/file.txt", "write"); !got.Allowed {
		t.Errorf("workspace write denied: %+v", got)
	}
	if err := n.trust.Remove(trusted); err != nil {
		t.Fatal(err)
	}
	if got := n.fileGrantDecision(trusted, "agent-one", "ws/project/file.txt", "read"); got.Allowed {
		t.Fatalf("revoked trust remained authorized: %+v", got)
	}
}

func TestTrustedFileListingVisibilityKeepsBoundaries(t *testing.T) {
	n, trusted, _ := trustedFileNode(t)
	for _, entry := range []string{"ws", "artifacts", "ws/project", "ws/project/file.txt", "artifacts/audio/out.wav"} {
		if !n.fileGrantVisible(trusted, "any-agent", "", entry, "read") {
			t.Errorf("trusted device cannot list root entry %q", entry)
		}
	}
	if !n.fileGrantVisible(trusted, "any-agent", "ws/project", "ws/project/child", "read") {
		t.Fatal("trusted device cannot see allowed workspace child")
	}
	if !n.fileGrantVisible(trusted, "any-agent", "ws/project/../project", "ws/project/child/../child/file", "read") {
		t.Fatal("ParseRef-equivalent listing paths were hidden")
	}
	if n.fileGrantVisible(trusted, "any-agent", "ws/project", "ws/other/file", "read") {
		t.Fatal("listing escaped requested workspace directory")
	}
	if n.fileGrantVisible(trusted, "any-agent", "ws", "ws/../outside", "read") || n.fileGrantVisible(trusted, "any-agent", "ws", "ws/file", "execute") || n.fileGrantVisible(trusted, "any-agent", "ws", "ws/bad\\name", "read") {
		t.Fatal("invalid entry or action became visible")
	}
	if n.fileGrantVisible(trusted, "any-agent", "artifacts", "artifacts/audio/out.wav", "write") {
		t.Fatal("artifact write became visible")
	}
	if n.fileGrantVisible("device-other", "other-agent", "ws/project", "ws/project/child", "read") {
		t.Fatal("untrusted device can see entries")
	}
}

func TestTrustedFilesListShowsRealStoreDirectories(t *testing.T) {
	n, trusted, _ := trustedFileNode(t)
	if err := n.files.store.Mkdir("ws/transfer"); err != nil {
		t.Fatal(err)
	}
	listing, err := n.files.store.List("ws")
	if err != nil {
		t.Fatal(err)
	}
	var hasTransfer bool
	for _, entry := range listing.Entries {
		if entry.Ref == "ws/transfer" && entry.IsDir {
			hasTransfer = true
			if !n.fileGrantVisible(trusted, "agent", "ws", entry.Ref, "read") {
				t.Fatalf("store directory metadata was hidden: %+v", entry)
			}
		}
	}
	if !hasTransfer {
		t.Fatalf("Store.List(ws) did not return canonical directory metadata: %+v", listing.Entries)
	}
	lt, ok := n.lookupTool("files_list")
	if !ok {
		t.Fatal("files_list tool not registered")
	}
	result, err := lt.provider.Call(t.Context(), "files_list", json.RawMessage(`{"ref":"ws"}`), provider.Caller{DeviceID: trusted, Agent: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Entries []files.Info `json:"entries"`
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 1 || got.Entries[0].Ref != "ws/transfer" || !got.Entries[0].IsDir {
		t.Fatalf("trusted files_list result = %+v", got.Entries)
	}
}

type trustCheckProvider struct{}

func (trustCheckProvider) Name() string { return "trust-check-test" }
func (trustCheckProvider) Tools() []provider.Tool {
	return []provider.Tool{{Class: provider.ClassExec, Def: &mcp.Tool{Name: "trust_check_test"}}}
}
func (trustCheckProvider) Call(context.Context, string, json.RawMessage, provider.Caller) (*mcp.CallToolResult, error) {
	return nil, nil
}
func (trustCheckProvider) Approval(context.Context, string, json.RawMessage, provider.Caller) (provider.Approval, error) {
	return provider.Approval{Exact: "prepared-exact-hash"}, nil
}

func TestCapabilityCheckReportsTrustedFileAndPreparedToolAuthorization(t *testing.T) {
	n, trusted, _ := trustedFileNode(t)
	pr := trustCheckProvider{}
	n.toolsMu.Lock()
	n.tools["trust_check_test"] = localTool{provider: pr, tool: pr.Tools()[0]}
	n.toolsMu.Unlock()
	cp := &capabilityProvider{store: n.grants, node: n}
	caller := provider.Caller{DeviceID: trusted, Agent: "two-agents"}
	for _, tc := range []struct {
		kind  string
		input string
	}{
		{"file", `{"kind":"file","path":"ws/project/data.txt","action":"read"}`},
		{"tool", `{"kind":"tool","tool":"trust_check_test","args":{"x":1}}`},
	} {
		result, err := cp.Call(t.Context(), "capability_check", json.RawMessage(tc.input), caller)
		if err != nil {
			t.Fatal(err)
		}
		var got capabilityCheckResult
		encoded, err := json.Marshal(result.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(encoded, &got); err != nil {
			t.Fatal(err)
		}
		if !got.Decision.Allowed || got.Decision.Code != "trusted_device" {
			t.Errorf("%s capability check: %+v", tc.kind, got.Decision)
		}
		if tc.kind == "tool" && got.ArgsHash != "prepared-exact-hash" {
			t.Errorf("native check hash %q", got.ArgsHash)
		}
	}
	list, err := cp.Call(t.Context(), "capability_list", json.RawMessage(`{}`), caller)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(list.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != "[]" {
		t.Fatalf("capability_list included trust policy or unexpected grants: %s", list.StructuredContent)
	}
}
