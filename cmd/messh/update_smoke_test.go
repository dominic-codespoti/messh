package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"messh/internal/buildinfo"
	"messh/internal/control"
	"messh/internal/state"
	"messh/internal/update"
)

// The copied test executable supplies a real, isolated node process. Its image
// trailer gives old/new images different embedded identities without a nested
// Go build. Only these exact invocations, with the private test env, intercept
// the testing runtime. No installed service or user's state is touched.
func init() {
	if os.Getenv("MESSH_UPDATE_TEST_PROCESS") != "1" {
		return
	}
	if len(os.Args) != 3 || os.Args[1] != "version" || os.Args[2] != "--json" {
		if len(os.Args) != 4 || os.Args[1] != "node" || os.Args[2] != "--state" {
			return
		}
	}
	exe, err := os.Executable()
	if err != nil {
		os.Exit(91)
	}
	image, err := os.ReadFile(exe)
	if err != nil {
		os.Exit(92)
	}
	kind := "old"
	for _, name := range []string{"new", "malformed", "overflow", "hang", "unhealthy", "crash"} {
		if bytesHaveUpdateImage(image, name) {
			kind = name
		}
	}
	identity := updateTestBuild(kind == "new" || kind == "unhealthy" || kind == "crash")
	if os.Args[1] == "version" {
		switch kind {
		case "malformed":
			fmt.Print(`{"version":`)
		case "overflow":
			fmt.Print(strings.Repeat("x", (64<<10)+1))
		case "hang":
			time.Sleep(time.Minute)
		default:
			_ = json.NewEncoder(os.Stdout).Encode(identity)
		}
		os.Exit(0)
	}
	if kind == "crash" {
		os.Exit(95)
	}
	if kind == "unhealthy" {
		identity = updateTestBuild(false)
	}
	if err := serveUpdateTestNode(state.Paths{Root: os.Args[3]}, exe, identity); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(94)
	}
	os.Exit(0)
}

func updateTestBuild(new bool) buildinfo.Info {
	if !new {
		return buildinfo.Info{Version: "0.1.0-dev", Channel: "development"}
	}
	commit := strings.Repeat("a", 40)
	return buildinfo.Info{Version: "0.1.0-main.2+" + commit[:12], Commit: commit, Channel: "main", Build: 2}
}

