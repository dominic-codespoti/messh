package browser

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/files"
	"messh/internal/state"
)

// Result size limits.
const (
	// MaxInlineImage is the largest image returned inside the MCP reply so the
	// model can look at it; larger ones are only saved as artifacts.
	MaxInlineImage = 512 << 10
	maxCapture     = 64 << 20 // largest file taken from the browser's output folder
	maxUploadSize  = 256 << 20
)

// splitSections cuts a Playwright result into its preamble and "### " sections.
func splitSections(text string) (pre string, secs []resultSection) {
	lines := strings.Split(text, "\n")
	var cur *resultSection
	var preLines []string
	for _, ln := range lines {
		if strings.HasPrefix(ln, "### ") {
			secs = append(secs, resultSection{title: strings.TrimSpace(ln[4:])})
			cur = &secs[len(secs)-1]
			continue
		}
		if cur == nil {
			preLines = append(preLines, ln)
		} else {
			cur.lines = append(cur.lines, ln)
		}
	}
	return strings.Join(preLines, "\n"), secs
}

type resultSection struct {
	title string
	lines []string
}

func (s resultSection) body() string { return strings.Join(s.lines, "\n") }

var (
	mdLink      = regexp.MustCompile(`\[([^\]]*)\]\(([^)\n]+)\)`)
	downloadRE  = regexp.MustCompile(`(?m)^- Downloaded file (.+) to "([^"\n]+)"$`)
	consoleLine = regexp.MustCompile(`(?m)^- New console entries: .*$\n?`)
)

// resultCleaner rewrites one upstream result for an agent on another device:
// the browser's own file paths (snapshot yml, console log, screenshots) mean
// nothing there, so they are replaced by artifact refs or hints, and the
// listing of other tabs is cut down to sites.
type resultCleaner struct {
	paths  state.Paths
	outDir string // the browser's output folder; only files inside are read
	refs   []string
	// tabsResult marks browser_tabs output, whose "Result" is a tab list with
	// page-chosen titles that must be reduced to sites.
	tabsResult bool
	// images are the inline images of this result, already saved; screenshot
	// links in the text take their refs in order.
	images []imageRef
	imgIdx int
	// captured collects every file taken from outDir so it can be removed.
	captured []string
	err      error
}

// clean returns the rewritten text.
func (c *resultCleaner) clean(text string) string {
	pre, secs := splitSections(text)
	var out []string
	if strings.TrimSpace(pre) != "" {
		out = append(out, strings.TrimRight(pre, "\n"))
	}
	for _, s := range secs {
		switch s.title {
		case "Ran Playwright code":
			continue // the browser's internal script; noise for the agent
		case "Snapshot":
			if mdLink.MatchString(s.body()) {
				out = append(out, "### Snapshot\n- Call browser_snapshot to read the page.")
				continue
			}
		case "Events":
			body := consoleLine.ReplaceAllString(s.body(), "")
			body = downloadRE.ReplaceAllStringFunc(body, func(m string) string {
				sub := downloadRE.FindStringSubmatch(m)
				if ref := c.capture(sub[2], ""); ref != "" {
					return fmt.Sprintf("- Downloaded file %s to %s", sub[1], ref)
				}
				return fmt.Sprintf("- Downloaded file %s (not kept)", sub[1])
			})
			if strings.TrimSpace(body) == "" {
				continue
			}
			out = append(out, "### Events\n"+strings.TrimRight(body, "\n"))
			continue
		case "Open tabs":
			if body := redactTabList(s.body()); body != "" {
				out = append(out, "### Open tabs\n"+body)
			}
			continue
		case "Result":
			if c.tabsResult {
				out = append(out, "### Result\n"+redactTabList(s.body()))
				continue
			}
			body := mdLink.ReplaceAllStringFunc(s.body(), func(m string) string {
				sub := mdLink.FindStringSubmatch(m)
				if ref := c.captureFile(sub[2]); ref != "" {
					return "[" + sub[1] + "](" + ref + ")"
				}
				return m
			})
			out = append(out, "### Result\n"+strings.TrimRight(body, "\n"))
			continue
		}
		out = append(out, "### "+s.title+"\n"+strings.TrimRight(s.body(), "\n"))
	}
	return strings.Join(out, "\n")
}

// imageRef is an image the upstream returned inline, already saved.
type imageRef struct{ ref string }

