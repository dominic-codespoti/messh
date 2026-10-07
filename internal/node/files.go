package node

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/files"
	"messh/internal/gateway"
	"messh/internal/grants"
	"messh/internal/provider"
	"messh/internal/provider/filesprov"
)

const (
	filesProbeTimeout = 8 * time.Second
	tempFileMaxAge    = time.Hour
)

// fileService is the node's side of file transfer: the mesh endpoints peers
// call, the client side for reaching peers, and the agent-facing mesh_copy.
type fileService struct {
	n     *Node
	store *files.Store
	// wrapTransport lets tests tamper with traffic sent to peers.
	wrapTransport func(http.RoundTripper) http.RoundTripper
}

// startFiles creates the file store and wires the files_* tools, the mesh
// routes' handler, and mesh_copy. It needs n.gateway.
func (n *Node) startFiles() error {
	store, err := files.NewStore(n.paths)
	if err != nil {
		return fmt.Errorf("prepare file areas: %w", err)
	}
	n.files = &fileService{n: n, store: store}
	n.register(filesprov.NewAuthorized(store, func(c provider.Caller, ref, action string) grants.Decision {
		return n.fileProviderDecision(c, ref, action)
	}, func(c provider.Caller, requested, entry, action string) bool {
		return n.fileGrantVisible(c.DeviceID, c.Agent, requested, entry, action)
	}))
	n.rebuildTools()
	n.gateway.AddTool(meshCopyTool(), n.files.meshCopy)
	n.goRun(func() { store.Sweep(n.ctx, tempFileMaxAge) })
	return nil
}

func (fs *fileService) mount(mux *http.ServeMux) {
	h := fs.n.requirePeer(files.NewAuthorizedHandler(fs.store, func(r *http.Request, ref, action string) (bool, string) {
		return fs.n.fileHTTPDecision(r, ref, action)
	}, func(r *http.Request, requested, entry, action string) bool {
		return fs.n.fileGrantVisible(r.Header.Get(hdrPeerID), r.Header.Get(files.HeaderAgent), requested, entry, action)
	}))
	mux.Handle(files.Path, h)
	mux.Handle(files.Path+"/", h)
}

func meshCopyTool() *mcp.Tool {
	yes := true
	return &mcp.Tool{
		Name: "mesh_copy",
		Description: "Copy a file or a whole directory between devices on the mesh. Both sides are DEVICE:ref, where DEVICE " +
			"is a device handle or name from mesh_nodes (leave it empty for the device you are on) and ref is a path in " +
			"messh's file areas: ws/<workspace>/... (working files for jobs) or artifacts/<source>/... (outputs captured by " +
			"messh). Jobs and services never hand back raw paths: they return refs like artifacts/voicestudio/ab12cd.wav " +
			"or ws/<job>/out.png, and this tool is how you move such a file to another device, or how you put job inputs " +
			"onto the device that will run the job. Examples: from=\"desktop:artifacts/voicestudio/ab12cd.wav\" " +
			"to=\"ws/demo/\" (desktop to here); from=\"ws/render/scene.blend\" to=\"desktop:ws/render/\" (here to desktop); " +
			"from=\"desktop:ws/job1/out.png\" to=\"nas:ws/share/\" (desktop to nas, streamed through here). " +
			"Destinations must be under ws/ (artifacts/ is read-only). If `to` ends with / the source is placed inside it " +
			"under its own name; otherwise `to` is the exact destination. Directories are copied recursively and merged " +
			"into an existing directory. Existing files are never replaced unless overwrite=true. Every file is " +
			"checked with SHA-256 end to end and appears atomically, so a failure leaves no half-written files. Reports " +
			"files, bytes, seconds and throughput; large copies can take minutes.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{` +
			`"from":{"type":"string","description":"source as DEVICE:ref, e.g. desktop:artifacts/voicestudio/ab12cd.wav; empty DEVICE means this device"},` +
			`"to":{"type":"string","description":"destination as DEVICE:ref under ws/, e.g. raspi:ws/demo/ (trailing / = place inside that directory)"},` +
			`"overwrite":{"type":"boolean","description":"replace destination files that already exist (default false)"}},` +
			`"required":["from","to"],"additionalProperties":false}`),
		Annotations: &mcp.ToolAnnotations{DestructiveHint: &yes, OpenWorldHint: new(bool), Title: "Copy files between devices"},
	}
}

