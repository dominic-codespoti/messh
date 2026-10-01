package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func serviceTestRun(t *testing.T, args []string, stdin io.Reader, stdinTTY bool) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, stdin, stdinTTY, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func serviceTestState(t *testing.T, extraEnv ...string) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("MESSH_STATE", dir)
	for i := 0; i+1 < len(extraEnv); i += 2 {
		t.Setenv(extraEnv[i], extraEnv[i+1])
	}
	return dir
}

func TestServiceLsEmptyJSON(t *testing.T) {
	dir := serviceTestState(t)
	code, out, _ := serviceTestRun(t, []string{"service", "ls", "--json", "--state", dir}, strings.NewReader(""), false)
	if code != 0 {
		t.Fatalf("service ls --json exit = %d", code)
	}
	var v []any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &v); err != nil {
		t.Fatalf("service ls --json is not a JSON array: %v (out %q)", err, out)
	}
	if len(v) != 0 {
		t.Fatalf("service ls --json on empty state = %v, want []", v)
	}
}

func TestServiceAddBadKindExit2(t *testing.T) {
	dir := serviceTestState(t)
	code, _, _ := serviceTestRun(t,
		[]string{"service", "add", "x", "http://127.0.0.1:1", "--kind", "bogus", "--state", dir},
		strings.NewReader(""), false)
	if code != exitUsage {
		t.Fatalf("service add --kind bogus exit = %d, want %d", code, exitUsage)
	}
}

func TestBrowserResetProfileNeedsYes(t *testing.T) {
	dir := serviceTestState(t)
	profile := filepath.Join(dir, "browser", "profile")
	if err := os.MkdirAll(profile, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(profile, "keep")
	if err := os.WriteFile(sentinel, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	stdin, _ := io.Pipe() // never written, never closed: the command must not read it
	code, _, _ := serviceTestRun(t,
		[]string{"browser", "reset-profile", "--state", dir},
		stdin, false)
	if code != exitUsage {
		t.Fatalf("browser reset-profile without --yes exit = %d, want %d", code, exitUsage)
	}
	if _, err := os.Stat(sentinel); err != nil {
		t.Fatalf("profile was touched without --yes: %v", err)
	}
}

func TestBrowserSetupTokenTTYExit2(t *testing.T) {
	dir := serviceTestState(t)
	pr, pw := io.Pipe()
	defer pw.Close() // never written: the command must refuse before reading
	code, _, _ := serviceTestRun(t,
		[]string{"browser", "setup", "--mode", "extension", "--token", "-", "--state", dir},
		pr, true)
	if code != exitUsage {
		t.Fatalf("browser setup --token - on a terminal exit = %d, want %d", code, exitUsage)
	}
}
