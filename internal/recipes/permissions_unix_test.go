//go:build !windows

package recipes

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRegistryRejectsBroadPOSIXPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recipes.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"recipes":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(path); err == nil || !strings.Contains(err.Error(), "permissions must be 0600") {
		t.Fatalf("New error = %v, want rejection for broad POSIX permissions", err)
	}
}
