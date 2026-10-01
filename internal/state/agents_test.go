package state

import (
	"errors"
	"io/fs"
	"os"
	"testing"
)

func TestAgentModeDefaultsSetAndRemove(t *testing.T) {
	p := Paths{Root: t.TempDir()}
	tok, err := p.AddAgent("omp")
	if err != nil {
		t.Fatal(err)
	}
	if mode, err := p.AgentMode("omp"); err != nil || mode != ToolsCompact {
		t.Fatalf("default mode = %q, %v; want compact", mode, err)
	}

	if err := p.SetAgentMode("omp", ToolsFull); err != nil {
		t.Fatal(err)
	}
	if mode, err := p.AgentMode("omp"); err != nil || mode != ToolsFull {
		t.Fatalf("mode after set = %q, %v; want full", mode, err)
	}
	if again, err := p.AddAgent("omp"); err != nil || again != tok {
		t.Fatalf("re-adding changed the token: %q vs %q, %v", again, tok, err)
	}
	if mode, _ := p.AgentMode("omp"); mode != ToolsFull {
		t.Fatalf("re-adding reset the mode to %q", mode)
	}
	if names, err := p.Agents(); err != nil || len(names) != 1 || names[0] != "omp" {
		t.Fatalf("Agents() = %v, %v; the settings file must not show up as an agent", names, err)
	}
	if err := p.SetAgentMode("omp", ToolsCompact); err != nil {
		t.Fatal(err)
	}
	if mode, _ := p.AgentMode("omp"); mode != ToolsCompact {
		t.Fatalf("mode after switching back = %q", mode)
	}

	if err := p.SetAgentMode("omp", ToolsFull); err != nil {
		t.Fatal(err)
	}
	if err := p.RemoveAgent("omp"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p.agentSettingsFile("omp")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("settings file survived RemoveAgent: %v", err)
	}
	// A later agent with the same name starts from the default again.
	if _, err := p.AddAgent("omp"); err != nil {
		t.Fatal(err)
	}
	if mode, _ := p.AgentMode("omp"); mode != ToolsCompact {
		t.Fatalf("re-registered agent inherited mode %q", mode)
	}
}

func TestAgentModeCLIIsFull(t *testing.T) {
	p := Paths{Root: t.TempDir()}
	if mode, err := p.AgentMode("cli"); err != nil || mode != ToolsFull {
		t.Fatalf("cli mode = %q, %v; want full", mode, err)
	}
	if err := p.SetAgentMode("cli", ToolsCompact); err == nil {
		t.Fatal("SetAgentMode accepted the reserved cli agent")
	}
}

func TestAgentModeRejectsInvalid(t *testing.T) {
	p := Paths{Root: t.TempDir()}
	if _, err := p.AddAgent("pi"); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"", "Compact", "all", "full "} {
		if err := p.SetAgentMode("pi", mode); err == nil {
			t.Fatalf("SetAgentMode accepted %q", mode)
		}
	}
	if mode, _ := p.AgentMode("pi"); mode != ToolsCompact {
		t.Fatalf("rejected mode changed the stored one to %q", mode)
	}
	if err := p.SetAgentMode("ghost", ToolsFull); err == nil {
		t.Fatal("SetAgentMode accepted an unregistered agent")
	}
	if err := WriteFileAtomic(p.agentSettingsFile("pi"), []byte(`{"tools":"everything"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.AgentMode("pi"); err == nil {
		t.Fatal("AgentMode accepted an unknown mode from disk")
	}
	if _, err := p.AgentMode("Bad Name"); err == nil {
		t.Fatal("AgentMode accepted an invalid agent name")
	}
}
