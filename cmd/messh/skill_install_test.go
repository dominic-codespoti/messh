package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSkillInstallRequiresExplicitTargetAndPreservesExistingContent(t *testing.T) {
	stateDir := t.TempDir()
	code, _, errOut := agentTestRun(t, stateDir, "skill", "install")
	if code != exitUsage || errOut == "" {
		t.Fatalf("skill install without target = %d, stderr %q; want usage error", code, errOut)
	}

	skillsDir := filepath.Join(t.TempDir(), "custom skills")
	target := filepath.Join(skillsDir, skillName, "SKILL.md")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	userContent := []byte("user-managed skill\n")
	if err := os.WriteFile(target, userContent, 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errOut = agentTestRun(t, stateDir, "skill", "install", "--dir", skillsDir)
	if code != exitUsage || errOut == "" {
		t.Fatalf("skill install over different content = %d, stderr %q; want usage error", code, errOut)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(userContent) {
		t.Fatalf("existing skill changed without --force: %q", got)
	}
}
