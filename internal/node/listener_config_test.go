package node

import (
	"log/slog"
	"os"
	"strings"
	"testing"

	"messh/internal/state"
)

func TestPersistedListenersAndExplicitOverrideKeepIdentity(t *testing.T) {
	paths := state.Paths{Root: t.TempDir()}
	cfg := state.Config{Name: "configured-target", Listen: "127.0.0.2:0", Local: "127.0.0.2:0"}
	if err := paths.SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	n, err := Start(t.Context(), Options{Paths: paths, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	id := n.ID()
	if !strings.HasPrefix(n.MeshAddr(), "127.0.0.2:") || !strings.HasPrefix(n.LocalAddr(), "127.0.0.2:") {
		t.Fatalf("persisted listeners ignored: mesh=%s local=%s", n.MeshAddr(), n.LocalAddr())
	}
	n.Close()
	n, err = Start(t.Context(), Options{Paths: paths, MeshAddr: "127.0.0.1:0", LocalAddr: "127.0.0.1:0", Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	if n.ID() != id || n.Name() != cfg.Name || !strings.HasPrefix(n.MeshAddr(), "127.0.0.1:") || !strings.HasPrefix(n.LocalAddr(), "127.0.0.1:") {
		t.Fatalf("explicit override changed identity/name or ignored listener: id=%s name=%s mesh=%s local=%s", n.ID(), n.Name(), n.MeshAddr(), n.LocalAddr())
	}
	stored, err := paths.LoadConfig()
	if err != nil || stored != cfg {
		t.Fatalf("runtime override mutated persisted configuration: %+v, %v", stored, err)
	}
}

func TestUnsafePersistedLocalListenerFailsBeforeIdentityCreation(t *testing.T) {
	paths := state.Paths{Root: t.TempDir()}
	if err := paths.SaveConfig(state.Config{Name: "unsafe-target", Listen: "127.0.0.1:0", Local: "0.0.0.0:0"}); err != nil {
		t.Fatal(err)
	}
	n, err := Start(t.Context(), Options{Paths: paths, Logger: slog.New(slog.DiscardHandler)})
	if err == nil {
		n.Close()
		t.Fatal("nonloopback local API was exposed")
	}
	for _, path := range []string{paths.IdentityDir(), paths.RunFile(), paths.ControlTokenFile()} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("unsafe startup created identity/control state at %s: %v", path, err)
		}
	}
}
