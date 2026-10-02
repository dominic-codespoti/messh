package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"messh/internal/files"
	"messh/internal/provider"
	"messh/internal/state"
)

const (
	maxCommandLen  = 32 << 10
	maxArgs        = 1024
	maxEnv         = 256
	maxInputs      = 256
	maxTimeoutSec  = 7 * 24 * 3600
	maxGPUIndex    = 63
	maxCPUsClaim   = 4096
	maxMemClaimMB  = 64 << 20
	maxLabelLen    = 120
	detailMaxChars = 1500
)

// submitArgs is the job_submit input.
type submitArgs struct {
	RequestID      string            `json:"request_id,omitempty"`
	Recovery       *recoveryArgs     `json:"recovery,omitempty"`
	Command        string            `json:"command"`
	Args           []string          `json:"args"`
	Cwd            string            `json:"cwd"`
	Env            map[string]string `json:"env"`
	Shell          bool              `json:"shell"`
	Inputs         []string          `json:"inputs"`
	Workspace      string            `json:"workspace"`
	Resources      Claims            `json:"resources"`
	TimeoutSeconds int               `json:"timeout_seconds"`
	Label          string            `json:"label"`
}

// request is a validated job_submit with everything resolved on this host.
type recoveryArgs struct {
	Checkpoint string   `json:"checkpoint"`
	Args       []string `json:"args"`
}

type request struct {
	RequestID  string
	Recovery   *recoveryArgs
	Path       string
	Args       []string
	Shell      bool
	Line       string
	Workspace  string // explicit workspace name, or "" for one named after the job
	Cwd        string // slash path inside the workspace, "" for its root
	Env        map[string]string
	Claims     Claims
	TimeoutSec int
	Inputs     []Input
	Label      string
}

func displayArgv(exe string, args []string, shell bool, line string) []string {
	if shell {
		if runtime.GOOS == "windows" {
			return []string{exe, "/S", "/C", line}
		}
		return []string{exe, "-c", line}
	}
	return append([]string{exe}, args...)
}

func (r *request) argv() []string { return displayArgv(r.Path, r.Args, r.Shell, r.Line) }

