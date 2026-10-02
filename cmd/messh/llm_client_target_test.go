package main

import (
	"flag"
	"strings"
	"testing"
)

func TestLLMConfigAgentTargetSelection(t *testing.T) {
	fs := flag.NewFlagSet("llm config", flag.ContinueOnError)
	registry["llm config"].Flags(fs)
	if got := fs.Lookup("for").DefValue; got != "openai" {
		t.Fatalf("default --for = %q, want openai", got)
	}
	for _, tc := range []struct{ harness, agent, want string }{
		{"omp", "", "omp"},
		{"pi", "", "pi"},
		{"omp", "custom", "custom"},
		{"openai", "custom", "custom"},
	} {
		got, err := llmConfigAgent(tc.harness, tc.agent)
		if err != nil || got != tc.want {
			t.Errorf("llmConfigAgent(%q, %q) = %q, %v; want %q", tc.harness, tc.agent, got, err, tc.want)
		}
	}
	for _, tc := range []struct{ harness, marker string }{
		{"omp", "providers:\n"},
		{"pi", "\"providers\""},
	} {
		text, _, err := renderLLMClientConfig(tc.harness, "provider", "http://localhost/v1", "messh agent token client", []string{"m1"}, nil, "approval wait", "service")
		if err != nil || !strings.Contains(text, tc.marker) {
			t.Errorf("%s renderer = %q, %v; want %q", tc.harness, text, err, tc.marker)
		}
	}
	text, _, err := renderLLMClientConfig("openai", "provider", "http://localhost/v1", "messh agent token client", []string{"m1"}, nil, "approval wait", "service")
	if err != nil || !strings.Contains(text, "OPENAI_BASE_URL") || strings.Contains(text, "providers:") || strings.Contains(text, "models.json") {
		t.Fatalf("generic renderer = %q, %v", text, err)
	}
	if _, err := llmConfigAgent("openai", ""); err == nil {
		t.Fatal("generic client accepted without explicit --agent")
	}
	if _, err := llmConfigAgent("bogus", "x"); err == nil {
		t.Fatal("unknown target accepted")
	}
}
