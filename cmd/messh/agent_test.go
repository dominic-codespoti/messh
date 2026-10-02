package main

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
)

func agentTestRun(t *testing.T, dir string, args ...string) (int, string, string) {
	t.Helper()
	stdin, stdinW := io.Pipe()
	var stdout, stderr bytes.Buffer
	code := run(append(args, "--state", dir), stdin, true, &stdout, &stderr)
	// Never write: the read end would block forever (proves no command waits for typed input).
	_ = stdinW
	return code, stdout.String(), stderr.String()
}

func agentJSON(t *testing.T, dir string, args ...string) (int, map[string]any, string) {
	t.Helper()
	code, out, errOut := agentTestRun(t, dir, append(args, "--json")...)
	var doc map[string]any
	if out != "" {
		if err := json.Unmarshal([]byte(out), &doc); err != nil {
			t.Fatalf("%v --json stdout is not a JSON object: %v\n%s", args, err, out)
		}
	}
	return code, doc, errOut
}

func agentJSONList(t *testing.T, dir string, args ...string) (int, []any, string) {
	t.Helper()
	code, out, errOut := agentTestRun(t, dir, append(args, "--json")...)
	var rows []any
	if out != "" {
		if err := json.Unmarshal([]byte(out), &rows); err != nil {
			t.Fatalf("%v --json stdout is not a JSON array: %v\n%s", args, err, out)
		}
	}
	return code, rows, errOut
}

func TestAgentAddCreatesCompactToken(t *testing.T) {
	dir := t.TempDir()
	code, doc, errOut := agentJSON(t, dir, "agent", "add", "pi")
	if code != 0 {
		t.Fatalf("agent add exit = %d, stderr = %s", code, errOut)
	}
	if doc["agent"] != "pi" || doc["mode"] != "compact" || doc["created"] != true {
		t.Fatalf("agent add doc = %v, want agent pi mode compact created true", doc)
	}
	wantKeys := []string{"agent", "mode", "created", "mcp_url", "token_command"}
	if len(doc) != len(wantKeys) {
		t.Fatalf("agent add JSON fields = %v, want exactly %v", doc, wantKeys)
	}
	for _, key := range wantKeys {
		if _, ok := doc[key]; !ok {
			t.Errorf("agent add JSON lacks %q: %v", key, doc)
		}
	}
	if cmd, _ := doc["token_command"].(string); cmd != tokenCommand("pi", true, dir) {
		t.Fatalf("token command = %q", cmd)
	}
	if doc["mcp_url"] == "" {
		t.Fatal("agent add JSON lacks MCP URL")
	}
	code, out, errOut := agentTestRun(t, dir, "agent", "add", "pi")
	if code != 0 {
		t.Fatalf("re-add exit = %d, stderr = %s", code, errOut)
	}
	if !strings.Contains(out, "Connect an MCP client") || !strings.Contains(out, "Tools: compact") {
		t.Fatalf("generic human output lacks connection details:\n%s", out)
	}
	if strings.Contains(out, "mcp.json") || strings.Contains(out, "messh.json") || strings.Contains(out, "pi install") {
		t.Fatalf("generic output contains harness configuration:\n%s", out)
	}
	_, tokenDoc, errOut := agentJSON(t, dir, "agent", "token", "pi")
	if errOut != "" {
		t.Fatalf("agent token stderr = %s", errOut)
	}
	token, _ := tokenDoc["token"].(string)
	if token == "" || strings.Contains(out, token) || strings.Contains(doc["token_command"].(string), token) {
		t.Fatalf("agent add output exposed token or token missing")
	}
}

func TestAgentReAddKeepsTokenChangesMode(t *testing.T) {
	dir := t.TempDir()
	if code, _, errOut := agentJSON(t, dir, "agent", "add", "pi"); code != 0 {
		t.Fatalf("agent add exit = %d, stderr = %s", code, errOut)
	}
	mustToken := func() string {
		code, doc, errOut := agentJSON(t, dir, "agent", "token", "pi")
		if code != 0 {
			t.Fatalf("agent token exit = %d, stderr = %s", code, errOut)
		}
		tok, _ := doc["token"].(string)
		return tok
	}
	before := mustToken()
	code, doc, errOut := agentJSON(t, dir, "agent", "add", "pi", "--tools", "full")
	if code != 0 {
		t.Fatalf("re-add --tools full exit = %d, stderr = %s", code, errOut)
	}
	if doc["mode"] != "full" || doc["created"] != false {
		t.Fatalf("re-add doc = %v, want mode full created false", doc)
	}
	if after := mustToken(); after != before {
		t.Fatalf("re-add changed the token")
	}
}

func TestAgentModeRejectsBogus(t *testing.T) {
	dir := t.TempDir()
	code, _, errOut := agentJSON(t, dir, "agent", "mode", "pi", "bogus")
	if code != exitUsage {
		t.Fatalf("agent mode bogus exit = %d, want %d (stderr %s)", code, exitUsage, errOut)
	}
	code, _, errOut = agentJSON(t, dir, "agent", "add", "pi", "--tools", "bogus")
	if code != exitUsage {
		t.Fatalf("agent add --tools bogus exit = %d, want %d (stderr %s)", code, exitUsage, errOut)
	}
}

func TestAgentLsListsModes(t *testing.T) {
	dir := t.TempDir()
	if code, _, errOut := agentJSON(t, dir, "agent", "add", "pi"); code != 0 {
		t.Fatalf("agent add exit = %d, stderr = %s", code, errOut)
	}
	code, rows, errOut := agentJSONList(t, dir, "agent", "ls")
	if code != 0 {
		t.Fatalf("agent ls exit = %d, stderr = %s", code, errOut)
	}
	if len(rows) != 1 {
		t.Fatalf("agent ls rows = %v, want one", rows)
	}
	row, _ := rows[0].(map[string]any)
	if row["name"] != "pi" || row["mode"] != "compact" {
		t.Fatalf("agent ls row = %v, want name pi mode compact", row)
	}
}

func TestAgentTokenBearerAndRemove(t *testing.T) {
	dir := t.TempDir()
	if code, _, errOut := agentJSON(t, dir, "agent", "add", "pi"); code != 0 {
		t.Fatalf("agent add exit = %d, stderr = %s", code, errOut)
	}
	code, doc, errOut := agentJSON(t, dir, "agent", "token", "pi", "--bearer")
	if code != 0 {
		t.Fatalf("agent token --bearer exit = %d, stderr = %s", code, errOut)
	}
	if tok, _ := doc["token"].(string); !strings.HasPrefix(tok, "Bearer ") || len(tok) <= len("Bearer ") {
		t.Fatalf("bearer token = %q, want the \"Bearer \" prefix", tok)
	}
	code, _, errOut = agentJSON(t, dir, "agent", "rm", "pi")
	if code != 0 {
		t.Fatalf("agent rm exit = %d, stderr = %s", code, errOut)
	}
	code, _, errOut = agentJSON(t, dir, "agent", "token", "pi")
	if code != exitFailure {
		t.Fatalf("agent token after rm exit = %d, want %d (stderr %s)", code, exitFailure, errOut)
	}
}
