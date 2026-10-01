package files

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"messh/internal/state"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	root := t.TempDir()
	s, err := NewStore(state.Paths{Root: root})
	if err != nil {
		t.Fatal(err)
	}
	s.FreeSpace = func(string) (uint64, uint64, error) { return 1 << 40, 1 << 41, nil }
	return s, root
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func put(t *testing.T, s *Store, ref string, data []byte, overwrite bool) (Info, error) {
	t.Helper()
	u, err := s.BeginWrite(ref, WriteOptions{Size: int64(len(data)), Overwrite: overwrite})
	if err != nil {
		return Info{}, err
	}
	if _, err := u.Write(data); err != nil {
		u.Abort()
		return Info{}, err
	}
	return u.Commit(sum(data))
}

// leftovers lists temp files anywhere under the state directory.
func leftovers(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), tempPrefix) {
			out = append(out, p)
		}
		return nil
	})
	return out
}

// mkDirLink makes link point at target: a symlink where the OS allows it, a
// junction on Windows otherwise. It skips the test if neither is possible.
func mkDirLink(t *testing.T, link, target string) {
	t.Helper()
	if err := os.Symlink(target, link); err == nil {
		return
	}
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err == nil {
			return
		} else {
			t.Skipf("cannot create a symlink or junction: %v %s", err, out)
		}
	}
	t.Skip("cannot create symlinks here")
}

func TestRefsCannotEscapeTheRoots(t *testing.T) {
	s, root := newTestStore(t)
	outside := filepath.Join(root, "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := []string{
		"../secret.txt", "ws/../secret.txt", "ws/../../secret.txt", "ws/a/../../secret.txt",
		"/secret.txt", "/etc/passwd", `ws\..\secret.txt`, `..\secret.txt`, `C:\Windows\win.ini`, "C:/Windows/win.ini",
		"c:secret.txt", "ws/C:/x", "ws/a:b", "identity/key.pem", "peers.json", "ws/..", "..", "", "ws/\x00x",
		"ws/CON", "ws/nul.txt", "ws/LPT1", "ws/a/com3.log", "ws/.messh-1.part", "ws/ trailing ", "ws/dot.",
	}
	for _, ref := range bad {
		if _, err := s.Stat(ref, false); err == nil && ref != "" {
			t.Errorf("Stat(%q) succeeded", ref)
		}
		if _, _, err := s.OpenFile(ref); err == nil {
			t.Errorf("OpenFile(%q) succeeded", ref)
		}
		if _, err := s.List(ref); err == nil && ref != "" {
			t.Errorf("List(%q) succeeded", ref)
		}
		if err := s.Mkdir(ref); err == nil {
			t.Errorf("Mkdir(%q) succeeded", ref)
		}
		if err := s.Delete(ref); err == nil {
			t.Errorf("Delete(%q) succeeded", ref)
		}
		if _, err := s.BeginWrite(ref, WriteOptions{Size: 1}); err == nil {
			t.Errorf("BeginWrite(%q) succeeded", ref)
		}
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "secret" {
		t.Fatalf("file outside the roots was touched: %q %v", data, err)
	}
}

func TestSymlinksAndJunctionsAreNeverFollowed(t *testing.T) {
	s, root := newTestStore(t)
	outsideDir := filepath.Join(root, "outside")
	if err := os.MkdirAll(outsideDir, 0o700); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := put(t, s, "ws/job/real.txt", []byte("real"), false); err != nil {
		t.Fatal(err)
	}
	mkDirLink(t, filepath.Join(root, "ws", "job", "escape"), outsideDir)

	for _, ref := range []string{"ws/job/escape", "ws/job/escape/secret.txt", "ws/job/escape/new.txt"} {
		if _, err := s.Stat(ref, false); err == nil {
			t.Errorf("Stat(%q) followed the link", ref)
		}
		if _, _, err := s.OpenFile(ref); err == nil {
			t.Errorf("OpenFile(%q) followed the link", ref)
		}
		if _, err := s.List(ref); err == nil {
			t.Errorf("List(%q) followed the link", ref)
		}
	}
	if _, err := s.BeginWrite("ws/job/escape/new.txt", WriteOptions{Size: 1}); err == nil {
		t.Error("BeginWrite wrote through the link")
	}
	if err := s.Mkdir("ws/job/escape/sub"); err == nil {
		t.Error("Mkdir created a directory through the link")
	}
	if _, err := os.Stat(filepath.Join(outsideDir, "new.txt")); err == nil {
		t.Error("a file appeared outside the roots")
	}

	// The link is invisible in listings and walks.
	l, err := s.List("ws/job")
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Entries) != 1 || l.Entries[0].Name != "real.txt" || l.Skipped != 1 {
		t.Fatalf("listing = %+v, want only real.txt and one skipped entry", l)
	}
	w, err := s.Walk("ws/job")
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Entries) != 1 || w.Skipped != 1 {
		t.Fatalf("walk = %+v", w)
	}

	// Deleting the tree removes the link itself and leaves its target alone.
	if err := s.Delete("ws/job"); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(secret); err != nil || string(data) != "secret" {
		t.Fatalf("deleting a workspace reached through a link into %s: %q %v", outsideDir, data, err)
	}
}