type copyArgs struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Overwrite bool   `json:"overwrite"`
}

type copyOut struct {
	From       string  `json:"from"`
	To         string  `json:"to"`
	DestRef    string  `json:"dest_ref"`
	Files      int     `json:"files"`
	Dirs       int     `json:"directories,omitempty"`
	Bytes      int64   `json:"bytes"`
	Seconds    float64 `json:"seconds"`
	MiBPerSec  float64 `json:"throughput_mib_s"`
	SHA256     string  `json:"sha256,omitempty"`
	Skipped    int     `json:"skipped,omitempty"`
	SkippedMsg string  `json:"note,omitempty"`
}

func (fs *fileService) meshCopy(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a copyArgs
	if err := json.Unmarshal(req.Params.Arguments, &a); err != nil {
		return provider.ErrorResult("mesh_copy: invalid arguments: %v", err), nil
	}
	if a.From == "" || a.To == "" {
		return provider.ErrorResult("mesh_copy: both from and to are required, as DEVICE:ref (e.g. desktop:ws/job/out.png)"), nil
	}
	agent := ""
	if req.Extra != nil && req.Extra.TokenInfo != nil {
		agent = req.Extra.TokenInfo.UserID
	}
	ctx = withFileAgent(ctx, agent)
	srcDev, srcRef := files.SplitQualified(a.From)
	dstDev, dstRef := files.SplitQualified(a.To)
	srcID, srcLabel, err := fs.resolveDevice(srcDev)
	if err != nil {
		return provider.ErrorResult("mesh_copy: from: %v", err), nil
	}
	dstID, dstLabel, err := fs.resolveDevice(dstDev)
	if err != nil {
		return provider.ErrorResult("mesh_copy: to: %v", err), nil
	}
	if srcID == fs.n.id.ID {
		if e := fs.n.authorizeLocalCopy(agent, srcRef, "read"); e != nil {
			return provider.ErrorFrom(e, "mesh_copy source"), nil
		}
	}
	if dstID == fs.n.id.ID {
		if e := fs.n.authorizeLocalCopy(agent, dstRef, "write"); e != nil {
			return provider.ErrorFrom(e, "mesh_copy destination"), nil
		}
	}

	src, closeSrc, err := fs.endpoint(ctx, srcID)
	if err != nil {
		return provider.ErrorResult("mesh_copy: from %s: %v", srcLabel, err), nil
	}
	defer closeSrc()
	dst := src
	if dstID != srcID {
		var closeDst func()
		if dst, closeDst, err = fs.endpoint(ctx, dstID); err != nil {
			return provider.ErrorResult("mesh_copy: to %s: %v", dstLabel, err), nil
		}
		defer closeDst()
	}

	res, err := files.Copy(ctx, src, dst, srcRef, dstRef, files.CopyOptions{Overwrite: a.Overwrite, SameDevice: srcID == dstID})
	fs.n.log.Info("mesh_copy", "from", srcLabel+":"+srcRef, "to", dstLabel+":"+dstRef, "agent", agent,
		"files", res.Files, "bytes", res.Bytes, "took", res.Duration.Round(time.Millisecond), "error", err)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return provider.ErrorFrom(err, "mesh_copy failed"), nil
	}
	secs := res.Duration.Seconds()
	out := copyOut{
		From: srcLabel + ":" + srcRef, To: dstLabel + ":" + res.To, DestRef: res.To,
		Files: res.Files, Dirs: res.Dirs, Bytes: res.Bytes, Seconds: round(secs, 3),
		SHA256: res.SHA256, Skipped: res.Skipped,
	}
	if secs > 0 {
		out.MiBPerSec = round(float64(res.Bytes)/secs/(1<<20), 2)
	}
	if res.Skipped > 0 {
		out.SkippedMsg = fmt.Sprintf("%d entries were not copied: symlinks, devices and files with unsupported names are skipped", res.Skipped)
	}
	return provider.JSONResult(out)
}

