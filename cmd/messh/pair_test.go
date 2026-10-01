package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"

	"messh/internal/node"
	"messh/internal/state"
)

// blockingStdin returns a reader that blocks forever, proving no command
// reads stdin: any read would hang the test.
func blockingStdin() (io.Reader, bool) {
	r, _ := io.Pipe() // never written, never closed
	return r, true
}

func startTestNode(t *testing.T, name string) (*node.Node, state.Paths) {
	t.Helper()
	paths := state.Paths{Root: t.TempDir()}
	n, err := node.Start(context.Background(), node.Options{
		Paths:     paths,
		Name:      name,
		MeshAddr:  "127.0.0.1:0",
		LocalAddr: "127.0.0.1:0",
		Logger:    slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	return n, paths
}

func runJSON(t *testing.T, args []string) (int, map[string]any, string) {
	t.Helper()
	stdin, tty := blockingStdin()
	var stdout, stderr bytes.Buffer
	code := run(append(args, "--json"), stdin, tty, &stdout, &stderr)
	var v map[string]any
	if out := strings.TrimSpace(stdout.String()); out != "" {
		var arr any
		if err := json.Unmarshal([]byte(out), &arr); err != nil {
			t.Fatalf("args %v: stdout is not JSON %q: %v", args, out, err)
		}
		if m, ok := arr.(map[string]any); ok {
			v = m
		} else {
			v = map[string]any{"_array": arr}
		}
	}
	return code, v, stderr.String()
}

func runJSONList(t *testing.T, args []string) (int, []map[string]any) {
	t.Helper()
	stdin, tty := blockingStdin()
	var stdout, stderr bytes.Buffer
	code := run(append(args, "--json"), stdin, tty, &stdout, &stderr)
	out := strings.TrimSpace(stdout.String())
	if out == "" {
		t.Fatalf("args %v: empty stdout", args)
	}
	var list []map[string]any
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("args %v: stdout is not a JSON array %q: %v", args, out, err)
	}
	if list == nil {
		list = []map[string]any{}
	}
	return code, list
}

func TestPairFlowEndToEnd(t *testing.T) {
	a, pathsA := startTestNode(t, "alpha")
	b, pathsB := startTestNode(t, "beta")

	stateA := []string{"--state", pathsA.Root}
	stateB := []string{"--state", pathsB.Root}

	// Open the window on A.
	if code, _, _ := runJSON(t, append([]string{"pair", "accept"}, stateA...)); code != 0 {
		t.Fatalf("pair accept: exit %d", code)
	}

	// B starts pairing with A's mesh address.
	args := append([]string{"pair", a.MeshAddr()}, stateB...)
	code, started, _ := runJSON(t, args)
	if code != 0 {
		t.Fatalf("pair target: exit %d", code)
	}
	outID, _ := started["id"].(string)
	outCode, _ := started["code"].(string)
	if outID == "" || outCode == "" {
		t.Fatalf("pair target returned no id/code: %v", started)
	}
	if started["state"] != "awaiting_confirmation" {
		t.Fatalf("pair target state = %v, want awaiting_confirmation", started["state"])
	}

	// A sees the request with the same code.
	_, reqs := runJSONList(t, append([]string{"pair", "requests"}, stateA...))
	if len(reqs) != 1 {
		t.Fatalf("pair requests: got %d requests, want 1", len(reqs))
	}
	inID, _ := reqs[0]["id"].(string)
	inCode, _ := reqs[0]["code"].(string)
	if inID == "" {
		t.Fatalf("request has no id: %v", reqs[0])
	}
	// Digits-only equality across formats.
	if inCode != outCode {
		t.Fatalf("codes differ: incoming %q vs outgoing %q", inCode, outCode)
	}

	// Wrong code: exit 1 and still pending.
	stdin, tty := blockingStdin()
	var stdout, stderr bytes.Buffer
	wrongArgs := []string{"pair", "approve", inID, "--code", "000-000", "--state", pathsA.Root, "--json"}
	if c := run(wrongArgs, stdin, tty, &stdout, &stderr); c != 1 {
		t.Fatalf("pair approve wrong code: exit %d, want 1 (stderr %q)", c, stderr.String())
	}
	_, reqs2 := runJSONList(t, append([]string{"pair", "requests"}, stateA...))
	if len(reqs2) != 1 {
		t.Fatalf("wrong-code approve removed the request: %d requests", len(reqs2))
	}

	// Approve with the right code (different formatting still matches).
	spaced := strings.Join(strings.Split(inCode, ""), " ")
	approveArgs := append([]string{"pair", "approve", inID, "--code", spaced}, stateA...)
	if c, _, _ := runJSON(t, approveArgs); c != 0 {
		t.Fatalf("pair approve: exit %d", c)
	}

	// Wrong confirm code: exit 1.
	if c, _, _ := runJSON(t, append([]string{"pair", "confirm", outID, "--code", "000-000"}, stateB...)); c != 1 {
		t.Fatalf("pair confirm wrong code: exit %d, want 1", c)
	}

	// Confirm with the right code and wait for the pairing.
	if c, v, _ := runJSON(t, append([]string{"pair", "confirm", outID, "--code", outCode, "--wait", "10s"}, stateB...)); c != 0 {
		t.Fatalf("pair confirm: exit %d (%v)", c, v)
	} else if v["state"] != "paired" {
		t.Fatalf("pair confirm state = %v, want paired", v["state"])
	}

	// Both peers list each other.
	_, peersA := runJSONList(t, append([]string{"peers"}, stateA...))
	_, peersB := runJSONList(t, append([]string{"peers"}, stateB...))
	if len(peersA) != 1 || len(peersB) != 1 {
		t.Fatalf("peers: A=%d B=%d, want 1 each", len(peersA), len(peersB))
	}
	if peersA[0]["id"] != b.ID() {
		t.Fatalf("A peers %v, want id %s", peersA[0], b.ID())
	}
	if peersB[0]["id"] != a.ID() {
		t.Fatalf("B peers %v, want id %s", peersB[0], a.ID())
	}
	_ = b
}

func TestNoNodeExits3(t *testing.T) {
	for _, stale := range []bool{false, true} {
		name := "missing"
		if stale {
			name = "stale"
		}
		t.Run(name, func(t *testing.T) {
			paths := state.Paths{Root: t.TempDir()}
			if stale {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				addr := listener.Addr().String()
				listener.Close()
				if err := paths.SaveRunInfo(state.RunInfo{Local: addr}); err != nil {
					t.Fatal(err)
				}
			}
			for _, args := range [][]string{{"status"}, {"tools", "--timeout", "2s"}, {"call", "mesh_nodes", "--timeout", "2s"}} {
				t.Run(args[0], func(t *testing.T) {
					stdin, tty := blockingStdin()
					var stdout, stderr bytes.Buffer
					code := run(append(args, "--state", paths.Root, "--json"), stdin, tty, &stdout, &stderr)
					if code != exitNoNode {
						t.Fatalf("exit %d, want %d (stderr %q)", code, exitNoNode, stderr.String())
					}
					kind, hint := contractRunErr(t, &stderr)
					if kind != "node_not_running" || hint == "" {
						t.Fatalf("kind=%q hint=%q, want node_not_running and recovery hint", kind, hint)
					}
				})
			}
		})
	}
}
