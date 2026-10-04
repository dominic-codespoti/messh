package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"messh/internal/state"
)

func nodeConfigJSON(t *testing.T, dir string, args ...string) (map[string]any, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	full := append(append([]string{}, args...), "--state", dir, "--json")
	if code := run(full, strings.NewReader(""), false, &stdout, &stderr); code != 0 {
		t.Fatalf("%v exit = %d, stderr = %s", args, code, stderr.String())
	}
	var doc map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatalf("%v stdout is not JSON: %v\n%s", args, err, stdout.String())
	}
	return doc, stderr.String()
}

func TestNodeConfigSetSparsePreservesUnsuppliedFields(t *testing.T) {
	dir := t.TempDir()
	paths := state.Paths{Root: dir}
	before := state.Config{Name: "keep", Listen: ":7521", Local: "127.0.0.1:7522"}
	if err := paths.SaveConfig(before); err != nil {
		t.Fatal(err)
	}
	doc, _ := nodeConfigJSON(t, dir, "node", "config", "set", "--listen", ":7531")
	if doc["name"] != "keep" || doc["listen"] != ":7531" || doc["local"] != "127.0.0.1:7522" || doc["restart_needed"] != true {
		t.Fatalf("sparse listen update = %v, want preserved name/local with restart_needed true", doc)
	}
	stored, err := paths.LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if stored.Name != "keep" || stored.Listen != ":7531" || stored.Local != "127.0.0.1:7522" {
		t.Fatalf("stored after sparse update = %+v", stored)
	}
	doc, _ = nodeConfigJSON(t, dir, "node", "config", "set", "--name", "renamed")
	if doc["name"] != "renamed" || doc["listen"] != ":7531" || doc["local"] != "127.0.0.1:7522" {
		t.Fatalf("sparse name update = %v, want preserved addresses", doc)
	}
}

func TestNodeConfigSetRejectsInvalidBeforePublication(t *testing.T) {
	dir := t.TempDir()
	paths := state.Paths{Root: dir}
	before := state.Config{Name: "stable", Listen: ":7521", Local: "127.0.0.1:7522"}
	if err := paths.SaveConfig(before); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"node", "config", "set", "--name", " bad"},
		{"node", "config", "set", "--name", ""},
		{"node", "config", "set", "--listen", "not-an-address"},
		{"node", "config", "set", "--listen", ":abc"},
		{"node", "config", "set", "--local", "localhost:7522"},
		{"node", "config", "set", "--local", "0.0.0.0:7522"},
		{"node", "config", "set", "--local", "192.168.1.31:7522"},
		{"node", "config", "set", "--local", ":7522"},
	} {
		var stdout, stderr bytes.Buffer
		full := append(append([]string{}, args...), "--state", dir)
		code := run(full, strings.NewReader(""), false, &stdout, &stderr)
		if code != exitUsage {
			t.Fatalf("%v exit = %d, want %d (%s)", args, code, exitUsage, stderr.String())
		}
		if stored, err := paths.LoadConfig(); err != nil || stored != before {
			t.Fatalf("%v mutated config to %+v, %v", args, stored, err)
		}
	}
}

func TestNodeConfigSetNoFlagsIsUsage(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"node", "config", "set", "--state", dir}, strings.NewReader(""), false, &stdout, &stderr); code != exitUsage {
		t.Fatalf("bare node config set exit = %d, want %d (%s)", code, exitUsage, stderr.String())
	}
}

func TestNodeConfigSetIdempotentReportsNoRestart(t *testing.T) {
	dir := t.TempDir()
	paths := state.Paths{Root: dir}
	before := state.Config{Name: "steady", Listen: ":7521", Local: "127.0.0.1:7522"}
	if err := paths.SaveConfig(before); err != nil {
		t.Fatal(err)
	}
	doc, _ := nodeConfigJSON(t, dir, "node", "config", "set", "--listen", ":7521")
	if doc["restart_needed"] != false {
		t.Fatalf("identical update = %v, want restart_needed false", doc)
	}
	if stored, err := paths.LoadConfig(); err != nil || stored != before {
		t.Fatalf("identical update mutated config to %+v, %v", stored, err)
	}
}

func TestNodeConfigSetZeroPortsPersist(t *testing.T) {
	dir := t.TempDir()
	doc, _ := nodeConfigJSON(t, dir, "node", "config", "set", "--listen", ":0", "--local", "127.0.0.1:0")
	if doc["listen"] != ":0" || doc["local"] != "127.0.0.1:0" || doc["restart_needed"] != true {
		t.Fatalf("zero-port update = %v, want persisted with restart_needed true", doc)
	}
}