func round(v float64, places int) float64 {
	p := 1.0
	for range places {
		p *= 10
	}
	return float64(int64(v*p+0.5)) / p
}

// resolveDevice maps what an agent typed (empty, a handle, a name, or an ID
// prefix) to a device ID and the label used in results.
func (fs *fileService) resolveDevice(name string) (id, label string, err error) {
	nodes := fs.n.Nodes()
	handles := gateway.Handles(nodes)
	if name == "" {
		return nodes[0].ID, handles[nodes[0].ID], nil
	}
	lower := strings.ToLower(name)
	tiers := []func(gateway.Node) bool{
		func(d gateway.Node) bool { return handles[d.ID] == lower },
		func(d gateway.Node) bool { return strings.EqualFold(d.Name, name) },
		func(d gateway.Node) bool { return d.ID == lower || (len(lower) >= 4 && strings.HasPrefix(d.ID, lower)) },
	}
	for _, match := range tiers {
		var found []gateway.Node
		for _, d := range nodes {
			if match(d) {
				found = append(found, d)
			}
		}
		switch len(found) {
		case 1:
			return found[0].ID, handles[found[0].ID], nil
		case 0:
		default:
			return "", "", fmt.Errorf("%q matches %d devices; use the handle from mesh_nodes", name, len(found))
		}
	}
	var have []string
	for _, d := range nodes {
		have = append(have, handles[d.ID])
	}
	return "", "", fmt.Errorf("no device %q on this mesh (have: %s)", name, strings.Join(have, ", "))
}

// endpoint returns the file space of a device: this one's directly, a
// peer's through its pinned mTLS channel. The returned func releases the
// connections.
func (fs *fileService) endpoint(ctx context.Context, id string) (files.Endpoint, func(), error) {
	if id == fs.n.id.ID {
		return files.Local{Store: fs.store}, func() {}, nil
	}
	return fs.dial(ctx, id)
}

// dial finds an address of the peer that answers a file request and returns
// a client bound to it. Like the MCP sessions it uses the discovered and the
// remembered addresses, in the same order.
func (fs *fileService) dial(ctx context.Context, id string) (*files.Client, func(), error) {
	n := fs.n
	p, ok := n.roster.Get(id)
	if !ok {
		return nil, nil, fmt.Errorf("device %s is not paired", id)
	}
	addrs := n.peers.candidates(id)
	if len(addrs) == 0 {
		return nil, nil, fmt.Errorf("%s has no known address yet; it appears once discovery hears the device", p.Name)
	}
	var lastErr error
	for _, addr := range addrs {
		tr := n.pinnedTransport(id)
		var rt http.RoundTripper = tr
		if fs.wrapTransport != nil {
			rt = fs.wrapTransport(rt)
		}
		c := &files.Client{HTTP: &http.Client{Transport: rt}, Base: "https://" + addr, CallerAgent: fileAgent(ctx)}
		pctx, cancel := context.WithTimeout(ctx, filesProbeTimeout)
		_, err := c.List(pctx, "")
		cancel()
		if err == nil {
			now := time.Now()
			n.peers.markContact(id, now)
			if serr := n.roster.Seen(id, addr, now); serr != nil {
				n.log.Warn("save peer address", "error", serr)
			}
			return c, tr.CloseIdleConnections, nil
		}
		tr.CloseIdleConnections()
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		n.log.Debug("peer file address failed", "peer", p.Name, "addr", addr, "error", err)
		lastErr = err
	}
	return nil, nil, fmt.Errorf("%s is unreachable or does not offer file transfer (tried %s): %v", p.Name, strings.Join(addrs, ", "), lastErr)
}