// captureFile maps a path printed by the upstream to an artifact ref. When
// the file's image bytes were already saved from the inline content, that ref
// is used and the file is only discarded.
func (c *resultCleaner) captureFile(printed string) string {
	abs, ok := c.insideOut(printed)
	if !ok {
		return ""
	}
	ext := strings.ToLower(filepath.Ext(abs))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".webp":
		if c.imgIdx < len(c.images) && c.images[c.imgIdx].ref != "" {
			ref := c.images[c.imgIdx].ref
			c.imgIdx++
			c.captured = append(c.captured, abs)
			return ref
		}
	case ".pdf":
	default:
		return "" // yml / log / other: only meaningful on the browser's own disk
	}
	return c.capture(abs, ext)
}

// capture copies a file from the output folder into an artifact.
func (c *resultCleaner) capture(printed, ext string) string {
	abs, ok := c.insideOut(printed)
	if !ok {
		return ""
	}
	if ext == "" {
		ext = strings.ToLower(filepath.Ext(abs))
	}
	if ext == "" || len(ext) > 8 || strings.ContainsAny(ext, " /\\:") {
		ext = ".bin"
	}
	fi, err := os.Lstat(abs)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > maxCapture {
		return ""
	}
	ref, f, err := files.NewArtifact(c.paths, "browser", ext)
	if err != nil {
		c.err = err
		return ""
	}
	src, err := os.Open(abs)
	if err != nil {
		f.Close()
		return ""
	}
	_, cerr := io.Copy(f, src)
	src.Close()
	if err := f.Close(); cerr == nil {
		cerr = err
	}
	if cerr != nil {
		c.err = cerr
		return ""
	}
	c.captured = append(c.captured, abs)
	c.refs = append(c.refs, string(ref))
	return string(ref)
}

