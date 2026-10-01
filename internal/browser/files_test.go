package browser

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// writeWS creates <state>/ws/<name> (name may contain slashes).
func writeWS(t *testing.T, root, name, content string) string {
	t.Helper()
	p := filepath.Join(root, "ws", filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func gotoPage(t *testing.T, n *node, url, title string) {
	t.Helper()
	n.env.fb.addSite(url, title)
	n.allowOnce()
	if res := n.call("browser_navigate", map[string]any{"url": url}); isErr(res) {
		t.Fatalf("navigate %s: %s", url, resultText(res))
	}
}

func TestScreenshotIsSavedAsArtifactAndSmallOnesInline(t *testing.T) {
	n := newNode(t, baseConfig())
	gotoPage(t, n, "http://a.test/", "A")
	res := n.call("browser_take_screenshot", map[string]any{})
	if isErr(res) {
		t.Fatal(resultText(res))
	}
	txt := resultText(res)
	i := strings.Index(txt, "artifacts/browser/")
	if i < 0 {
		t.Fatalf("no artifact ref in %q", txt)
	}
	ref := txt[i:]
	if j := strings.IndexAny(ref, ")\n "); j >= 0 {
		ref = ref[:j]
	}
	if !strings.HasSuffix(ref, ".png") {
		t.Errorf("ref = %q", ref)
	}
	data, err := os.ReadFile(filepath.Join(n.env.paths.Root, filepath.FromSlash(ref)))
	if err != nil || !bytes.HasPrefix(data, []byte("\x89PNG")) || len(data) != 2000 {
		t.Errorf("artifact: %v, %d bytes", err, len(data))
	}
	var inline int
	for _, c := range res.Content {
		if img, ok := c.(*mcp.ImageContent); ok {
			inline++
			if len(img.Data) != 2000 || img.MIMEType != "image/png" {
				t.Errorf("inline image: %d bytes %s", len(img.Data), img.MIMEType)
			}
		}
	}
	if inline != 1 {
		t.Errorf("small screenshot should also be inline, got %d images", inline)
	}
	if strings.Contains(txt, "out"+string(filepath.Separator)) || strings.Contains(txt, "Ran Playwright") {
		t.Errorf("the browser's own path leaked: %s", txt)
	}
	// The browser's copy is gone; only the artifact remains.
	if left, _ := filepath.Glob(filepath.Join(n.env.p.up.outDir(), "*")); len(left) != 0 {
		t.Errorf("output folder still holds %v", left)
	}

	// Big images are artifacts only.
	n.env.fb.pngSize = MaxInlineImage + 1
	res = n.call("browser_take_screenshot", map[string]any{})
	if isErr(res) {
		t.Fatal(resultText(res))
	}
	for _, c := range res.Content {
		if _, ok := c.(*mcp.ImageContent); ok {
			t.Error("an oversized image was returned inline")
		}
	}
	if !strings.Contains(resultText(res), "artifacts/browser/") {
		t.Errorf("oversized screenshot has no ref: %s", resultText(res))
	}
	if m, _ := filepath.Glob(filepath.Join(n.env.paths.Root, "artifacts", "browser", "*.png")); len(m) != 2 {
		t.Errorf("artifacts = %v", m)
	}
}

func TestPDFIsSavedAsArtifact(t *testing.T) {
	n := newNode(t, baseConfig())
	gotoPage(t, n, "http://a.test/", "A")
	res := n.call("browser_pdf_save", map[string]any{})
	txt := resultText(res)
	i := strings.Index(txt, "artifacts/browser/")
	if isErr(res) || i < 0 || !strings.Contains(txt, ".pdf") {
		t.Fatalf("pdf result: %s", txt)
	}
	m, _ := filepath.Glob(filepath.Join(n.env.paths.Root, "artifacts", "browser", "*.pdf"))
	if len(m) != 1 {
		t.Fatalf("pdf artifacts = %v", m)
	}
	if b, _ := os.ReadFile(m[0]); string(b) != "%PDF-1.4 fake" {
		t.Errorf("pdf content = %q", b)
	}
}

func TestFilenameParameterIsRejected(t *testing.T) {
	n := newNode(t, baseConfig())
	gotoPage(t, n, "http://a.test/", "A")
	for _, tool := range []string{"browser_take_screenshot", "browser_snapshot", "browser_pdf_save"} {
		if res := n.call(tool, map[string]any{"filename": "../../outside.png"}); !isErr(res) {
			t.Errorf("%s accepted a filename", tool)
		}
	}
}

func TestUploadTranslatesRefsAndStagesACopy(t *testing.T) {
	n := newNode(t, baseConfig())
	gotoPage(t, n, "http://a.test/", "A")
	writeWS(t, n.env.paths.Root, "docs/report.txt", "quarterly numbers")
	n.allowOnce()
	res := n.call("browser_file_upload", map[string]any{"paths": []string{"ws/docs/report.txt"}})
	if isErr(res) {
		t.Fatal(resultText(res))
	}
	fb := n.env.fb
	if len(fb.uploads) != 1 {
		t.Fatalf("uploads = %v", fb.uploads)
	}
	host := fb.uploads[0]
	if fb.uploadTxt[0] != "quarterly numbers" {
		t.Errorf("the browser read %q", fb.uploadTxt[0])
	}
	// The browser saw a copy inside its own workspace, not the shared file.
	work := n.env.p.up.workDir()
	if rel, err := filepath.Rel(work, host); err != nil || strings.HasPrefix(rel, "..") || filepath.Base(host) != "report.txt" {
		t.Errorf("staged path %s is not inside %s", host, work)
	}
	if strings.Contains(resultText(res), n.env.paths.Root) {
		t.Errorf("host path leaked: %s", resultText(res))
	}
	// The staged copy is removed afterwards.
	if left, _ := filepath.Glob(filepath.Join(work, "uploads", "*")); len(left) != 0 {
		t.Errorf("staging left %v", left)
	}
	// The shared file is untouched.
	if b, _ := os.ReadFile(filepath.Join(n.env.paths.Root, "ws", "docs", "report.txt")); string(b) != "quarterly numbers" {
		t.Error("source file changed")
	}
}

func TestUploadRefusals(t *testing.T) {
	n := newNode(t, baseConfig())
	gotoPage(t, n, "http://a.test/", "A")
	writeWS(t, n.env.paths.Root, "ok.txt", "x")
	outside := filepath.Join(t.TempDir(), "secret.txt")
	os.WriteFile(outside, []byte("secret"), 0o600)
	n.allowOnce()
	for _, bad := range []string{
		outside,               // absolute host path
		"/etc/passwd",         // absolute
		`C:\Windows\win.ini`,  // Windows absolute
		"ws/../../secret.txt", // escape
		"../secret.txt",       // escape
		"ws/../ok.txt",        // not a clean ref... cleaned, still must not smuggle ..
		"desktop:ws/ok.txt",   // qualified refs belong to mesh_copy
		"ws\\ok.txt",          // backslash
		"ws/missing.txt",      // does not exist
		"ws",                  // a directory
		"identity/key.pem",    // outside the shared roots
		"",                    // empty
	} {
		res := n.call("browser_file_upload", map[string]any{"paths": []string{bad}})
		// "ws/../ok.txt" cleans to "ok.txt" which is not under a root: refused too.
		if !isErr(res) {
			t.Errorf("upload of %q was accepted: %s", bad, resultText(res))
		}
	}
	if len(n.env.fb.uploads) != 0 {
		t.Errorf("a refused upload reached the browser: %v", n.env.fb.uploads)
	}
	// A link inside ws/ is never followed.
	link := filepath.Join(n.env.paths.Root, "ws", "link.txt")
	if err := os.Symlink(outside, link); err == nil {
		if res := n.call("browser_file_upload", map[string]any{"paths": []string{"ws/link.txt"}}); !isErr(res) {
			t.Errorf("a symlink was followed: %s", resultText(res))
		}
	}
	// And the file that is allowed works.
	if res := n.call("browser_file_upload", map[string]any{"paths": []string{"ws/ok.txt"}}); isErr(res) {
		t.Errorf("valid upload: %s", resultText(res))
	}
}

func TestUploadChangedAfterApprovalIsNotSent(t *testing.T) {
	n := newNode(t, baseConfig())
	gotoPage(t, n, "http://a.test/", "A")
	p := writeWS(t, n.env.paths.Root, "f.txt", "original")
	args := mustJSON(map[string]any{"paths": []string{"ws/f.txt"}})
	ap, err := n.env.p.Approval(t.Context(), "browser_file_upload", args, n.caller)
	if err != nil {
		t.Fatal(err)
	}
	_ = ap
	os.WriteFile(p, []byte("swapped after the approval"), 0o600)
	n.denyAll()
	res, _ := n.env.p.Call(t.Context(), "browser_file_upload", args, n.caller)
	if !isErr(res) || len(n.env.fb.uploads) != 0 {
		t.Errorf("a changed file was uploaded: %s", resultText(res))
	}
}
