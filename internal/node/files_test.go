package node

import (
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/files"
	"messh/internal/grants"
)

// startFileNode is startNode with a free-space probe that does not depend on
// the disk the tests happen to run on.
func startFileNode(t *testing.T, name string) *Node {
	t.Helper()
	n := startNode(t, name)
	n.files.store.FreeSpace = func(string) (uint64, uint64, error) { return 1 << 40, 1 << 41, nil }
	return n
}

// fileMesh is three paired nodes: the agent's raspi, which is paired with the
// desktop and the nas, which are not paired with each other.
type fileMesh struct {
	raspi, desktop, nas *Node
	agent               *mcp.ClientSession
}

func newFileMesh(t *testing.T) fileMesh {
	t.Helper()
	m := fileMesh{raspi: startFileNode(t, "raspi"), desktop: startFileNode(t, "desktop"), nas: startFileNode(t, "nas")}
	pair(t, m.desktop, m.raspi)
	pair(t, m.nas, m.raspi)
	for _, target := range []*Node{m.desktop, m.nas} {
		for _, path := range []string{"ws/job", "ws/inbox", "ws/demo", "ws/share", "ws/proj", "ws/proj-copy", "ws/model", "ws/in", "ws/new", "ws/x", "ws/job2"} {
			_, err := target.grants.Create(grants.Grant{Subject: grants.Subject{DeviceID: m.raspi.ID(), Agent: cliAgent}, Kind: "file", Path: path, Actions: []string{"read", "write"}, ExpiresAt: time.Now().Add(time.Hour)})
			if err != nil {
				t.Fatal(err)
			}
		}
		_, err := target.grants.Create(grants.Grant{Subject: grants.Subject{DeviceID: m.raspi.ID(), Agent: cliAgent}, Kind: "file", Path: "artifacts/voicestudio", Actions: []string{"read"}, ExpiresAt: time.Now().Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
	}
	m.agent = agentSession(t, m.raspi)
	return m
}

func writeWS(t *testing.T, n *Node, ref string, data []byte) {
	t.Helper()
	p := filepath.Join(n.paths.Root, filepath.FromSlash(ref))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func readWS(t *testing.T, n *Node, ref string) ([]byte, error) {
	t.Helper()
	return os.ReadFile(filepath.Join(n.paths.Root, filepath.FromSlash(ref)))
}

func digest(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func pseudoRandom(n int) []byte {
	b := make([]byte, n)
	r := rand.NewChaCha8([32]byte{1, 2, 3})
	r.Read(b)
	return b
}

type copyResult struct {
	Files, Bytes int64
	DestRef      string `json:"dest_ref"`
	To           string
	SHA256       string
	Seconds      float64
	Throughput   float64 `json:"throughput_mib_s"`
}

func meshCopy(t *testing.T, s *mcp.ClientSession, from, to string, overwrite bool) (copyResult, string, bool) {
	t.Helper()
	res, err := s.CallTool(t.Context(), &mcp.CallToolParams{Name: "mesh_copy", Arguments: map[string]any{"from": from, "to": to, "overwrite": overwrite}})
	if err != nil {
		t.Fatal(err)
	}
	text := res.Content[0].(*mcp.TextContent).Text
	var out copyResult
	if !res.IsError {
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			t.Fatalf("mesh_copy result %q: %v", text, err)
		}
	}
	return out, text, res.IsError
}

func mustCopy(t *testing.T, s *mcp.ClientSession, from, to string) copyResult {
	t.Helper()
	out, text, isErr := meshCopy(t, s, from, to, false)
	if isErr {
		t.Fatalf("mesh_copy %s -> %s failed: %s", from, to, text)
	}
	return out
}

func TestMeshCopyBetweenDevices(t *testing.T) {
	m := newFileMesh(t)
	payload := pseudoRandom(300_000)

	// local -> remote
	writeWS(t, m.raspi, "ws/job/in.bin", payload)
	out := mustCopy(t, m.agent, "ws/job/in.bin", "desktop:ws/inbox/in.bin")
	if out.Files != 1 || out.Bytes != int64(len(payload)) || out.SHA256 != digest(payload) || out.DestRef != "ws/inbox/in.bin" {
		t.Fatalf("local->remote result %+v", out)
	}
	if got, err := readWS(t, m.desktop, "ws/inbox/in.bin"); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("desktop copy differs: %v", err)
	}

	// remote -> local, from artifacts, into a directory
	audio := pseudoRandom(5000)
	writeWS(t, m.desktop, "artifacts/voicestudio/ab12cd.wav", audio)
	out = mustCopy(t, m.agent, "desktop:artifacts/voicestudio/ab12cd.wav", ":ws/demo/")
	if out.DestRef != "ws/demo/ab12cd.wav" {
		t.Fatalf("remote->local result %+v", out)
	}
	if got, err := readWS(t, m.raspi, "ws/demo/ab12cd.wav"); err != nil || !bytes.Equal(got, audio) {
		t.Fatalf("local copy differs: %v", err)
	}

	// remote -> remote, relayed through the agent's device; the two ends are not paired.
	out = mustCopy(t, m.agent, "desktop:ws/inbox/in.bin", "nas:ws/share/")
	if out.SHA256 != digest(payload) {
		t.Fatalf("relay digest %s", out.SHA256)
	}
	if got, err := readWS(t, m.nas, "ws/share/in.bin"); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("nas copy differs: %v", err)
	}
	if _, err := readWS(t, m.raspi, "ws/share/in.bin"); err == nil {
		t.Fatal("the relay left a copy on the agent's device")
	}

	// local -> local within ws
	mustCopy(t, m.agent, "ws/job/in.bin", "ws/job2/copy.bin")
	if got, err := readWS(t, m.raspi, "ws/job2/copy.bin"); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("local copy differs: %v", err)
	}
}

