package trust

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestAddAndAllowsAreDeviceScoped(t *testing.T) {
	store, err := New(filepath.Join(t.TempDir(), "trust.json"))
	if err != nil {
		t.Fatal(err)
	}
	if store.Allows("") || store.Allows("agent-id") {
		t.Fatal("empty store allowed an identity")
	}
	if _, err := store.Add("", "laptop"); err == nil {
		t.Fatal("Add accepted an empty device ID")
	}
	first, err := store.Add("device-a", "Laptop")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Add("device-b", "Laptop")
	if err != nil {
		t.Fatal(err)
	}
	if !store.Allows("device-a") || !store.Allows("device-b") || store.Allows("Laptop") || store.Allows("agent-id") {
		t.Fatal("trust was not keyed exclusively by device ID")
	}
	if first.DeviceID == second.DeviceID || first.Device != second.Device {
		t.Fatalf("unexpected device entries: %#v, %#v", first, second)
	}
	original, err := store.Add("device-a", "renamed laptop")
	if err != nil {
		t.Fatal(err)
	}
	if original != first {
		t.Fatalf("re-adding an ID changed its record: got %#v, want %#v", original, first)
	}
	if got := store.List(); len(got) != 2 || got[0].DeviceID != "device-a" || got[1].DeviceID != "device-b" {
		t.Fatalf("List is not sorted by exact ID: %#v", got)
	}
}

func TestRemovePersistsAndIsolatesDevices(t *testing.T) {
	file := filepath.Join(t.TempDir(), "trust.json")
	store, err := New(file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add("device-a", "Laptop"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add("device-b", "Laptop"); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove("device-a"); err != nil {
		t.Fatal(err)
	}
	if store.Allows("device-a") || !store.Allows("device-b") {
		t.Fatal("revocation did not isolate device IDs")
	}
	if err := store.Remove("device-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Remove absent ID error = %v, want ErrNotFound", err)
	}
	reloaded, err := New(file)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Allows("device-a") || !reloaded.Allows("device-b") {
		t.Fatal("revocation was not durable")
	}
}

func TestNewRejectsInvalidPersistedDocuments(t *testing.T) {
	cases := map[string]string{
		"malformed":           "{",
		"trailing value":      `{"version":1,"devices":[]} {}`,
		"unsupported version": `{"version":2,"devices":[]}`,
		"empty device ID":     `{"version":1,"devices":[{"device_id":"","device":"Laptop","created":"2026-01-01T00:00:00Z"}]}`,
		"empty device name":   `{"version":1,"devices":[{"device_id":"id","device":"","created":"2026-01-01T00:00:00Z"}]}`,
		"missing creation":    `{"version":1,"devices":[{"device_id":"id","device":"Laptop"}]}`,
		"duplicate ID":        `{"version":1,"devices":[{"device_id":"id","device":"Laptop","created":"2026-01-01T00:00:00Z"},{"device_id":"id","device":"Phone","created":"2026-01-01T00:00:00Z"}]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "trust.json")
			if err := os.WriteFile(file, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := New(file); err == nil {
				t.Fatal("New accepted invalid persisted data")
			}
		})
	}
}

func TestPersistenceFailuresDoNotChangeMemory(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "blocked-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := New(filepath.Join(parent, "trust.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(parent); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(parent, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add("device-a", "Laptop"); err == nil {
		t.Fatal("Add succeeded despite persistence failure")
	}
	if store.Allows("device-a") || len(store.List()) != 0 {
		t.Fatal("failed Add changed in-memory policy")
	}

	file := filepath.Join(root, "trust.json")
	store, err = New(file)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Add("device-a", "Laptop"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(file, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.Remove("device-a"); err == nil {
		t.Fatal("Remove succeeded despite persistence failure")
	}
	if !store.Allows("device-a") || len(store.List()) != 1 {
		t.Fatal("failed Remove changed in-memory policy")
	}
}