func serveUpdateTestNode(paths state.Paths, exe string, identity buildinfo.Info) error {
	const id = "update-test-existing-identity"
	gate, err := paths.LoadUpdateStartup()
	if err != nil {
		return err
	}
	if gate != nil {
		if err := gate.Validate(id, exe, time.Now()); err != nil {
			return err
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	defer listener.Close()
	run := state.RunInfo{PID: os.Getpid(), ID: id, Local: listener.Addr().String(), Started: time.Now().UTC(), Executable: exe}
	status := control.Status{ID: id, Local: run.Local, Started: run.Started, Version: identity.Version, Build: identity}
	token, err := os.ReadFile(paths.ControlTokenFile())
	if err != nil {
		return err
	}
	var mu sync.Mutex
	var once sync.Once
	stop := make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+strings.TrimSpace(string(token)) {
			http.Error(w, "unauthorized", 401)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.URL.Path == "/v1/status":
			_ = json.NewEncoder(w).Encode(status)
		case r.URL.Path == "/v1/update/prepare" && r.Method == http.MethodPost:
			if _, err := os.Stat(filepath.Join(paths.Root, "busy-other-owner")); err == nil {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(control.Error{Error: "node is busy: another owner's nonterminal job"})
				return
			}
			if gate != nil {
				http.Error(w, "already gated", 409)
				return
			}
			gate = &state.UpdateStartup{Lease: strings.Repeat("a", 64), ID: id, Executable: exe, Expires: time.Now().Add(2 * time.Minute)}
			_ = json.NewEncoder(w).Encode(gate)
		case r.URL.Path == "/v1/update/prepare" && r.Method == http.MethodDelete || r.URL.Path == "/v1/update/stop":
			var request control.UpdateLeaseRequest
			if json.NewDecoder(r.Body).Decode(&request) != nil || gate == nil || request.Lease != gate.Lease {
				http.Error(w, "invalid lease", 409)
				return
			}
			if r.URL.Path == "/v1/update/stop" {
				w.Header().Set("Content-Length", "0")
				w.WriteHeader(http.StatusAccepted)
				_ = http.NewResponseController(w).Flush()
				once.Do(func() { close(stop) })
			} else {
				if err := paths.RemoveUpdateStartup(*gate); err != nil {
					http.Error(w, err.Error(), 409)
					return
				}
				gate = nil
				w.WriteHeader(http.StatusNoContent)
			}
		case r.URL.Path == "/work":
			if gate != nil {
				http.Error(w, "gated", 503)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	})}
	if err := paths.SaveRunInfo(run); err != nil {
		return err
	}
	defer paths.RemoveRunInfo()
	go func() {
		<-stop
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	err = server.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// This launcher owns only subprocesses it created in a temporary directory.
// Production uses updatehost.Discover; both use the same CLI transaction, real
// control requests, persisted gate, stage verification, and filesystem swap.
type updateTestManager struct {
	paths              state.Paths
	exe                string
	cmd                *exec.Cmd
	done               chan struct{}
	exitErr            error
	failCandidateStart bool
	loseRecovery       bool
	failStop           bool
}

func (m *updateTestManager) launch(ctx context.Context) error {
	m.cmd = exec.Command(m.exe, "node", "--state", m.paths.Root)
	m.cmd.Env = append(os.Environ(), "MESSH_UPDATE_TEST_PROCESS=1")
	m.cmd.Stdout, m.cmd.Stderr = io.Discard, io.Discard
	if err := m.cmd.Start(); err != nil {
		return err
	}
	m.done = make(chan struct{})
	cmd, done := m.cmd, m.done
	go func() {
		m.exitErr = cmd.Wait()
		close(done)
	}()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-m.done:
			return fmt.Errorf("helper node exited before readiness: %v", m.exitErr)
		default:
		}
		run, err := m.paths.LoadRunInfo()
		if err == nil && run.PID == m.cmd.Process.Pid {
			return nil
		}
		select {
		case <-m.done:
			return fmt.Errorf("helper node exited before readiness: %v", m.exitErr)
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (m *updateTestManager) Start(ctx context.Context) error {
	gate, err := m.paths.LoadUpdateStartup()
	if err != nil {
		return err
	}
	if gate == nil || time.Until(gate.Expires) <= 45*time.Second {
		return errors.New("missing or expiring startup gate")
	}
	if err := m.launch(ctx); err != nil {
		return err
	}
	if m.failCandidateStart {
		image, err := os.ReadFile(m.exe)
		if err != nil {
			return err
		}
		if bytesHaveUpdateImage(image, "new") {
			return errors.New("launcher returned an ambiguous error after candidate start")
		}
	}
	return nil
}

func (m *updateTestManager) Stop(ctx context.Context, lease string) error {
	if m.failStop {
		return errors.New("launcher refused before stopping")
	}
	n, err := readUpdateNode(ctx, m.paths)
	if err != nil {
		return err
	}
	if n == nil {
		return errors.New("missing helper node")
	}
	if err := n.client.StopUpdate(ctx, lease); err != nil {
		return err
	}
	select {
	case <-m.done:
		return m.exitErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *updateTestManager) RecoverStop(ctx context.Context, lease string) error {
	if m.loseRecovery {
		return errors.New("candidate launcher ownership changed")
	}
	gate, err := m.paths.LoadUpdateStartup()
	if err != nil {
		return err
	}
	if gate == nil || gate.Lease != lease || time.Until(gate.Expires) <= 45*time.Second {
		return errors.New("recovery gate lost")
	}
	select {
	case <-m.done:
		return nil
	default:
	}
	return m.Stop(ctx, lease)
}

func (m *updateTestManager) discover(_ context.Context, exe string, paths state.Paths, run state.RunInfo) (updateManager, error) {
	if m.cmd == nil || m.cmd.Process.Pid != run.PID || paths != m.paths || !sameUpdatePath(exe, m.exe) {
		return nil, errors.New("recorded process is not owned by the isolated launcher")
	}
	return m, nil
}

func bytesHaveUpdateImage(image []byte, kind string) bool {
	return strings.HasSuffix(string(image[max(0, len(image)-80):]), "\nMESSH_UPDATE_IMAGE="+kind+"\n")
}

func updateTestImage(t *testing.T, kind string) []byte {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	return append(image, []byte("\nMESSH_UPDATE_IMAGE="+kind+"\n")...)
}

func updateTestArchive(t *testing.T, goos, member string, image []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if goos == "windows" {
		archive := zip.NewWriter(&buffer)
		header := &zip.FileHeader{Name: member, Method: zip.Store}
		header.SetMode(0o755)
		file, err := archive.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(image); err != nil {
			t.Fatal(err)
		}
		if err := archive.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		compressed, err := gzip.NewWriterLevel(&buffer, gzip.BestSpeed)
		if err != nil {
			t.Fatal(err)
		}
		archive := tar.NewWriter(compressed)
		header := &tar.Header{Name: member, Mode: 0o755, Size: int64(len(image)), Typeflag: tar.TypeReg}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write(image); err != nil {
			t.Fatal(err)
		}
		if err := archive.Close(); err != nil {
			t.Fatal(err)
		}
		if err := compressed.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buffer.Bytes()
}

func updateTestClient(t *testing.T, image []byte, fault string) *update.Client {
	t.Helper()
	type publishedAsset struct {
		Name  string `json:"name"`
		Size  int64  `json:"size"`
		URL   string `json:"browser_download_url"`
		State string `json:"state"`
	}
	type publishedRelease struct {
		Tag        string           `json:"tag_name"`
		Prerelease bool             `json:"prerelease"`
		URL        string           `json:"html_url"`
		Assets     []publishedAsset `json:"assets"`
	}
	files := make(map[string][]byte)
	releases := []publishedRelease{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/test/releases" {
			_ = json.NewEncoder(w).Encode(releases)
			return
		}
		if data, ok := files[r.URL.Path]; ok {
			_, _ = w.Write(data)
			return
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)
	client := &update.Client{HTTP: server.Client(), APIBase: server.URL, Repository: "test"}
	if fault == "no_release" {
		return client
	}
	wanted := updateTestBuild(true)
	tag := fmt.Sprintf("main-%d-%s", wanted.Build, wanted.Commit[:12])
	manifest := update.Manifest{Schema: 1, Channel: wanted.Channel, Version: wanted.Version, Commit: wanted.Commit, Build: wanted.Build, Tag: tag}
	release := publishedRelease{Tag: tag, Prerelease: true, URL: server.URL + "/release/" + tag}
	add := func(name string, body []byte) {
		files["/"+name] = body
		release.Assets = append(release.Assets, publishedAsset{Name: name, Size: int64(len(body)), URL: server.URL + "/" + name, State: "uploaded"})
	}
	var sums strings.Builder
	for _, target := range [][2]string{{"windows", "amd64"}, {"linux", "amd64"}, {"linux", "arm64"}} {
		root := "messh-" + wanted.Version + "-" + target[0] + "-" + target[1]
		ext, executable := ".tar.gz", "messh"
		if target[0] == "windows" {
			ext, executable = ".zip", "messh.exe"
		}
		body := []byte("unselected platform archive")
		if target[0] == runtime.GOOS && target[1] == runtime.GOARCH {
			body = updateTestArchive(t, target[0], root+"/"+executable, image)
		}
		asset := update.Asset{OS: target[0], Arch: target[1], Name: root + ext, Executable: root + "/" + executable, Size: int64(len(body)), SHA256: fmt.Sprintf("%x", sha256.Sum256(body))}
		manifest.Assets = append(manifest.Assets, asset)
		add(asset.Name, body)
		fmt.Fprintf(&sums, "%s  %s\n", asset.SHA256, asset.Name)
		if fault == "checksum" && target[0] == runtime.GOOS && target[1] == runtime.GOARCH {
			files["/"+asset.Name] = []byte("corrupted archive")
		}
	}
	if fault == "manifest" {
		manifest.Schema = 0
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&sums, "%x  update.json\n", sha256.Sum256(body))
	add("update.json", body)
	add("SHA256SUMS", []byte(sums.String()))
	releases = append(releases, release)
	return client
}

func updateTestInstallation(t *testing.T, kind string, running bool) (*updateTestManager, map[string][]byte) {
	t.Helper()
	if runtime.GOOS != "windows" && runtime.GOOS != "linux" {
		t.Skip("main updater supports Windows and Linux")
	}
	t.Setenv("MESSH_UPDATE_TEST_PROCESS", "1")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	manager := &updateTestManager{exe: filepath.Join(root, "messh"), paths: state.Paths{Root: filepath.Join(root, "state")}}
	if runtime.GOOS == "windows" {
		manager.exe += ".exe"
	}
	if err := os.WriteFile(manager.exe, updateTestImage(t, kind), 0o700); err != nil {
		t.Fatal(err)
	}
	preserved := make(map[string][]byte)
	if running {
		preserved[manager.paths.ControlTokenFile()] = []byte("existing-private-control-token")
		preserved[manager.paths.ConfigFile()] = []byte(`{"name":"existing node"}`)
		preserved[filepath.Join(manager.paths.IdentityDir(), "sentinel")] = []byte("existing identity material")
		preserved[filepath.Join(manager.paths.JobsDir(), "completed-job")] = []byte("completed work")
		for path, data := range preserved {
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	t.Cleanup(func() {
		if manager.cmd == nil {
			return
		}
		select {
		case <-manager.done:
			return
		default:
		}
		// Test cleanup owns this exact subprocess; transaction recovery never
		// uses process killing or bypasses the inherited gate.
		_ = manager.cmd.Process.Kill()
		select {
		case <-manager.done:
		case <-time.After(5 * time.Second):
			t.Error("isolated helper did not exit during cleanup")
		}
	})
	if running {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := manager.launch(ctx); err != nil {
			t.Fatal(err)
		}
	}
	return manager, preserved
}

func assertUpdatePreserved(t *testing.T, preserved map[string][]byte) {
	t.Helper()
	for path, wanted := range preserved {
		actual, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(actual, wanted) {
			t.Fatalf("state changed at %s: %v", path, err)
		}
	}
}

func assertUpdateNode(t *testing.T, manager *updateTestManager, wanted buildinfo.Info) *updateNode {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	node, err := readUpdateNode(ctx, manager.paths)
	if err != nil {
		t.Fatal(err)
	}
	if node == nil || node.status.Build != wanted || node.run.ID != "update-test-existing-identity" || node.run.PID != manager.cmd.Process.Pid {
		t.Fatalf("unexpected live node: %+v", node)
	}
	if err := node.client.Do(ctx, http.MethodPost, "/work", nil, nil); err != nil {
		t.Fatalf("node admission did not reopen: %v", err)
	}
	return node
}

func assertUpdateNoGate(t *testing.T, paths state.Paths) {
	t.Helper()
	gate, err := paths.LoadUpdateStartup()
	if err != nil || gate != nil {
		t.Fatalf("startup gate was not released: %+v, %v", gate, err)
	}
}

func TestUpdateSamePathAndPreservedState(t *testing.T) {
	manager, preserved := updateTestInstallation(t, "old", true)
	old := assertUpdateNode(t, manager, updateTestBuild(false))
	oldImage, err := os.ReadFile(manager.exe)
	if err != nil {
		t.Fatal(err)
	}
	client := updateTestClient(t, updateTestImage(t, "new"), "")
	result, err := installUpdate(context.Background(), manager.paths, manager.exe, old.status.Build, updateDependencies{client: client, discover: manager.discover})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Updated || !result.Restarted || result.Executable != manager.exe || result.New != updateTestBuild(true) {
		t.Fatalf("wrong update result: %+v", result)
	}
	newNode := assertUpdateNode(t, manager, updateTestBuild(true))
	if newNode.run.PID == old.run.PID || !newNode.run.Started.After(old.run.Started) {
		t.Fatal("old process was not replaced")
	}
	if err := verifyUpdateExecutable(context.Background(), manager.exe, updateTestBuild(true)); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(result.Backup)
	if err != nil || !bytes.Equal(backup, oldImage) {
		t.Fatalf("original executable was not retained: %v", err)
	}
	assertUpdatePreserved(t, preserved)
	assertUpdateNoGate(t, manager.paths)
}

func TestUpdateRefusalPreservesLiveNode(t *testing.T) {
	for _, fault := range []string{"mismatch", "malformed", "overflow", "checksum", "manifest", "busy", "unmanaged", "locked", "stop"} {
		t.Run(fault, func(t *testing.T) {
			manager, preserved := updateTestInstallation(t, "old", true)
			old := assertUpdateNode(t, manager, updateTestBuild(false))
			oldImage, err := os.ReadFile(manager.exe)
			if err != nil {
				t.Fatal(err)
			}
			kind := "new"
			if fault == "mismatch" {
				kind = "old"
			}
			if fault == "malformed" || fault == "overflow" {
				kind = fault
			}
			client := updateTestClient(t, updateTestImage(t, kind), fault)
			deps := updateDependencies{client: client, discover: manager.discover}
			switch fault {
			case "busy":
				if err := os.WriteFile(filepath.Join(manager.paths.Root, "busy-other-owner"), []byte("active"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "unmanaged":
				deps.discover = func(context.Context, string, state.Paths, state.RunInfo) (updateManager, error) {
					return nil, errors.New("not owned by a supported launcher")
				}
			case "locked":
				unlock, err := update.AcquireLock(manager.exe)
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
			case "stop":
				manager.failStop = true
			}
			result, err := installUpdate(context.Background(), manager.paths, manager.exe, old.status.Build, deps)
			if err == nil || result.Updated || result.Restarted {
				t.Fatalf("unsafe update succeeded: %+v, %v", result, err)
			}
			node := assertUpdateNode(t, manager, old.status.Build)
			if node.run != old.run {
				t.Fatal("refused update changed the original process")
			}
			actual, err := os.ReadFile(manager.exe)
			if err != nil || !bytes.Equal(actual, oldImage) {
				t.Fatalf("refused update changed the executable: %v", err)
			}
			if _, err := os.Stat(manager.exe + ".previous"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("refused update changed the backup: %v", err)
			}
			assertUpdatePreserved(t, preserved)
			assertUpdateNoGate(t, manager.paths)
		})
	}
}

func TestUpdateRestoresHealthyOriginal(t *testing.T) {
	for _, fault := range []string{"ambiguous_start", "crash", "unhealthy"} {
		t.Run(fault, func(t *testing.T) {
			manager, preserved := updateTestInstallation(t, "old", true)
			old := assertUpdateNode(t, manager, updateTestBuild(false))
			oldImage, err := os.ReadFile(manager.exe)
			if err != nil {
				t.Fatal(err)
			}
			kind := fault
			if fault == "ambiguous_start" {
				kind, manager.failCandidateStart = "new", true
			}
			client := updateTestClient(t, updateTestImage(t, kind), "")
			result, err := installUpdate(context.Background(), manager.paths, manager.exe, old.status.Build, updateDependencies{client: client, discover: manager.discover})
			if err == nil || result.Updated || result.Restarted {
				t.Fatalf("failed candidate was reported successful: %+v, %v", result, err)
			}
			restored := assertUpdateNode(t, manager, old.status.Build)
			if restored.run.PID == old.run.PID || !restored.run.Started.After(old.run.Started) {
				t.Fatal("original node was not actually restarted")
			}
			actual, err := os.ReadFile(manager.exe)
			if err != nil || !bytes.Equal(actual, oldImage) {
				t.Fatalf("original executable was not restored: %v", err)
			}
			assertUpdatePreserved(t, preserved)
			assertUpdateNoGate(t, manager.paths)
		})
	}
}

func TestUpdateLostRecoveryOwnershipKeepsCandidateGated(t *testing.T) {
	manager, preserved := updateTestInstallation(t, "old", true)
	manager.failCandidateStart, manager.loseRecovery = true, true
	client := updateTestClient(t, updateTestImage(t, "new"), "")
	result, err := installUpdate(context.Background(), manager.paths, manager.exe, updateTestBuild(false), updateDependencies{client: client, discover: manager.discover})
	if err == nil || result.Updated || result.Restarted {
		t.Fatalf("incomplete recovery was reported successful: %+v, %v", result, err)
	}
	gate, gateErr := manager.paths.LoadUpdateStartup()
	if gateErr != nil || gate == nil {
		t.Fatalf("unverified candidate lost its gate: %+v, %v", gate, gateErr)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	node, nodeErr := readUpdateNode(ctx, manager.paths)
	if nodeErr != nil || node == nil || node.status.Build != updateTestBuild(true) {
		t.Fatalf("candidate was changed without ownership: %+v, %v", node, nodeErr)
	}
	if err := node.client.Do(ctx, http.MethodPost, "/work", nil, nil); err == nil {
		t.Fatal("unverified candidate accepted work")
	}
	if err := verifyUpdateExecutable(ctx, manager.exe, updateTestBuild(true)); err != nil {
		t.Fatal(err)
	}
	if err := verifyUpdateExecutable(ctx, manager.exe+".previous", updateTestBuild(false)); err != nil {
		t.Fatal(err)
	}
	assertUpdatePreserved(t, preserved)
}

func TestUpdateWithoutNodeDoesNotCreateState(t *testing.T) {
	manager, _ := updateTestInstallation(t, "old", false)
	client := updateTestClient(t, updateTestImage(t, "new"), "")
	result, err := installUpdate(context.Background(), manager.paths, manager.exe, updateTestBuild(false), updateDependencies{client: client, discover: manager.discover})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Updated || result.Restarted || result.New != updateTestBuild(true) {
		t.Fatalf("wrong CLI-only update: %+v", result)
	}
	if _, err := os.Stat(manager.paths.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("CLI-only update created state: %v", err)
	}
	if err := verifyUpdateExecutable(context.Background(), manager.exe, updateTestBuild(true)); err != nil {
		t.Fatal(err)
	}
	if err := verifyUpdateExecutable(context.Background(), result.Backup, updateTestBuild(false)); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateNoOpKeepsNodeAndExecutable(t *testing.T) {
	for _, fault := range []string{"current", "no_release"} {
		t.Run(fault, func(t *testing.T) {
			manager, preserved := updateTestInstallation(t, "new", true)
			old := assertUpdateNode(t, manager, updateTestBuild(true))
			client := updateTestClient(t, []byte("must not execute a no-op candidate"), fault)
			result, err := installUpdate(context.Background(), manager.paths, manager.exe, old.status.Build, updateDependencies{client: client, discover: manager.discover})
			if err != nil || result.Updated || result.Restarted || result.New != old.status.Build {
				t.Fatalf("no-op changed the installation: %+v, %v", result, err)
			}
			if node := assertUpdateNode(t, manager, old.status.Build); node.run != old.run {
				t.Fatal("no-op restarted the node")
			}
			if err := verifyUpdateExecutable(context.Background(), manager.exe, old.status.Build); err != nil {
				t.Fatal(err)
			}
			assertUpdatePreserved(t, preserved)
			assertUpdateNoGate(t, manager.paths)
		})
	}
}

func TestUpdateCheckDoesNotCreateSelectedState(t *testing.T) {
	manager, _ := updateTestInstallation(t, "old", false)
	client := updateTestClient(t, []byte("check must not run or stage this candidate"), "")
	command := registry["update"]
	flags := flagSet(command)
	if err := flags.Parse([]string{"--check"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	c := &Context{Cmd: command, Flags: flags, JSON: true, Stdout: &out, state: manager.paths.Root}
	if err := runUpdateWithDependencies(c, updateDependencies{client: client}); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Available *update.Release `json:"available"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Available == nil || manifestBuild(result.Available.Manifest) != updateTestBuild(true) {
		t.Fatalf("published release not reported: %s", out.Bytes())
	}
	if _, err := os.Stat(manager.paths.Root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("check created state: %v", err)
	}
}

func TestUpdateCheckLeavesLiveNodeUntouched(t *testing.T) {
	manager, preserved := updateTestInstallation(t, "old", true)
	old := assertUpdateNode(t, manager, updateTestBuild(false))
	client := updateTestClient(t, nil, "no_release")
	command := registry["update"]
	flags := flagSet(command)
	if err := flags.Parse([]string{"--check"}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	c := &Context{Cmd: command, Flags: flags, JSON: true, Stdout: &out, state: manager.paths.Root}
	if err := runUpdateWithDependencies(c, updateDependencies{client: client}); err != nil {
		t.Fatal(err)
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if value, ok := result["available"]; !ok || string(value) != "null" {
		t.Fatalf("no-release availability is not explicit: %s", out.Bytes())
	}
	if string(result["update_available"]) != "false" {
		t.Fatalf("no release was incorrectly offered: %s", out.Bytes())
	}
	if node := assertUpdateNode(t, manager, old.status.Build); node.run != old.run {
		t.Fatal("read-only check changed the running node")
	}
	assertUpdatePreserved(t, preserved)
	assertUpdateNoGate(t, manager.paths)
}

func TestUpdateVersionVerificationHonorsCancellation(t *testing.T) {
	manager, _ := updateTestInstallation(t, "hang", false)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	if err := verifyUpdateExecutable(ctx, manager.exe, updateTestBuild(true)); err == nil || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("hung executable was not canceled: %v, %v", err, ctx.Err())
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("identity verification ignored the caller's deadline")
	}
}