func TestSymlinkToFileInsideWorkspaceIsNotServed(t *testing.T) {
	s, root := newTestStore(t)
	if _, err := put(t, s, "ws/job/real.txt", []byte("real"), false); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "ws", "job", "real.txt"), filepath.Join(root, "ws", "job", "ln.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, _, err := s.OpenFile("ws/job/ln.txt"); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("OpenFile through an in-workspace symlink = %v, want ErrNotRegular", err)
	}
	if _, err := put(t, s, "ws/job/ln.txt", []byte("x"), true); !errors.Is(err, ErrNotRegular) {
		t.Fatalf("overwriting a symlink = %v, want ErrNotRegular", err)
	}
}

func TestOverwriteRules(t *testing.T) {
	s, root := newTestStore(t)
	if _, err := put(t, s, "ws/job/a.txt", []byte("one"), false); err != nil {
		t.Fatal(err)
	}
	if _, err := put(t, s, "ws/job/a.txt", []byte("two"), false); !errors.Is(err, ErrExists) {
		t.Fatalf("second write without overwrite = %v, want ErrExists", err)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "ws", "job", "a.txt")); string(data) != "one" {
		t.Fatalf("file changed to %q despite refused overwrite", data)
	}
	if in, err := put(t, s, "ws/job/a.txt", []byte("three"), true); err != nil || in.Size != 5 || in.SHA256 != sum([]byte("three")) {
		t.Fatalf("overwrite = %+v, %v", in, err)
	}
	if err := s.Mkdir("ws/job/dir"); err != nil {
		t.Fatal(err)
	}
	if _, err := put(t, s, "ws/job/dir", []byte("x"), true); !errors.Is(err, ErrIsDirectory) {
		t.Fatalf("writing over a directory = %v, want ErrIsDirectory", err)
	}
	if _, err := put(t, s, "ws/job/a.txt/inner", []byte("x"), true); err == nil {
		t.Fatal("wrote beneath a regular file")
	}
	if got := leftovers(t, root); len(got) != 0 {
		t.Fatalf("temp files left behind: %v", got)
	}
}

func TestUploadIntegrity(t *testing.T) {
	s, root := newTestStore(t)
	data := bytes.Repeat([]byte("integrity"), 1000)

	u, err := s.BeginWrite("ws/job/a.bin", WriteOptions{Size: int64(len(data))})
	if err != nil {
		t.Fatal(err)
	}
	u.Write(data)
	if _, err := u.Commit(sum([]byte("something else"))); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("wrong digest accepted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "ws", "job", "a.bin")); err == nil {
		t.Fatal("corrupt file was published")
	}

	u, err = s.BeginWrite("ws/job/a.bin", WriteOptions{Size: int64(len(data))})
	if err != nil {
		t.Fatal(err)
	}
	u.Write(data[:len(data)-1])
	if _, err := u.Commit(""); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("short upload accepted: %v", err)
	}

	u, err = s.BeginWrite("ws/job/a.bin", WriteOptions{Size: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := u.Write([]byte("12345")); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("more data than announced accepted: %v", err)
	}
	u.Abort()

	if got := leftovers(t, root); len(got) != 0 {
		t.Fatalf("temp files left behind: %v", got)
	}
	if _, err := put(t, s, "ws/job/a.bin", data, false); err != nil {
		t.Fatalf("good upload after failed ones: %v", err)
	}
}

func TestFreeSpaceReserve(t *testing.T) {
	s, _ := newTestStore(t)
	var probed string
	free, total := uint64(1000), uint64(1000)
	s.FreeSpace = func(dir string) (uint64, uint64, error) { probed = dir; return free, total, nil }

	s.ReserveBytes, s.ReservePercent = 100, 0
	if u, err := s.BeginWrite("ws/job/deep/er/a.bin", WriteOptions{Size: 900}); err != nil {
		t.Fatalf("size leaving exactly the reserve refused: %v", err)
	} else {
		u.Abort()
	}
	if !strings.HasSuffix(filepath.ToSlash(probed), "/ws") {
		t.Fatalf("probed %q, want the nearest existing directory (the ws root)", probed)
	}
	if _, err := s.BeginWrite("ws/job/a.bin", WriteOptions{Size: 901}); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("size eating into the reserve = %v, want ErrNoSpace", err)
	}
	if _, err := s.BeginWrite("ws/job/a.bin", WriteOptions{Size: 5000}); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("size beyond free space = %v, want ErrNoSpace", err)
	}

	// The percentage caps the reserve on small disks.
	s.ReserveBytes, s.ReservePercent = 1<<30, 5
	if u, err := s.BeginWrite("ws/job/a.bin", WriteOptions{Size: 950}); err != nil {
		t.Fatalf("5%% reserve of a 1000 byte disk refused 950 bytes: %v", err)
	} else {
		u.Abort()
	}
	if _, err := s.BeginWrite("ws/job/a.bin", WriteOptions{Size: 951}); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("951 bytes = %v, want ErrNoSpace", err)
	}

	s.FreeSpace = func(string) (uint64, uint64, error) { return 0, 0, errors.New("probe failed") }
	if _, err := s.BeginWrite("ws/job/a.bin", WriteOptions{Size: 1}); err == nil {
		t.Fatal("write allowed although free space is unknown")
	}
}