func TestMeshCopyRemoteToRemoteRequiresDestinationGrant(t *testing.T) {
	m := newFileMesh(t)
	payload := []byte("authorized relay")
	writeWS(t, m.desktop, "ws/job/relay.txt", payload)
	if _, text, isErr := meshCopy(t, m.agent, "desktop:ws/job/relay.txt", "nas:ws/relay/out.txt", false); !isErr || !strings.Contains(text, "capability denied") {
		t.Fatalf("ungranted relay: err=%v %q", isErr, text)
	}
	if _, err := readWS(t, m.nas, "ws/relay/out.txt"); err == nil {
		t.Fatal("ungranted relay published a destination file")
	}
	if _, err := m.nas.grants.Create(grants.Grant{Subject: grants.Subject{DeviceID: m.raspi.ID(), Agent: cliAgent}, Kind: "file", Path: "ws/relay", Actions: []string{"read", "write"}, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, text, isErr := meshCopy(t, m.agent, "desktop:ws/job/relay.txt", "nas:ws/relay/out.txt", false); isErr {
		t.Fatalf("granted relay: %q", text)
	}
	got, err := readWS(t, m.nas, "ws/relay/out.txt")
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("granted relay payload = %q, err=%v", got, err)
	}
}

func TestMeshCopyDirectory(t *testing.T) {
	m := newFileMesh(t)
	tree := map[string][]byte{
		"ws/proj/a.txt":         []byte("alpha"),
		"ws/proj/src/b.go":      []byte("package b"),
		"ws/proj/src/x/y/z.bin": pseudoRandom(70_000),
	}
	for ref, data := range tree {
		writeWS(t, m.desktop, ref, data)
	}
	os.MkdirAll(filepath.Join(m.desktop.paths.Root, "ws", "proj", "empty"), 0o700)

	out := mustCopy(t, m.agent, "desktop:ws/proj", "nas:ws/proj-copy")
	if out.Files != 3 {
		t.Fatalf("result %+v", out)
	}
	for ref, data := range tree {
		dst := "ws/proj-copy/" + strings.TrimPrefix(ref, "ws/proj/")
		if got, err := readWS(t, m.nas, dst); err != nil || !bytes.Equal(got, data) {
			t.Fatalf("%s differs: %v", dst, err)
		}
	}
	if fi, err := os.Stat(filepath.Join(m.nas.paths.Root, "ws", "proj-copy", "empty")); err != nil || !fi.IsDir() {
		t.Fatalf("empty directory not copied: %v", err)
	}

	// Without overwrite a second copy is refused whole; with it, it merges.
	if _, text, isErr := meshCopy(t, m.agent, "desktop:ws/proj", "nas:ws/proj-copy", false); !isErr || !strings.Contains(text, "overwrite") {
		t.Fatalf("second copy without overwrite: err=%v %q", isErr, text)
	}
	if _, text, isErr := meshCopy(t, m.agent, "desktop:ws/proj", "nas:ws/proj-copy", true); isErr {
		t.Fatalf("overwrite copy failed: %s", text)
	}
}

func TestMeshCopyLargeFileStreams(t *testing.T) {
	if testing.Short() {
		t.Skip("moves 64 MiB")
	}
	m := newFileMesh(t)
	big := pseudoRandom(64 << 20)
	writeWS(t, m.desktop, "ws/model/big.bin", big)

	// desktop -> nas relayed through the raspi, then nas -> raspi.
	out := mustCopy(t, m.agent, "desktop:ws/model/big.bin", "nas:ws/model/big.bin")
	if out.Bytes != int64(len(big)) || out.SHA256 != digest(big) || out.Throughput <= 0 {
		t.Fatalf("relay result %+v", out)
	}
	mustCopy(t, m.agent, "nas:ws/model/big.bin", "ws/model/")
	got, err := readWS(t, m.raspi, "ws/model/big.bin")
	if err != nil || !bytes.Equal(got, big) {
		t.Fatalf("64 MiB file differs after two hops: %v", err)
	}
}

// corrupter flips one byte of the traffic it is told to.
type corrupter struct {
	rt       http.RoundTripper
	get, put bool
}

type flipBody struct {
	io.ReadCloser
	pos int
}

func (f *flipBody) Read(p []byte) (int, error) {
	n, err := f.ReadCloser.Read(p)
	if at := 4000; at >= f.pos && at < f.pos+n {
		p[at-f.pos] ^= 0xff
	}
	f.pos += n
	return n, err
}

func (c corrupter) RoundTrip(r *http.Request) (*http.Response, error) {
	if c.put && r.Method == http.MethodPut {
		r.Body = &flipBody{ReadCloser: r.Body}
	}
	resp, err := c.rt.RoundTrip(r)
	if err == nil && c.get && r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, files.Path+"/") {
		resp.Body = &flipBody{ReadCloser: resp.Body}
	}
	return resp, err
}

