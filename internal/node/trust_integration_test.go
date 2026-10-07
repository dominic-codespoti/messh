package node

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/approval"
	"messh/internal/state"
)

func startTrustIntegrationNode(t *testing.T, name string, surface *scriptSurface) *Node {
	t.Helper()
	n, err := Start(t.Context(), Options{
		Paths:           state.Paths{Root: t.TempDir()},
		Name:            name,
		MeshAddr:        "127.0.0.1:0",
		LocalAddr:       "127.0.0.1:0",
		Logger:          slog.New(slog.DiscardHandler),
		ApprovalSurface: surface,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	return n
}

func trustAgentSession(t *testing.T, n *Node, token string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "trust-integration-agent", Version: "1"}, nil)
	s, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{
		Endpoint:             "http://" + n.LocalAddr() + "/mcp",
		HTTPClient:           &http.Client{Transport: authHeader("Bearer " + token)},
		MaxRetries:           -1,
		DisableStandaloneSSE: true,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func trustAPICall(t *testing.T, n *Node, token, method, path string, body any) int {
	t.Helper()
	var payload *bytes.Reader
	if body == nil {
		payload = bytes.NewReader(nil)
	} else {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, "http://"+n.LocalAddr()+path, payload)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func registerTrustTestAgent(t *testing.T, n *Node, name string) string {
	t.Helper()
	token, err := n.paths.AddAgent(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.paths.SetAgentMode(name, state.ToolsFull); err != nil {
		t.Fatal(err)
	}
	return token
}

type trustTestJob struct {
	JobID string `json:"job_id"`
	State string `json:"state"`
}

func submitTrustTestJob(t *testing.T, s *mcp.ClientSession, marker string) trustTestJob {
	t.Helper()
	res := callTool(t, s, "desktop__job_submit", map[string]any{
		"command": "echo " + marker,
		"shell":   true,
	})
	if res.IsError {
		t.Fatalf("job_submit(%s): %s", marker, resultText(res))
	}
	var job trustTestJob
	if err := json.Unmarshal([]byte(resultText(res)), &job); err != nil || job.JobID == "" {
		t.Fatalf("job_submit(%s) returned %q: %v", marker, resultText(res), err)
	}
	return job
}

func waitTrustTestJob(t *testing.T, s *mcp.ClientSession, jobID string) string {
	t.Helper()
	// Submission is durable before the outbox has delivered it to the target.
	// Wait until the target's real status is terminal before using job_wait,
	// which is only available after the target has accepted the job.
	waitFor(t, "native job completion", func() bool {
		res := callTool(t, s, "desktop__job_status", map[string]any{"job_id": jobID})
		if res.IsError {
			return false
		}
		var status struct {
			State string `json:"state"`
		}
		if err := json.Unmarshal([]byte(resultText(res)), &status); err != nil {
			return false
		}
		return status.State == "succeeded"
	})
	res := callTool(t, s, "desktop__job_wait", map[string]any{"job_id": jobID, "timeout_seconds": 30})
	if res.IsError {
		t.Fatalf("job_wait(%s): %s", jobID, resultText(res))
	}
	return resultText(res)
}

func waitTrustTestApproval(t *testing.T, host *Node, s *mcp.ClientSession, jobID string) {
	t.Helper()
	waitFor(t, "job awaiting approval", func() bool {
		res := callTool(t, s, "desktop__job_status", map[string]any{"job_id": jobID})
		return !res.IsError && strings.Contains(resultText(res), "\"awaiting_approval\"")
	})
	waitFor(t, "native job approval prompt", func() bool { return len(host.approvals.Pending()) == 1 })
	pending := host.approvals.Pending()
	if err := host.approvals.Resolve(pending[0].ID, approval.Answer{Allow: true}); err != nil {
		t.Fatal(err)
	}
}

func TestNativeTrustIntegrationAcrossAgentsAndRevocation(t *testing.T) {
	host := startTrustIntegrationNode(t, "desktop", &scriptSurface{})
	trusted := startTrustIntegrationNode(t, "trusted-device", &scriptSurface{})
	other := startTrustIntegrationNode(t, "other-device", &scriptSurface{})
	pair(t, host, trusted)
	pair(t, host, other)

	alphaToken := registerTrustTestAgent(t, trusted, "alpha")
	betaToken := registerTrustTestAgent(t, trusted, "beta")
	otherToken := registerTrustTestAgent(t, other, "gamma")
	ownerOnlyProbe := registerTrustTestAgent(t, host, "agent-probe")
	alpha := trustAgentSession(t, trusted, alphaToken)
	beta := trustAgentSession(t, trusted, betaToken)
	gamma := trustAgentSession(t, other, otherToken)
	waitForTool(t, alpha, "desktop__job_submit")
	waitForTool(t, beta, "desktop__job_submit")
	waitForTool(t, gamma, "desktop__job_submit")

	if status := trustAPICall(t, host, ownerOnlyProbe, http.MethodPost, "/v1/trust", map[string]string{"device": trusted.ID()}); status != http.StatusUnauthorized {
		t.Fatalf("agent bearer token changed trust policy: POST /v1/trust status=%d, want 401", status)
	}
	if status := trustAPICall(t, host, host.controlToken, http.MethodPost, "/v1/trust", map[string]string{"device": "unpaired-device-id"}); status < 400 {
		t.Fatalf("unpaired device was trusted: POST /v1/trust status=%d", status)
	}
	if host.trust.Allows("unpaired-device-id") {
		t.Fatal("failed trust addition authorized an unpaired ID")
	}
	if status := trustAPICall(t, host, host.controlToken, http.MethodPost, "/v1/trust", map[string]string{"device": trusted.ID()}); status != http.StatusCreated {
		t.Fatalf("owner trust addition status=%d, want 201", status)
	}

	alphaJob := submitTrustTestJob(t, alpha, "trusted-alpha")
	if result := waitTrustTestJob(t, alpha, alphaJob.JobID); !strings.Contains(result, "\"succeeded\"") || !strings.Contains(result, "trusted-alpha") {
		t.Fatalf("trusted alpha job did not succeed: %s", result)
	}
	betaJob := submitTrustTestJob(t, beta, "trusted-beta")
	if result := waitTrustTestJob(t, beta, betaJob.JobID); !strings.Contains(result, "\"succeeded\"") || !strings.Contains(result, "trusted-beta") {
		t.Fatalf("trusted beta job did not succeed: %s", result)
	}
	if got := len(host.approvals.Pending()); got != 0 {
		t.Fatalf("trusted agents triggered %d approval prompts, want none", got)
	}
	if res := callTool(t, beta, "desktop__job_status", map[string]any{"job_id": alphaJob.JobID}); !res.IsError {
		t.Fatalf("second agent read first agent's private job %s: %s", alphaJob.JobID, resultText(res))
	}

	otherJob := submitTrustTestJob(t, gamma, "other-device-needs-approval")
	waitTrustTestApproval(t, host, gamma, otherJob.JobID)
	if result := waitTrustTestJob(t, gamma, otherJob.JobID); !strings.Contains(result, "\"succeeded\"") {
		t.Fatalf("approved other-device job did not succeed: %s", result)
	}
	if got := len(host.approvals.Pending()); got != 0 {
		t.Fatalf("approval prompt remained after allowing other device: %d", got)
	}

	if status := trustAPICall(t, host, host.controlToken, http.MethodDelete, "/v1/trust/"+trusted.ID(), nil); status != http.StatusNoContent {
		t.Fatalf("owner trust revocation status=%d, want 204", status)
	}
	revokedJob := submitTrustTestJob(t, alpha, "revoked-device-needs-approval")
	waitTrustTestApproval(t, host, alpha, revokedJob.JobID)
	if result := waitTrustTestJob(t, alpha, revokedJob.JobID); !strings.Contains(result, "\"succeeded\"") {
		t.Fatalf("job after revocation did not succeed once approved: %s", result)
	}

	// Unpair both sides, then pair the same identity again. Pairing must not
	// recreate the owner-controlled trust grant.
	if status := trustAPICall(t, host, host.controlToken, http.MethodDelete, "/v1/peers/"+trusted.ID(), nil); status != http.StatusOK {
		t.Fatalf("host unpair status=%d, want 200", status)
	}
	if status := trustAPICall(t, trusted, trusted.controlToken, http.MethodDelete, "/v1/peers/"+host.ID(), nil); status != http.StatusOK {
		t.Fatalf("device unpair status=%d, want 200", status)
	}
	pair(t, host, trusted)
	if host.trust.Allows(trusted.ID()) {
		t.Fatalf("re-pair silently restored trust for %s", trusted.ID())
	}

	// The independent caller decision confirms that post-re-pair native jobs
	// still pass through the approval gate rather than inheriting old trust.
	alpha = trustAgentSession(t, trusted, alphaToken)
	waitForTool(t, alpha, "desktop__job_submit")
	repairedJob := submitTrustTestJob(t, alpha, "repaired-device-needs-approval")
	waitTrustTestApproval(t, host, alpha, repairedJob.JobID)
	if result := waitTrustTestJob(t, alpha, repairedJob.JobID); !strings.Contains(result, "\"succeeded\"") {
		t.Fatalf("re-paired job did not succeed after approval: %s", result)
	}
}
