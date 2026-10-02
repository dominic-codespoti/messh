// Package update discovers and stages official, explicitly requested main-channel updates.
// Checksums protect against corruption, not compromise of the publishing repository.
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"messh/internal/buildinfo"
)

const officialRepository = "dominic-codespoti/messh"
const maxArchiveSize int64 = 256 << 20
const maxMetadataSize int64 = 1 << 20

var mainTag = regexp.MustCompile(`^main-([1-9][0-9]*)-([0-9a-f]{12})$`)
var fullCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)
var digestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Client's transport and origin fields are injectable for local test fixtures only.
// Production callers must use NewClient and must not expose source configuration.
type Client struct {
	HTTP       *http.Client
	APIBase    string
	Repository string
}

func NewClient() *Client {
	return &Client{HTTP: &http.Client{Timeout: 2 * time.Minute}, APIBase: "https://api.github.com", Repository: officialRepository}
}

type Asset struct {
	OS         string `json:"os"`
	Arch       string `json:"arch"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	SHA256     string `json:"sha256"`
	Executable string `json:"executable"`
}

type Manifest struct {
	Schema  int     `json:"schema"`
	Channel string  `json:"channel"`
	Version string  `json:"version"`
	Commit  string  `json:"commit"`
	Build   uint64  `json:"build"`
	Tag     string  `json:"tag"`
	Assets  []Asset `json:"assets"`
}

type Release struct {
	Manifest   Manifest `json:"manifest"`
	Asset      Asset    `json:"asset"`
	URL        string   `json:"url"`
	archiveURL string
}

type CheckResult struct {
	Current         buildinfo.Info `json:"current"`
	Available       *Release       `json:"available,omitempty"`
	UpdateAvailable bool           `json:"update_available"`
	// Reason distinguishes unpublished local builds from an already installed release.
	Reason string `json:"reason"`
}

type githubAsset struct {
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	URL   string `json:"browser_download_url"`
	State string `json:"state"`
}

type githubRelease struct {
	Tag        string        `json:"tag_name"`
	Draft      bool          `json:"draft"`
	Prerelease bool          `json:"prerelease"`
	URL        string        `json:"html_url"`
	Assets     []githubAsset `json:"assets"`
}

func supported(goos, goarch string) bool {
	return goos == "windows" && goarch == "amd64" || goos == "linux" && (goarch == "amd64" || goarch == "arm64")
}

func (c *Client) settings() (string, string) {
	base, repo := strings.TrimRight(c.APIBase, "/"), c.Repository
	if base == "" {
		base = "https://api.github.com"
	}
	if repo == "" {
		repo = officialRepository
	}
	return base, repo
}

func (c *Client) trusted(raw string, api bool) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Fragment != "" {
		return false
	}
	base, _ := c.settings()
	b, err := url.Parse(base)
	if err != nil {
		return false
	}
	if base != "https://api.github.com" {
		return u.Scheme == b.Scheme && u.Host == b.Host && (u.Scheme == "http" || u.Scheme == "https")
	}
	if u.Scheme != "https" {
		return false
	}
	if api {
		return u.Host == "api.github.com"
	}
	return u.Host == "github.com" || u.Host == "release-assets.githubusercontent.com" || u.Host == "objects.githubusercontent.com"
}

func (c *Client) request(ctx context.Context, raw string, api bool) (*http.Response, error) {
	if !c.trusted(raw, api) {
		return nil, fmt.Errorf("untrusted update URL %q", raw)
	}
	client := http.Client{Timeout: 2 * time.Minute}
	if c.HTTP != nil {
		client = *c.HTTP
	}
	if client.Timeout <= 0 || client.Timeout > 2*time.Minute {
		client.Timeout = 2 * time.Minute
	}
	originalRedirect := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many update redirects")
		}
		if !c.trusted(req.URL.String(), api) {
			return errors.New("untrusted update redirect")
		}
		if originalRedirect != nil {
			return originalRedirect(req, via)
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "messh-updater")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("update download returned HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

func (c *Client) bytes(ctx context.Context, raw string, limit int64, api bool) ([]byte, http.Header, error) {
	resp, err := c.request(ctx, raw, api)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.ContentLength > limit {
		return nil, nil, errors.New("update metadata exceeds size limit")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, nil, err
	}
	if int64(len(data)) > limit {
		return nil, nil, errors.New("update metadata exceeds size limit")
	}
	return data, resp.Header, nil
}

func (c *Client) Check(ctx context.Context, current buildinfo.Info, goos, goarch string) (CheckResult, error) {
	result := CheckResult{Current: current, Reason: "no_release"}
	if !supported(goos, goarch) {
		return result, fmt.Errorf("unsupported update target %s/%s", goos, goarch)
	}
	base, repo := c.settings()
	if base == "https://api.github.com" && repo != officialRepository {
		return result, errors.New("updates require the official repository")
	}
	var best *githubRelease
	var bestBuild uint64
	seen := make(map[uint64]string)
	for page := 1; ; page++ {
		if page > 100 {
			return result, errors.New("release pagination exceeds safety limit")
		}
		raw := fmt.Sprintf("%s/repos/%s/releases?per_page=100&page=%d", base, repo, page)
		data, headers, err := c.bytes(ctx, raw, 4<<20, true)
		if err != nil {
			return result, err
		}
		var releases []githubRelease
		if err := json.Unmarshal(data, &releases); err != nil {
			return result, fmt.Errorf("decode releases: %w", err)
		}
		for _, release := range releases {
			if release.Draft || !release.Prerelease {
				continue
			}
			parts := mainTag.FindStringSubmatch(release.Tag)
			if parts == nil {
				continue
			}
			build, err := strconv.ParseUint(parts[1], 10, 64)
			if err != nil {
				return result, fmt.Errorf("invalid release build: %w", err)
			}
			if tag, exists := seen[build]; exists && tag != release.Tag {
				return result, errors.New("conflicting main releases for the same build")
			}
			seen[build] = release.Tag
			if best == nil || build > bestBuild {
				copy := release
				best, bestBuild = &copy, build
			}
		}
		if len(releases) < 100 && !strings.Contains(headers.Get("Link"), `rel="next"`) {
			break
		}
	}
	if best == nil {
		return result, nil
	}
	release, err := c.validateRelease(ctx, *best, goos, goarch)
	if err != nil {
		return result, err
	}
	result.Available = release
	if current.Channel == "main" && current.Build > 0 {
		switch {
		case current.Build > release.Manifest.Build:
			result.Reason = "newer_installed"
		case current.Build == release.Manifest.Build:
			if current.Commit != release.Manifest.Commit {
				return result, errors.New("installed build and published commit disagree")
			}
			if current.Version == release.Manifest.Version {
				result.Reason = "current"
			} else {
				result.UpdateAvailable, result.Reason = true, "unpublished_build"
			}
		default:
			result.UpdateAvailable, result.Reason = true, "update_available"
		}
	} else {
		result.UpdateAvailable, result.Reason = true, "unpublished_build"
	}
	return result, nil
}

func (c *Client) assetURL(asset githubAsset, tag string) bool {
	base, repo := c.settings()
	if !c.trusted(asset.URL, false) {
		return false
	}
	if base != "https://api.github.com" {
		return true
	}
	u, err := url.Parse(asset.URL)
	return err == nil && u.Host == "github.com" && u.RawQuery == "" && u.Path == "/"+repo+"/releases/download/"+tag+"/"+asset.Name
}

func (c *Client) validateRelease(ctx context.Context, release githubRelease, goos, goarch string) (*Release, error) {
	assets := make(map[string]githubAsset)
	for _, asset := range release.Assets {
		if _, duplicate := assets[asset.Name]; duplicate {
			return nil, errors.New("duplicate release asset")
		}
		if asset.State != "uploaded" || asset.Size <= 0 || !c.assetURL(asset, release.Tag) {
			return nil, fmt.Errorf("invalid release asset %q", asset.Name)
		}
		assets[asset.Name] = asset
	}
	manifestAsset, okManifest := assets["update.json"]
	sumsAsset, okSums := assets["SHA256SUMS"]
	if !okManifest || !okSums {
		return nil, errors.New("release is missing update.json or SHA256SUMS")
	}
	manifestData, _, err := c.bytes(ctx, manifestAsset.URL, maxMetadataSize, false)
	if err != nil {
		return nil, err
	}
	sumsData, _, err := c.bytes(ctx, sumsAsset.URL, maxMetadataSize, false)
	if err != nil {
		return nil, err
	}
	if int64(len(manifestData)) != manifestAsset.Size || int64(len(sumsData)) != sumsAsset.Size {
		return nil, errors.New("release metadata size mismatch")
	}
	sums, err := parseSums(sumsData)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(manifestData)
	if sums["update.json"] != hex.EncodeToString(digest[:]) {
		return nil, errors.New("update.json checksum mismatch")
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return nil, fmt.Errorf("decode update manifest: %w", err)
	}
	parts := mainTag.FindStringSubmatch(release.Tag)
	if parts == nil {
		return nil, errors.New("invalid main tag")
	}
	build, _ := strconv.ParseUint(parts[1], 10, 64)
	version := "0.1.0-main." + parts[1] + "+" + parts[2]
	if manifest.Schema != 1 || manifest.Channel != "main" || manifest.Tag != release.Tag || manifest.Build != build || manifest.Version != version || !fullCommit.MatchString(manifest.Commit) || !strings.HasPrefix(manifest.Commit, parts[2]) {
		return nil, errors.New("manifest and release identity disagree")
	}
	if len(manifest.Assets) != 3 {
		return nil, errors.New("main manifest must contain all three supported targets")
	}
	targets := make(map[string]bool)
	var selected *Release
	for _, asset := range manifest.Assets {
		target := asset.OS + "/" + asset.Arch
		if !supported(asset.OS, asset.Arch) || targets[target] {
			return nil, errors.New("invalid or duplicate manifest target")
		}
		targets[target] = true
		root := "messh-" + version + "-" + asset.OS + "-" + asset.Arch
		ext, binary := ".tar.gz", "messh"
		if asset.OS == "windows" {
			ext, binary = ".zip", "messh.exe"
		}
		published, exists := assets[asset.Name]
		if asset.Name != root+ext || asset.Executable != root+"/"+binary || asset.Size <= 0 || asset.Size > maxArchiveSize || !digestPattern.MatchString(asset.SHA256) || !exists || published.Size != asset.Size || sums[asset.Name] != asset.SHA256 {
			return nil, fmt.Errorf("invalid or incomplete manifest asset for %s", target)
		}
		if asset.OS == goos && asset.Arch == goarch {
			selected = &Release{Manifest: manifest, Asset: asset, URL: release.URL, archiveURL: published.URL}
		}
	}
	if len(sums) != len(manifest.Assets)+1 {
		return nil, errors.New("checksum set does not match manifest")
	}
	if selected == nil {
		return nil, errors.New("release has no requested target")
	}
	return selected, nil
}

func parseSums(data []byte) (map[string]string, error) {
	sums := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		if len(line) < 67 || !digestPattern.MatchString(line[:64]) || line[64] != ' ' || (line[65] != ' ' && line[65] != '*') {
			return nil, errors.New("invalid SHA256SUMS line")
		}
		name := line[66:]
		if name == "" || strings.ContainsAny(name, "/\\\r\n") {
			return nil, errors.New("invalid checksum filename")
		}
		if _, exists := sums[name]; exists {
			return nil, errors.New("duplicate checksum filename")
		}
		sums[name] = line[:64]
	}
	return sums, nil
}