// insideOut resolves a path the upstream printed (relative to its working
// directory) and returns it only if it is a file inside the output folder.
func (c *resultCleaner) insideOut(printed string) (string, bool) {
	printed = strings.TrimSpace(printed)
	if i := strings.Index(printed, "#"); i >= 0 { // "file.log#L3"
		printed = printed[:i]
	}
	if printed == "" {
		return "", false
	}
	p := filepath.FromSlash(strings.ReplaceAll(printed, `\`, "/"))
	if !filepath.IsAbs(p) {
		p = filepath.Join(filepath.Dir(c.outDir), p)
	}
	p = filepath.Clean(p)
	rel, err := filepath.Rel(c.outDir, p)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", false
	}
	return p, true
}

// redactTabList turns the upstream's tab listing (with page-chosen titles)
// into index, current marker and site only.
func redactTabList(body string) string {
	tabs, err := ParseTabs("### Result\n" + body)
	if err != nil {
		return "- (tab details withheld: " + err.Error() + ")"
	}
	return renderTabs(tabs)
}

// renderTabs lists tabs by site only: titles and paths belong to pages the
// agent may not have been allowed to read.
func renderTabs(tabs []Tab) string {
	if len(tabs) == 0 {
		return "No open tabs. Navigate to a URL to create one."
	}
	var b strings.Builder
	for i, t := range tabs {
		if i > 0 {
			b.WriteByte('\n')
		}
		cur := ""
		if t.Current {
			cur = " (current)"
		}
		fmt.Fprintf(&b, "- %d:%s %s", t.Index, cur, tabSite(t))
	}
	return b.String()
}

func tabSite(t Tab) string {
	switch {
	case !t.Origin.IsZero():
		return t.Origin.String()
	case t.Scheme != "":
		return t.Scheme + " page (not a website)"
	}
	return "blank page"
}

// saveImage stores image bytes as an artifact and reports the ref.
func saveImage(paths state.Paths, data []byte, mime string) (string, error) {
	ext := map[string]string{"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp", "image/gif": ".gif"}[strings.ToLower(mime)]
	if ext == "" {
		ext = ".img"
	}
	ref, f, err := files.NewArtifact(paths, "browser", ext)
	if err != nil {
		return "", err
	}
	_, werr := io.Copy(f, bytes.NewReader(data))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return "", werr
	}
	return string(ref), nil
}

// convert rewrites an upstream result for the agent: images become artifact
// refs (small ones also stay inline), browser-local paths disappear, and the
// raw file the browser wrote is deleted.
func convert(paths state.Paths, outDir string, res *mcp.CallToolResult, tabsTool bool) (*mcp.CallToolResult, []string, error) {
	c := &resultCleaner{paths: paths, outDir: outDir, tabsResult: tabsTool}
	out := &mcp.CallToolResult{IsError: res.IsError}
	var keep []mcp.Content
	var firstErr error
	for _, content := range res.Content {
		if img, ok := content.(*mcp.ImageContent); ok {
			ref, err := saveImage(paths, img.Data, img.MIMEType)
			if err != nil && firstErr == nil {
				firstErr = err
			}
			c.images = append(c.images, imageRef{ref: ref})
			if ref != "" {
				c.refs = append(c.refs, ref)
			}
			if len(img.Data) <= MaxInlineImage {
				keep = append(keep, img)
			}
			continue
		}
		keep = append(keep, content)
	}
	for _, content := range keep {
		if t, ok := content.(*mcp.TextContent); ok {
			out.Content = append(out.Content, &mcp.TextContent{Text: c.clean(t.Text)})
			continue
		}
		out.Content = append(out.Content, content)
	}
	// An image with no text to carry its ref still has to tell the agent where it went.
	for _, im := range c.images {
		if im.ref != "" && !mentions(out, im.ref) {
			out.Content = append(out.Content, &mcp.TextContent{Text: "Image saved as " + im.ref})
		}
	}
	for _, p := range c.captured {
		os.Remove(p)
	}
	if firstErr == nil {
		firstErr = c.err
	}
	return out, c.refs, firstErr
}

func mentions(res *mcp.CallToolResult, ref string) bool {
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok && strings.Contains(t.Text, ref) {
			return true
		}
	}
	return false
}

// textOf concatenates the text parts of a result.
func textOf(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			if b.Len() > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

// stagedFile is an upload copied next to the browser so the upstream's
// workspace-root restriction lets it read the file.
type stagedFile struct {
	Ref    string
	Host   string // path on this device
	Size   int64
	SHA256 string
	staged string // copy handed to the browser
}

// resolveUploads maps ws/ and artifacts/ refs to host files. Qualified refs,
// absolute paths, "..", links and anything else outside the two shared roots
// are refused by files.ParseRef / files.Resolve.
func resolveUploads(paths state.Paths, refs []string) ([]stagedFile, error) {
	var out []stagedFile
	for _, s := range refs {
		r, err := files.ParseRef(s)
		if err != nil {
			return nil, fmt.Errorf("upload %q: %v (use ws/... or artifacts/... on this device)", clip(s, 80), err)
		}
		host, err := files.Resolve(paths, r)
		if err != nil {
			return nil, fmt.Errorf("upload %q: %v", s, err)
		}
		fi, err := os.Lstat(host)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil, fmt.Errorf("upload %q does not exist on this device", s)
			}
			return nil, fmt.Errorf("upload %q: cannot read it", s)
		}
		if !fi.Mode().IsRegular() {
			return nil, fmt.Errorf("upload %q is not a regular file", s)
		}
		if fi.Size() > maxUploadSize {
			return nil, fmt.Errorf("upload %q is larger than %d MiB", s, maxUploadSize>>20)
		}
		sum, err := hashFile(host)
		if err != nil {
			return nil, fmt.Errorf("upload %q: %v", s, err)
		}
		out = append(out, stagedFile{Ref: string(r), Host: host, Size: fi.Size(), SHA256: sum})
	}
	return out, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// stageUploads copies each file into dir/<random>/<name>, checking the bytes
// still match the hash that was approved.
func stageUploads(dir string, list []stagedFile) (staged []stagedFile, cleanup func(), err error) {
	root := filepath.Join(dir, randomHex(8))
	cleanup = func() { os.RemoveAll(root) }
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, cleanup, err
	}
	for i, f := range list {
		dst := filepath.Join(root, fmt.Sprintf("%d", i), filepath.Base(f.Host))
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return nil, cleanup, err
		}
		sum, err := copyHashed(f.Host, dst)
		if err != nil {
			return nil, cleanup, fmt.Errorf("upload %q: %v", f.Ref, err)
		}
		if sum != f.SHA256 {
			return nil, cleanup, fmt.Errorf("upload %q changed after it was approved; ask again", f.Ref)
		}
		f.staged = dst
		staged = append(staged, f)
	}
	return staged, cleanup, nil
}

func copyHashed(src, dst string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, cerr := io.Copy(io.MultiWriter(out, h), in)
	if err := out.Close(); cerr == nil {
		cerr = err
	}
	if cerr != nil {
		return "", cerr
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