func TestWritesOnlyUnderWorkspaces(t *testing.T) {
	s, root := newTestStore(t)
	if _, err := put(t, s, "artifacts/svc/a.wav", []byte("x"), false); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("write under artifacts = %v, want ErrReadOnly", err)
	}
	if err := s.Mkdir("artifacts/svc/dir"); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("mkdir under artifacts = %v, want ErrReadOnly", err)
	}
	if _, err := put(t, s, "ws/a.txt", []byte("x"), false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("file directly in ws = %v, want ErrInvalid", err)
	}
	if err := s.Mkdir("ws"); !errors.Is(err, ErrRoot) {
		t.Fatalf("mkdir ws = %v, want ErrRoot", err)
	}
	long := "ws/" + strings.Repeat("d/", MaxWriteDepth)
	if _, err := put(t, s, long+"f", []byte("x"), false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("too deep = %v, want ErrInvalid", err)
	}
	if _, err := put(t, s, "ws/job/"+strings.Repeat("n", 100)+"/"+strings.Repeat("m", 100), []byte("x"), false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("too long = %v, want ErrInvalid", err)
	}

	// Artifacts can be read and deleted, and the roots cannot.
	artifact := filepath.Join(root, "artifacts", "svc")
	os.MkdirAll(artifact, 0o700)
	os.WriteFile(filepath.Join(artifact, "a.wav"), []byte("x"), 0o600)
	if _, err := s.Stat("artifacts/svc/a.wav", false); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("artifacts/svc/a.wav"); err != nil {
		t.Fatal(err)
	}
	for _, r := range []string{"ws", "artifacts"} {
		if err := s.Delete(r); !errors.Is(err, ErrRoot) {
			t.Fatalf("Delete(%q) = %v, want ErrRoot", r, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "ws")); err != nil {
		t.Fatal("ws root vanished")
	}
}

func TestListAndWalk(t *testing.T) {
	s, root := newTestStore(t)
	for _, f := range []string{"ws/p/b.txt", "ws/p/a/z.txt", "ws/p/a/y.txt"} {
		if _, err := put(t, s, f, []byte(f), false); err != nil {
			t.Fatal(err)
		}
	}
	s.Mkdir("ws/p/empty")
	// A name no ref can express, a temp file, and a stray symlink-free oddity.
	os.WriteFile(filepath.Join(root, "ws", "p", "caf\u00e9.txt"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(root, "ws", "p", ".messh-1.part"), []byte("x"), 0o600)

	l, err := s.List("ws/p")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range l.Entries {
		names = append(names, e.Name)
	}
	if !slices.Equal(names, []string{"a", "b.txt", "empty"}) || l.Skipped != 1 {
		t.Fatalf("list = %v skipped %d, want [a b.txt empty] and 1 skipped (temp files are not counted)", names, l.Skipped)
	}
	w, err := s.Walk("ws/p")
	if err != nil {
		t.Fatal(err)
	}
	var refs []string
	for _, e := range w.Entries {
		refs = append(refs, e.Ref)
	}
	want := []string{"ws/p/a", "ws/p/a/y.txt", "ws/p/a/z.txt", "ws/p/b.txt", "ws/p/empty"}
	if !slices.Equal(refs, want) {
		t.Fatalf("walk = %v, want %v", refs, want)
	}
	roots, err := s.List("")
	if err != nil || len(roots.Entries) != 2 {
		t.Fatalf("roots = %+v, %v", roots, err)
	}
}

func TestSweepRemovesOnlyOldTempFiles(t *testing.T) {
	s, root := newTestStore(t)
	dir := filepath.Join(root, "ws", "job")
	os.MkdirAll(dir, 0o700)
	old := filepath.Join(dir, ".messh-old.part")
	fresh := filepath.Join(dir, ".messh-fresh.part")
	keep := filepath.Join(dir, "keep.txt")
	for _, p := range []string{old, fresh, keep} {
		os.WriteFile(p, []byte("x"), 0o600)
	}
	past := time.Now().Add(-2 * time.Hour)
	os.Chtimes(old, past, past)
	os.Chtimes(keep, past, past)
	s.Sweep(context.Background(), time.Hour)
	for p, want := range map[string]bool{old: false, fresh: true, keep: true} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s exists = %v, want %v", filepath.Base(p), err == nil, want)
		}
	}
}