func noTempFiles(t *testing.T, n *Node) {
	t.Helper()
	filepath.WalkDir(filepath.Join(n.paths.Root, "ws"), func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), ".messh-") {
			t.Errorf("temp file left on %s: %s", n.name, p)
		}
		return nil
	})
}

func TestMeshCopyDetectsCorruption(t *testing.T) {
	m := newFileMesh(t)
	payload := pseudoRandom(200_000)
	writeWS(t, m.raspi, "ws/job/a.bin", payload)
	writeWS(t, m.desktop, "ws/job/b.bin", payload)

	// corrupted on the way out to a peer
	m.raspi.files.wrapTransport = func(rt http.RoundTripper) http.RoundTripper { return corrupter{rt: rt, put: true} }
	if _, text, isErr := meshCopy(t, m.agent, "ws/job/a.bin", "desktop:ws/in/a.bin", false); !isErr || !strings.Contains(text, "integrity") {
		t.Fatalf("corrupted upload: err=%v %q", isErr, text)
	}
	if _, err := readWS(t, m.desktop, "ws/in/a.bin"); err == nil {
		t.Fatal("corrupted upload was published on the desktop")
	}

	// corrupted on the way in from a peer
	m.raspi.files.wrapTransport = func(rt http.RoundTripper) http.RoundTripper { return corrupter{rt: rt, get: true} }
	if _, text, isErr := meshCopy(t, m.agent, "desktop:ws/job/b.bin", "ws/in/b.bin", false); !isErr || !strings.Contains(text, "integrity") {
		t.Fatalf("corrupted download: err=%v %q", isErr, text)
	}
	if _, err := readWS(t, m.raspi, "ws/in/b.bin"); err == nil {
		t.Fatal("corrupted download was published locally")
	}

	// corrupted on either hop of a relay
	for _, c := range []corrupter{{get: true}, {put: true}} {
		m.raspi.files.wrapTransport = func(rt http.RoundTripper) http.RoundTripper { c.rt = rt; return c }
		if _, text, isErr := meshCopy(t, m.agent, "desktop:ws/job/b.bin", "nas:ws/in/b.bin", false); !isErr || !strings.Contains(text, "integrity") {
			t.Fatalf("relay with %+v: err=%v %q", c, isErr, text)
		}
		if _, err := readWS(t, m.nas, "ws/in/b.bin"); err == nil {
			t.Fatal("corrupted relay was published on the nas")
		}
	}

	// and with the tampering gone the same copy works
	m.raspi.files.wrapTransport = nil
	mustCopy(t, m.agent, "desktop:ws/job/b.bin", "nas:ws/in/b.bin")
	for _, n := range []*Node{m.raspi, m.desktop, m.nas} {
		noTempFiles(t, n)
	}
}

