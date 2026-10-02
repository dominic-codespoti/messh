package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"messh/internal/buildinfo"
)

type releaseFixture struct {
	server *httptest.Server
	client *Client
	pages  [][]githubRelease
	files  map[string][]byte
}

func newFixture(t *testing.T) *releaseFixture {
	t.Helper()
	fixture := &releaseFixture{files: make(map[string][]byte)}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases") {
			var page int
			_, _ = fmt.Sscan(r.URL.Query().Get("page"), &page)
			if page < 1 || page > len(fixture.pages) {
				_, _ = w.Write([]byte("[]"))
				return
			}
			if page < len(fixture.pages) {
				w.Header().Set("Link", fmt.Sprintf(`<%s/repos/test/releases?page=%d>; rel="next"`, fixture.server.URL, page+1))
			}
			_ = json.NewEncoder(w).Encode(fixture.pages[page-1])
			return
		}
		data, ok := fixture.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(data)
	}))
	t.Cleanup(fixture.server.Close)
	fixture.client = &Client{HTTP: fixture.server.Client(), APIBase: fixture.server.URL, Repository: "test"}
	return fixture
}

func checksum(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func archiveBytes(t *testing.T, goos, member, attack string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	if goos == "windows" {
		archive := zip.NewWriter(&buffer)
		name := member
		if attack == "traversal" {
			name = "../escaped"
		}
		if attack == "missing" {
			name = strings.TrimSuffix(member, "messh.exe") + "README.txt"
		}
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetMode(0755)
		if attack == "symlink" {
			header.SetMode(os.ModeSymlink | 0777)
		}
		file, err := archive.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte("new-executable")); err != nil {
			t.Fatal(err)
		}
		if err := archive.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		compressed := gzip.NewWriter(&buffer)
		archive := tar.NewWriter(compressed)
		name := member
		if attack == "traversal" {
			name = "../escaped"
		}
		if attack == "missing" {
			name = strings.TrimSuffix(member, "messh") + "README.txt"
		}
		header := &tar.Header{Name: name, Mode: 0755, Size: int64(len("new-executable")), Typeflag: tar.TypeReg}
		if attack == "symlink" {
			header.Typeflag, header.Linkname, header.Size = tar.TypeSymlink, "outside", 0
		}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if header.Size != 0 {
			if _, err := archive.Write([]byte("new-executable")); err != nil {
				t.Fatal(err)
			}
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

func (f *releaseFixture) release(t *testing.T, build uint64, attack string) githubRelease {
	t.Helper()
	commit := fmt.Sprintf("%040x", build)
	// Give each fixture a distinct twelve-character prefix too.
	commit = fmt.Sprintf("%012x", build) + commit[12:]
	tag := fmt.Sprintf("main-%d-%s", build, commit[:12])
	version := fmt.Sprintf("0.1.0-main.%d+%s", build, commit[:12])
	manifest := Manifest{Schema: 1, Channel: "main", Version: version, Commit: commit, Build: build, Tag: tag}
	release := githubRelease{Tag: tag, Prerelease: true, URL: f.server.URL + "/release/" + tag}
	var sums strings.Builder
	for _, target := range [][2]string{{"windows", "amd64"}, {"linux", "amd64"}, {"linux", "arm64"}} {
		root := "messh-" + version + "-" + target[0] + "-" + target[1]
		ext, exe := ".tar.gz", "messh"
		if target[0] == "windows" {
			ext, exe = ".zip", "messh.exe"
		}
		data := archiveBytes(t, target[0], root+"/"+exe, attack)
		asset := Asset{OS: target[0], Arch: target[1], Name: root + ext, Size: int64(len(data)), SHA256: checksum(data), Executable: root + "/" + exe}
		manifest.Assets = append(manifest.Assets, asset)
		f.files["/"+tag+"/"+asset.Name] = data
		release.Assets = append(release.Assets, githubAsset{Name: asset.Name, Size: asset.Size, URL: f.server.URL + "/" + tag + "/" + asset.Name, State: "uploaded"})
		fmt.Fprintf(&sums, "%s  %s\n", asset.SHA256, asset.Name)
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(&sums, "%s  update.json\n", checksum(data))
	for name, body := range map[string][]byte{"update.json": data, "SHA256SUMS": []byte(sums.String())} {
		f.files["/"+tag+"/"+name] = body
		release.Assets = append(release.Assets, githubAsset{Name: name, Size: int64(len(body)), URL: f.server.URL + "/" + tag + "/" + name, State: "uploaded"})
	}
	return release
}

func installed(t *testing.T) string {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(parent, "messh.exe")
	if err := os.WriteFile(name, []byte("old-executable"), 0700); err != nil {
		t.Fatal(err)
	}
	return name
}

func TestCheckPaginationOrderingAndNoDowngrade(t *testing.T) {
	f := newFixture(t)
	old, newest := f.release(t, 10, ""), f.release(t, 30, "")
	draft := f.release(t, 40, "")
	draft.Draft = true
	f.pages = [][]githubRelease{{old, draft}, {newest}}
	result, err := f.client.Check(context.Background(), buildinfo.Info{Channel: "main", Build: 20}, "linux", "arm64")
	if err != nil {
		t.Fatal(err)
	}
	if !result.UpdateAvailable || result.Available.Manifest.Build != 30 || result.Available.Asset.Arch != "arm64" {
		t.Fatalf("wrong selected release: %+v", result)
	}
	current := buildinfo.Info{Channel: "main", Build: 30, Commit: result.Available.Manifest.Commit, Version: result.Available.Manifest.Version}
	result, err = f.client.Check(context.Background(), current, "windows", "amd64")
	if err != nil || result.UpdateAvailable || result.Reason != "current" {
		t.Fatalf("current release: %+v, %v", result, err)
	}
	current.Build = 31
	result, err = f.client.Check(context.Background(), current, "linux", "amd64")
	if err != nil || result.UpdateAvailable || result.Reason != "newer_installed" {
		t.Fatalf("downgrade: %+v, %v", result, err)
	}
	current.Build, current.Commit = 30, strings.Repeat("f", 40)
	if _, err := f.client.Check(context.Background(), current, "linux", "amd64"); err == nil {
		t.Fatal("accepted conflicting installed build identity")
	}
	f.pages = nil
	result, err = f.client.Check(context.Background(), buildinfo.Info{}, "linux", "amd64")
	if err != nil || result.Available != nil || result.UpdateAvailable {
		t.Fatalf("no release: %+v, %v", result, err)
	}
}

func TestCheckUnpublishedBuildsOfferPublishedIdentity(t *testing.T) {
	f := newFixture(t)
	release := f.release(t, 20, "")
	f.pages = [][]githubRelease{{release}}
	var manifest Manifest
	if err := json.Unmarshal(f.files["/"+release.Tag+"/update.json"], &manifest); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		current buildinfo.Info
	}{
		{
			name:    "development at published commit",
			current: buildinfo.Info{Channel: "development", Commit: manifest.Commit},
		},
		{
			name:    "development with published sequence",
			current: buildinfo.Info{Channel: "development", Commit: manifest.Commit, Build: manifest.Build},
		},
		{
			name:    "tagged channel at published commit",
			current: buildinfo.Info{Channel: "stable", Version: "0.1.0", Commit: manifest.Commit, Build: manifest.Build},
		},
		{
			name:    "other channel with higher sequence",
			current: buildinfo.Info{Channel: "stable", Commit: manifest.Commit, Build: manifest.Build + 1},
		},
		{
			name:    "main without published sequence",
			current: buildinfo.Info{Channel: "main", Commit: manifest.Commit},
		},
		{
			name:    "main with mismatched version",
			current: buildinfo.Info{Channel: "main", Version: "0.1.0-dev", Commit: manifest.Commit, Build: manifest.Build},
		},
		{
			name:    "unknown local revision",
			current: buildinfo.Info{Channel: "development"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			result, err := f.client.Check(context.Background(), test.current, "linux", "amd64")
			if err != nil {
				t.Fatal(err)
			}
			if !result.UpdateAvailable || result.Reason != "unpublished_build" {
				t.Fatalf("unpublished build did not offer installation: %+v", result)
			}
			if result.Current != test.current {
				t.Fatalf("current identity changed: got %+v, want %+v", result.Current, test.current)
			}
			if result.Available == nil || result.Available.Manifest.Build != manifest.Build || result.Available.Manifest.Commit != manifest.Commit {
				t.Fatalf("wrong published identity offered: %+v", result.Available)
			}
		})
	}
}

func TestIncompleteNewestReleaseDoesNotFallBack(t *testing.T) {
	f := newFixture(t)
	newest := f.release(t, 20, "")
	newest.Assets = newest.Assets[:3]
	f.pages = [][]githubRelease{{f.release(t, 10, ""), newest}}
	if _, err := f.client.Check(context.Background(), buildinfo.Info{}, "linux", "amd64"); err == nil {
		t.Fatal("accepted incomplete newest release")
	}
}

func TestManifestIdentityAndChecksumValidation(t *testing.T) {
	for _, mutation := range []string{"checksum", "identity", "size", "origin", "schema"} {
		t.Run(mutation, func(t *testing.T) {
			f := newFixture(t)
			release := f.release(t, 20, "")
			manifestPath := "/" + release.Tag + "/update.json"
			switch mutation {
			case "checksum":
				f.files[manifestPath] = append(f.files[manifestPath], ' ')
			case "identity", "schema":
				var manifest Manifest
				if err := json.Unmarshal(f.files[manifestPath], &manifest); err != nil {
					t.Fatal(err)
				}
				if mutation == "identity" {
					manifest.Commit = strings.Repeat("f", 40)
				} else {
					manifest.Schema = 2
				}
				previous := checksum(f.files[manifestPath])
				data, err := json.Marshal(manifest)
				if err != nil {
					t.Fatal(err)
				}
				f.files[manifestPath] = data
				sumsPath := "/" + release.Tag + "/SHA256SUMS"
				f.files[sumsPath] = []byte(strings.ReplaceAll(string(f.files[sumsPath]), previous, checksum(data)))
				for i := range release.Assets {
					if release.Assets[i].Name == "update.json" {
						release.Assets[i].Size = int64(len(data))
					}
				}
			case "size":
				release.Assets[0].Size++
			case "origin":
				release.Assets[0].URL = "https://untrusted.invalid/archive.zip"
			}
			f.pages = [][]githubRelease{{release}}
			if _, err := f.client.Check(context.Background(), buildinfo.Info{}, "linux", "amd64"); err == nil {
				t.Fatal("accepted invalid release")
			}
		})
	}
}

func TestStageRejectsCorruptionAndUnsafeArchives(t *testing.T) {
	for _, goos := range []string{"windows", "linux"} {
		for _, failure := range []string{"corrupt", "truncated", "oversize", "traversal", "symlink", "missing"} {
			t.Run(goos+"/"+failure, func(t *testing.T) {
				f := newFixture(t)
				f.pages = [][]githubRelease{{f.release(t, 20, failure)}}
				result, err := f.client.Check(context.Background(), buildinfo.Info{}, goos, "amd64")
				if err != nil {
					t.Fatal(err)
				}
				assetPath := "/" + result.Available.Manifest.Tag + "/" + result.Available.Asset.Name
				switch failure {
				case "corrupt":
					f.files[assetPath][0] ^= 0xff
				case "truncated":
					f.files[assetPath] = f.files[assetPath][:10]
				case "oversize":
					f.files[assetPath] = append(f.files[assetPath], 'x')
				}
				exe := installed(t)
				if stage, err := f.client.DownloadStage(context.Background(), *result.Available, exe); err == nil {
					stage.Close()
					t.Fatal("accepted invalid archive")
				}
				original, err := os.ReadFile(exe)
				if err != nil || string(original) != "old-executable" {
					t.Fatalf("modified installed executable: %q, %v", original, err)
				}
				entries, err := os.ReadDir(filepath.Dir(exe))
				if err != nil {
					t.Fatal(err)
				}
				for _, entry := range entries {
					if strings.HasPrefix(entry.Name(), ".messh-update-") {
						t.Fatal("abandoned stage survived failure")
					}
				}
			})
		}
	}
}

func TestStageSuccessAndClose(t *testing.T) {
	for _, goos := range []string{"windows", "linux"} {
		t.Run(goos, func(t *testing.T) {
			f := newFixture(t)
			f.pages = [][]githubRelease{{f.release(t, 20, "")}}
			result, err := f.client.Check(context.Background(), buildinfo.Info{}, goos, "amd64")
			if err != nil {
				t.Fatal(err)
			}
			exe := installed(t)
			stage, err := f.client.DownloadStage(context.Background(), *result.Available, exe)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(stage.Path)
			if err != nil || string(data) != "new-executable" {
				t.Fatalf("wrong staged executable: %q, %v", data, err)
			}
			dir := filepath.Dir(stage.Path)
			if err := stage.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(dir); !os.IsNotExist(err) {
				t.Fatalf("stage not removed: %v", err)
			}
			data, err = os.ReadFile(exe)
			if err != nil || string(data) != "old-executable" {
				t.Fatalf("staging changed installation: %q, %v", data, err)
			}
		})
	}
}