// parseSubmit validates the arguments and resolves the executable. Input
// hashes are filled in by the caller of resolveInputs.
func (p *Provider) parseSubmit(raw json.RawMessage) (*request, error) {
	var a submitArgs
	if err := ValidateSubmission(raw); err != nil {
		return nil, fmt.Errorf("invalid job_submit arguments: %w", err)
	}
	if len(raw) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&a); err != nil {
			return nil, fmt.Errorf("invalid job_submit arguments: %w", err)
		}
	}
	if strings.TrimSpace(a.Command) == "" {
		return nil, errors.New("command is required")
	}
	if len(a.Command) > maxCommandLen || strings.ContainsRune(a.Command, 0) {
		return nil, errors.New("command is too long or contains a NUL byte")
	}
	if len(a.Args) > maxArgs {
		return nil, fmt.Errorf("too many args (max %d)", maxArgs)
	}
	total := 0
	for _, s := range a.Args {
		total += len(s)
		if strings.ContainsRune(s, 0) {
			return nil, errors.New("args must not contain NUL bytes")
		}
	}
	if total > 4*maxCommandLen {
		return nil, errors.New("args are too long")
	}
	if a.Shell && len(a.Args) > 0 {
		return nil, errors.New("with shell:true put the whole command line in command and leave args empty")
	}

	r := &request{Shell: a.Shell, Label: strings.TrimSpace(a.Label), RequestID: a.RequestID}
	if a.Recovery != nil {
		clean := path.Clean(strings.TrimPrefix(a.Recovery.Checkpoint, "./"))
		if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
			return nil, errors.New("recovery.checkpoint must stay inside the workspace")
		}
		a.Recovery.Checkpoint = clean
		a.Recovery.Args = append([]string(nil), a.Recovery.Args...)
		r.Recovery = a.Recovery
	}
	if len(r.Label) > maxLabelLen {
		r.Label = r.Label[:maxLabelLen]
	}
	if a.Shell {
		exe, err := shellPath()
		if err != nil {
			return nil, err
		}
		r.Path, r.Line = exe, a.Command
	} else {
		exe, err := resolveExecutable(a.Command)
		if err != nil {
			return nil, err
		}
		r.Path = exe
		r.Args = append([]string(nil), a.Args...)
	}

	if a.Workspace != "" {
		if _, err := workspaceRef(a.Workspace); err != nil {
			return nil, err
		}
		r.Workspace = a.Workspace
	}
	cwd, err := cleanCwd(a.Cwd)
	if err != nil {
		return nil, err
	}
	r.Cwd = cwd

	if len(a.Env) > maxEnv {
		return nil, fmt.Errorf("too many env overrides (max %d)", maxEnv)
	}
	if len(a.Env) > 0 {
		r.Env = make(map[string]string, len(a.Env))
		for k, v := range a.Env {
			if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
				return nil, fmt.Errorf("invalid env entry %q", k)
			}
			r.Env[k] = v
		}
	}

	c := a.Resources
	for _, g := range c.GPUs {
		if g < 0 || g > maxGPUIndex {
			return nil, fmt.Errorf("resources.gpus: invalid GPU index %d", g)
		}
	}
	sort.Ints(c.GPUs)
	c.GPUs = dedupInts(c.GPUs)
	if c.VRAMMB < 0 || c.MemMB < 0 || c.CPUs < 0 || c.MemMB > maxMemClaimMB || c.VRAMMB > maxMemClaimMB || c.CPUs > maxCPUsClaim {
		return nil, errors.New("resources: vram_mb, mem_mb and cpus must be non-negative and realistic")
	}
	r.Claims = c

	if a.TimeoutSeconds < 0 || a.TimeoutSeconds > maxTimeoutSec {
		return nil, fmt.Errorf("timeout_seconds must be between 0 (none) and %d", maxTimeoutSec)
	}
	r.TimeoutSec = a.TimeoutSeconds

	if len(a.Inputs) > maxInputs {
		return nil, fmt.Errorf("too many inputs (max %d)", maxInputs)
	}
	seen := map[string]bool{}
	for _, s := range a.Inputs {
		dev, ref := files.SplitQualified(s)
		if dev != "" {
			return nil, fmt.Errorf("input %q names another device; inputs must already be on the device that runs the job (copy them here first)", s)
		}
		pr, err := files.ParseRef(ref)
		if err != nil {
			return nil, fmt.Errorf("input: %w", err)
		}
		if !seen[string(pr)] {
			seen[string(pr)] = true
			r.Inputs = append(r.Inputs, Input{Ref: string(pr)})
		}
	}
	sort.Slice(r.Inputs, func(i, j int) bool { return r.Inputs[i].Ref < r.Inputs[j].Ref })
	return r, nil
}

func dedupInts(s []int) []int {
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}

func workspaceRef(name string) (files.Ref, error) {
	if strings.ContainsAny(name, "/\\") {
		return "", fmt.Errorf("workspace %q must be a single directory name, not a path", name)
	}
	r, err := files.ParseRef(files.RootWorkspaces + "/" + name)
	if err != nil {
		return "", fmt.Errorf("workspace: %w", err)
	}
	return r, nil
}

// cleanCwd normalises a workspace-relative directory and refuses escapes.
func cleanCwd(cwd string) (string, error) {
	if cwd == "" {
		return "", nil
	}
	if strings.ContainsAny(cwd, "\\:\x00") || strings.HasPrefix(cwd, "/") {
		return "", fmt.Errorf("cwd %q must be a relative, forward-slash path inside the job workspace", cwd)
	}
	c := path.Clean(cwd)
	if c == "." {
		return "", nil
	}
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("cwd %q escapes the job workspace", cwd)
	}
	if _, err := files.ParseRef(files.RootWorkspaces + "/w/" + c); err != nil {
		return "", fmt.Errorf("cwd: %w", err)
	}
	return c, nil
}