func TestMeshCopyRefusals(t *testing.T) {
	m := newFileMesh(t)
	if _, err := m.desktop.grants.Create(grants.Grant{Subject: grants.Subject{DeviceID: m.raspi.ID(), Agent: cliAgent}, Kind: "file", Path: "ws/nothing", Actions: []string{"read"}, ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	writeWS(t, m.raspi, "ws/job/a.txt", []byte("a"))

	cases := []struct{ name, from, to, want string }{
		{"unknown device", "toaster:ws/job/a.txt", "ws/x/a.txt", "no device"},
		{"write into artifacts", "ws/job/a.txt", "desktop:artifacts/x/a.txt", "read-only"},
		{"escape", "desktop:ws/../../etc/passwd", "ws/x/p", "invalid"},
		{"drive path", `C:\Windows\win.ini`, "ws/x/p", "no device"},
		{"missing source", "desktop:ws/nothing", "ws/x/p", "not exist"},
		{"missing args", "", "ws/x", "required"},
	}
	for _, c := range cases {
		_, text, isErr := meshCopy(t, m.agent, c.from, c.to, false)
		if !isErr || !strings.Contains(strings.ToLower(text), c.want) {
			t.Errorf("%s: err=%v %q, want an error containing %q", c.name, isErr, text, c.want)
		}
	}
	if _, err := os.Stat(filepath.Join(m.desktop.paths.Root, "artifacts", "x")); err == nil {
		t.Error("a peer-bound copy created something under artifacts")
	}
}

func TestFilesToolsReachPairedDevice(t *testing.T) {
	m := newFileMesh(t)
	writeWS(t, m.desktop, "ws/job/out.png", []byte("png"))
	waitFor(t, "desktop file tools", func() bool {
		for tool, err := range m.agent.Tools(t.Context(), nil) {
			if err == nil && tool.Name == "desktop__files_list" {
				return true
			}
		}
		return false
	})
	call := func(tool string, args map[string]any) (string, bool) {
		res, err := m.agent.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		return res.Content[0].(*mcp.TextContent).Text, res.IsError
	}
	text, isErr := call("desktop__files_list", map[string]any{"ref": "ws/job"})
	if isErr || !strings.Contains(text, "ws/job/out.png") {
		t.Fatalf("files_list: %v %s", isErr, text)
	}
	text, isErr = call("desktop__files_stat", map[string]any{"ref": "ws/job/out.png", "sha256": true})
	if isErr || !strings.Contains(text, digest([]byte("png"))) {
		t.Fatalf("files_stat: %v %s", isErr, text)
	}
	denied, err := m.agent.CallTool(t.Context(), &mcp.CallToolParams{Name: "desktop__files_stat", Arguments: map[string]any{"ref": "ws/private/secret.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	decision, ok := denied.StructuredContent.(map[string]any)
	if !denied.IsError || !ok || decision["code"] != "no_matching_grant" {
		t.Fatalf("ungranted files_stat decision = %#v, error=%v", denied.StructuredContent, denied.IsError)
	}
	if _, isErr = call("desktop__files_mkdir", map[string]any{"ref": "ws/new/dir"}); isErr {
		t.Fatal("files_mkdir failed")
	}
	if _, isErr = call("desktop__files_mkdir", map[string]any{"ref": "artifacts/x"}); !isErr {
		t.Fatal("files_mkdir under artifacts succeeded")
	}
	if _, isErr = call("desktop__files_delete", map[string]any{"ref": "ws"}); !isErr {
		t.Fatal("deleting the ws root succeeded")
	}
	if _, isErr = call("desktop__files_delete", map[string]any{"ref": "ws/job"}); isErr {
		t.Fatal("files_delete failed")
	}
	if _, err := readWS(t, m.desktop, "ws/job/out.png"); err == nil {
		t.Fatal("file still there after files_delete")
	}
}

func TestFileEndpointsRefuseUnpairedCallers(t *testing.T) {
	desktop := startFileNode(t, "desktop")
	stranger := startFileNode(t, "stranger")
	writeWS(t, desktop, "ws/job/secret.txt", []byte("secret"))
	base := "https://" + desktop.MeshAddr()

	// A device with a certificate that is not paired is answered 403 on every verb.
	client := &http.Client{Transport: stranger.pinnedTransport(desktop.ID())}
	for _, tc := range []struct{ method, path, body string }{
		{"GET", "/v1/files/ws/job/secret.txt", ""},
		{"GET", "/v1/files?ref=ws/job", ""},
		{"GET", "/v1/files", ""},
		{"PUT", "/v1/files/ws/job/evil.txt", "evil"},
		{"POST", "/v1/files/ws/job/dir", ""},
	} {
		req, _ := http.NewRequestWithContext(t.Context(), tc.method, base+tc.path, strings.NewReader(tc.body))
		req.Header.Set(files.HeaderSize, "4")
		req.Header.Set(files.HeaderSHA256, digest([]byte("evil")))
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden || bytes.Contains(body, []byte("secret")) {
			t.Errorf("%s %s by an unpaired device: %s %q", tc.method, tc.path, resp.Status, body)
		}
	}
	if _, err := readWS(t, desktop, "ws/job/evil.txt"); err == nil {
		t.Fatal("an unpaired device wrote a file")
	}

	// A client with no certificate at all never gets past the handshake.
	anon := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	if resp, err := anon.Get(base + "/v1/files/ws/job/secret.txt"); err == nil {
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("anonymous client read a file: %s", resp.Status)
		}
	}
}
