package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSkillInstallRequiresExplicitTargetAndInstallsDirectory(t *testing.T) {
	stateDir := t.TempDir()
	code, _, errOut := agentTestRun(t, stateDir, "skill", "install")
	if code != exitUsage || errOut == "" {
		t.Fatalf("skill install without target = %d, stderr %q; want usage error", code, errOut)
	}

	skillsDir := filepath.Join(t.TempDir(), "custom skills")
	code, _, errOut = agentTestRun(t, stateDir, "skill", "install", "--dir", skillsDir)
	if code != exitOK {
		t.Fatalf("skill install --dir exit = %d, stderr = %s", code, errOut)
	}
	assertInstalledSkill(t, filepath.Join(skillsDir, skillName, "SKILL.md"))

	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	code, _, errOut = agentTestRun(t, stateDir, "skill", "install", "--for", "pi")
	if code != exitOK {
		t.Fatalf("skill install --for pi exit = %d, stderr = %s", code, errOut)
	}
	assertInstalledSkill(t, filepath.Join(home, ".pi", "agent", "skills", skillName, "SKILL.md"))
}

func assertInstalledSkill(t *testing.T, path string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed skill %s: %v", path, err)
	}
	if string(got) != skillMarkdown {
		t.Fatalf("installed skill at %s differs from bundled content", path)
	}
}