func shellPath() (string, error) {
	if runtime.GOOS == "windows" {
		exe, err := exec.LookPath("cmd.exe")
		if err != nil {
			return "", fmt.Errorf("shell:true needs cmd.exe: %w", err)
		}
		return filepath.Clean(exe), nil
	}
	if st, err := os.Stat("/bin/sh"); err == nil && !st.IsDir() {
		return "/bin/sh", nil
	}
	exe, err := exec.LookPath("sh")
	if err != nil {
		return "", fmt.Errorf("shell:true needs sh: %w", err)
	}
	return exe, nil
}

// resolveExecutable maps a program name to an absolute path on this host.
// Relative paths with separators are refused: their meaning would depend on
// the node's own working directory.
func resolveExecutable(command string) (string, error) {
	if strings.ContainsAny(command, "\r\n") {
		return "", errors.New("command must be a program name or absolute path, not a command line (use shell:true for that)")
	}
	if !filepath.IsAbs(command) && strings.ContainsAny(command, `/\`) {
		return "", fmt.Errorf("command %q is a relative path; use a program name found on PATH or an absolute path", command)
	}
	exe, err := exec.LookPath(command)
	if err != nil {
		return "", fmt.Errorf("command %q not found on this device: %w", command, err)
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

// inputRel maps an input ref to its place inside workspace ws and reports
// whether it already lives there.
func inputRel(ref, ws string) (rel string, inPlace bool) {
	parts := strings.SplitN(ref, "/", 3)
	if parts[0] == files.RootWorkspaces && len(parts) == 3 {
		return parts[2], parts[1] == ws
	}
	return ref, false // artifacts keep their artifacts/<source>/<file> layout
}

// racyWindow is how much older than its hash time a file's mtime must be
// before size+mtime may stand in for its content (git's "racily clean" rule).
// Filesystem timestamps are coarse (a few ms on Linux, up to 2 s on FAT), so
// a write landing in the same tick as the hash leaves the metadata unchanged.
const racyWindow = 2 * time.Second

// statClean reports whether a file whose content was hashed at hashedAt
// (UnixNano, 0 = unknown) can be trusted unchanged on matching size+mtime:
// any later write would have given it an mtime at or after hashedAt, minus
// the clock's granularity, which can then no longer equal the recorded one.
func statClean(mod, hashedAt int64) bool {
	return hashedAt != 0 && mod < hashedAt-int64(racyWindow)
}

// hashes input files, remembering results per (path, size, mtime) so a large
// model file is not re-read for every submission.
type hashCache struct {
	mu sync.Mutex
	m  map[string]cachedHash
}

type cachedHash struct {
	sum      string
	hashedAt int64 // UnixNano taken before the file was read
}

type fileKey struct {
	path string
	size int64
	mod  int64
}

func (k fileKey) String() string {
	return k.path + "|" + strconv.FormatInt(k.size, 10) + "|" + strconv.FormatInt(k.mod, 10)
}

func (h *hashCache) get(k fileKey) (cachedHash, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.m[k.String()]
	return c, ok
}

func (h *hashCache) put(k fileKey, c cachedHash) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.m == nil || len(h.m) > 256 {
		h.m = map[string]cachedHash{}
	}
	h.m[k.String()] = c
}

// hashFile returns the SHA-256 of a regular file, the stat that goes with
// it, and when (UnixNano) that content was read. A cached hash is used only
// when the file's mtime predates that hash time by racyWindow; see statClean.
func (h *hashCache) hashFile(p string) (sum string, st os.FileInfo, hashedAt int64, err error) {
	hashedAt = time.Now().UnixNano() // before the stat, so no write after it can hide
	st, err = os.Stat(p)
	if err != nil {
		return "", nil, 0, err
	}
	if !st.Mode().IsRegular() {
		return "", nil, 0, fmt.Errorf("%s is not a regular file", filepath.Base(p))
	}
	k := fileKey{p, st.Size(), st.ModTime().UnixNano()}
	if c, ok := h.get(k); ok && statClean(k.mod, c.hashedAt) {
		return c.sum, st, c.hashedAt, nil
	}
	f, err := os.Open(p)
	if err != nil {
		return "", nil, 0, err
	}
	defer f.Close()
	d := sha256.New()
	if _, err := io.Copy(d, f); err != nil {
		return "", nil, 0, err
	}
	sum = hex.EncodeToString(d.Sum(nil))
	// The file may have changed while we read it; only cache a stable read,
	// and only when its metadata can vouch for the content later.
	if st2, err := f.Stat(); err == nil && st2.Size() == st.Size() && st2.ModTime().Equal(st.ModTime()) && statClean(k.mod, hashedAt) {
		h.put(k, cachedHash{sum, hashedAt})
	}
	return sum, st, hashedAt, nil
}

// hashInputs fills in size and SHA-256 for every input from its source file.
func (p *Provider) hashInputs(r *request, workspace string) error {
	for i := range r.Inputs {
		in := &r.Inputs[i]
		abs, err := files.Resolve(p.paths, files.Ref(in.Ref))
		if err != nil {
			return fmt.Errorf("input %s: %w", in.Ref, err)
		}
		sum, st, _, err := p.hashes.hashFile(abs)
		if err != nil {
			return fmt.Errorf("input %s: %w", in.Ref, err)
		}
		in.SHA256, in.Size = sum, st.Size()
		if workspace != "" {
			in.Rel, in.InPlace = inputRel(in.Ref, workspace)
		}
	}
	return nil
}

// --- canonical hashing ---

type canonRequest struct {
	V          int           `json:"v"`
	Tool       string        `json:"tool"`
	Path       string        `json:"path"`
	Args       []string      `json:"args"`
	Shell      bool          `json:"shell"`
	Line       string        `json:"line"`
	Workspace  string        `json:"workspace"`
	Cwd        string        `json:"cwd"`
	Env        [][2]string   `json:"env"`
	GPUs       []int         `json:"gpus"`
	VRAMMB     int64         `json:"vram_mb"`
	MemMB      int64         `json:"mem_mb"`
	CPUs       int           `json:"cpus"`
	TimeoutSec int           `json:"timeout_seconds"`
	Inputs     [][2]string   `json:"inputs"`
	Recovery   *recoveryArgs `json:"recovery,omitempty"`
}

func sortedEnv(env map[string]string) [][2]string {
	out := make([][2]string, 0, len(env))
	for k, v := range env {
		out = append(out, [2]string{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

func sum256(v any) string {
	b, _ := json.Marshal(v) // plain data; cannot fail
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func (r *request) canon() canonRequest {
	c := canonRequest{
		V: 1, Tool: "job_submit", Path: r.Path, Args: nonNil(r.Args), Shell: r.Shell, Line: r.Line,
		Workspace: r.Workspace, Cwd: r.Cwd, Env: sortedEnv(r.Env),
		GPUs: append([]int{}, r.Claims.GPUs...), VRAMMB: r.Claims.VRAMMB, MemMB: r.Claims.MemMB, CPUs: r.Claims.CPUs,
		TimeoutSec: r.TimeoutSec, Inputs: [][2]string{}, Recovery: r.Recovery,
	}
	for _, in := range r.Inputs {
		c.Inputs = append(c.Inputs, [2]string{in.Ref, in.SHA256})
	}
	return c
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// exact is the hash "Allow once" approves: the whole request including
// input content.
func (r *request) exact() string { return sum256(r.canon()) }

// commandKey is the "always allow this command" scope: everything except
// the input files and resource claims.
func (r *request) commandKey() string {
	c := r.canon()
	return sum256(struct {
		Path      string        `json:"path"`
		Args      []string      `json:"args"`
		Shell     bool          `json:"shell"`
		Line      string        `json:"line"`
		Workspace string        `json:"workspace"`
		Cwd       string        `json:"cwd"`
		Env       [][2]string   `json:"env"`
		Recovery  *recoveryArgs `json:"recovery,omitempty"`
	}{c.Path, c.Args, c.Shell, c.Line, c.Workspace, c.Cwd, c.Env, r.Recovery})
}

// interpreters run whatever they are given, so approving one with any
// arguments is as broad as approving any job.
var interpreters = map[string]bool{
	"cmd": true, "powershell": true, "pwsh": true, "sh": true, "bash": true, "zsh": true, "dash": true,
	"fish": true, "python": true, "python3": true, "py": true, "node": true, "perl": true, "ruby": true,
	"wscript": true, "cscript": true, "mshta": true, "env": true, "wsl": true, "deno": true, "bun": true,
}

func isInterpreter(exe string) bool {
	b := strings.ToLower(filepath.Base(exe))
	b = strings.TrimSuffix(b, ".exe")
	if interpreters[b] {
		return true
	}
	return strings.HasPrefix(b, "python3.")
}

// --- approval ---

func quoteArg(s string) string { return strconv.Quote(s) }

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !isRuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + fmt.Sprintf("… (%d more bytes)", len(s)-cut)
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }

func (r *request) approval(workspaceShown string) provider.Approval {
	argv := r.argv()
	q := make([]string, len(argv))
	for i, a := range argv {
		q[i] = quoteArg(a)
	}
	ws := files.RootWorkspaces + "/" + workspaceShown
	dir := ws
	if r.Cwd != "" {
		dir += "/" + r.Cwd
	}
	details := []provider.Detail{
		{Label: "Command", Value: truncate(strings.Join(q, " "), detailMaxChars)},
		{Label: "Runs as", Value: "the logged-in user on this device, with the full user environment"},
		{Label: "Folder", Value: dir},
	}
	if r.Shell {
		details = append(details, provider.Detail{Label: "Shell", Value: "the command line is interpreted by " + filepath.Base(r.Path)})
	}
	if r.Recovery != nil {
		args := make([]string, len(r.Recovery.Args))
		for i, a := range r.Recovery.Args {
			args[i] = quoteArg(a)
		}
		details = append(details, provider.Detail{Label: "Resume command", Value: truncate(quoteArg(r.Path)+" "+strings.Join(args, " "), detailMaxChars)}, provider.Detail{Label: "Checkpoint", Value: r.Recovery.Checkpoint})
	}
	if len(r.Env) > 0 {
		var kv []string
		for _, e := range sortedEnv(r.Env) {
			kv = append(kv, e[0]+"="+truncate(strconv.Quote(e[1]), 120))
		}
		details = append(details, provider.Detail{Label: "Environment", Value: truncate(strings.Join(kv, "  "), detailMaxChars)})
	}
	if len(r.Inputs) > 0 {
		var lines []string
		for i, in := range r.Inputs {
			if i == 12 {
				lines = append(lines, fmt.Sprintf("… and %d more", len(r.Inputs)-i))
				break
			}
			lines = append(lines, fmt.Sprintf("%s (%s, sha256 %s…)", in.Ref, humanSize(in.Size), in.SHA256[:12]))
		}
		details = append(details, provider.Detail{Label: "Input files", Value: strings.Join(lines, "\n")})
	}
	if !r.Claims.empty() {
		details = append(details, provider.Detail{Label: "Resources", Value: r.Claims.String()})
	}
	if r.TimeoutSec > 0 {
		details = append(details, provider.Detail{Label: "Time limit", Value: (time.Duration(r.TimeoutSec) * time.Second).String()})
	} else {
		details = append(details, provider.Detail{Label: "Time limit", Value: "none"})
	}

	cmdLabel := "this exact command, arguments, folder and environment, with any input files"
	scopes := []provider.Scope{{Key: "exec:cmd:" + r.commandKey(), Label: cmdLabel}}
	binLabel := "any use of " + r.Path + " with any arguments"
	broad := isInterpreter(r.Path) || r.Shell
	if broad {
		binLabel = "anything run through " + filepath.Base(r.Path) + " (it can run any program or script)"
	}
	scopes = append(scopes,
		provider.Scope{Key: "exec:bin:" + r.Path, Label: binLabel, Broad: broad},
		provider.Scope{Key: "exec:*", Label: "any job from this agent on that device", Broad: true},
	)
	return provider.Approval{
		Title:    "Run a job",
		Details:  details,
		Exact:    r.exact(),
		Scopes:   scopes,
		Deferred: true,
	}
}

// String renders claims for humans.
func (c Claims) String() string {
	var parts []string
	if len(c.GPUs) > 0 {
		g := make([]string, len(c.GPUs))
		for i, v := range c.GPUs {
			g[i] = strconv.Itoa(v)
		}
		parts = append(parts, "GPU "+strings.Join(g, ",")+" (exclusive)")
	}
	if c.VRAMMB > 0 {
		parts = append(parts, fmt.Sprintf("%d MB VRAM", c.VRAMMB))
	}
	if c.MemMB > 0 {
		parts = append(parts, fmt.Sprintf("%d MB RAM (hard limit where the OS allows)", c.MemMB))
	}
	if c.CPUs > 0 {
		parts = append(parts, fmt.Sprintf("%d CPU threads", c.CPUs))
	}
	return strings.Join(parts, ", ")
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// jobEnv builds the child environment: the user's own plus overrides, plus
// the job's identity and its GPU selection.
func jobEnv(base []string, j *Job, workspaceDir, checkpointSnapshot string) []string {
	overrides := map[string]string{}
	for k, v := range j.Env {
		overrides[k] = v
	}
	overrides["MESSH_JOB_ID"] = j.ID
	overrides["MESSH_WORKSPACE"] = workspaceDir
	if j.Recovery != nil {
		if j.Attempt > 0 && j.CheckpointSnapshot != "" {
			overrides["MESSH_CHECKPOINT"] = checkpointSnapshot
			overrides["MESSH_RESUMED"] = "1"
		} else {
			overrides["MESSH_CHECKPOINT"] = filepath.Join(workspaceDir, filepath.FromSlash(j.Recovery.Checkpoint))
		}
	}
	if len(j.Claims.GPUs) > 0 {
		g := make([]string, len(j.Claims.GPUs))
		for i, v := range j.Claims.GPUs {
			g[i] = strconv.Itoa(v)
		}
		overrides["CUDA_VISIBLE_DEVICES"] = strings.Join(g, ",")
	}
	fold := func(s string) string { return s }
	if runtime.GOOS == "windows" {
		fold = strings.ToUpper
	}
	drop := map[string]bool{}
	for k := range overrides {
		drop[fold(k)] = true
	}
	out := make([]string, 0, len(base)+len(overrides))
	for _, e := range base {
		k, _, _ := strings.Cut(e, "=")
		if k == "" { // Windows keeps drive-cwd entries like "=C:=C:\x"
			out = append(out, e)
			continue
		}
		if !drop[fold(k)] {
			out = append(out, e)
		}
	}
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		out = append(out, k+"="+overrides[k])
	}
	return out
}

// workspaceDir resolves ws/<name> on disk.
func workspaceDir(p state.Paths, name string) (string, error) {
	ref, err := workspaceRef(name)
	if err != nil {
		return "", err
	}
	return files.Resolve(p, ref)
}

// cwdDir returns the absolute working directory for a job, creating it. The
// path is resolved before it is created so a symlink planted inside a shared
// workspace cannot redirect it elsewhere.
func (p *Provider) cwdDir(ws, cwd string) (string, error) {
	s := files.RootWorkspaces + "/" + ws
	if cwd != "" {
		s += "/" + cwd
	}
	ref, err := files.ParseRef(s)
	if err != nil {
		return "", err
	}
	abs, err := files.Resolve(p.paths, ref)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return "", err
	}
	return files.Resolve(p.paths, ref)
}

func recoveryFromRequest(r *recoveryArgs) *Recovery {
	if r == nil {
		return nil
	}
	return &Recovery{Checkpoint: r.Checkpoint, Args: append([]string(nil), r.Args...)}
}

// ApprovalForJob reconstructs the narrow prompt for an unapproved durable job.
func ApprovalForJob(j Job) provider.Approval {
	r := &request{Path: j.Path, Args: j.Args, Shell: j.Shell, Line: j.Line, Workspace: j.Workspace, Cwd: j.Cwd, Env: j.Env, Claims: j.Claims, TimeoutSec: j.TimeoutSec, Inputs: j.Inputs, Recovery: nil}
	if j.Recovery != nil {
		r.Recovery = &recoveryArgs{Checkpoint: j.Recovery.Checkpoint, Args: j.Recovery.Args}
	}
	ap := r.approval(j.Workspace)
	if j.Exact != "" {
		ap.Exact = j.Exact
	}
	return ap
}
