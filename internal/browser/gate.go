package browser

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"messh/internal/provider"
)

// ScopeAny is the rule key that approves every browser action on every site.
const ScopeAny = "browser:*"

// ScopeKey is the rule key for a level on one origin. A saved interact rule
// also satisfies later read requests because read requests list it too.
func ScopeKey(o Origin, l Level) string { return "browser:" + o.String() + ":" + string(l) }

var errNoPage = errors.New("no page is open in the browser yet; call browser_navigate with a URL first")

// plan is the resolved meaning of one call: which site it touches, at what
// level, and what the approval must say.
type plan struct {
	Tool    string
	Raw     json.RawMessage
	Args    callArgs
	Spec    toolSpec
	Level   Level
	Origin  Origin // the website this call is approved against; zero when none
	PageURL string // that page's address without credentials, query or fragment
	Auto    string // set when no prompt is needed, with the reason
	Tabs    []Tab  // the tab list the plan was made from (page-bound calls)
	Target  *Tab   // for tab actions, the tab acted on
	Uploads []stagedFile
	Exact   string
	Mode    string
	// Reached is set on follow-up approvals: where the browser ended up.
	Reached string
}

func (pl *plan) siteLabel() string { return siteName(pl.Origin) }

// siteName is how an origin reads in a prompt: github.com, or
// http://127.0.0.1:8080 when the scheme or port is not the usual one.
func siteName(o Origin) string {
	if o.Scheme == "https" && o.Port == "443" {
		return o.Host
	}
	return o.String()
}

// plan classifies a call and resolves the website it touches. Anything it
// cannot establish is an error: the call is refused, never guessed.
func (p *Provider) plan(ctx context.Context, tool string, raw json.RawMessage) (*plan, error) {
	cfg := p.config()
	if !cfg.Active() {
		return nil, errors.New("browser tools are disabled on this device (browser.json)")
	}
	if !Published(tool) || (tool == "browser_evaluate" && !cfg.AllowScript) {
		return nil, fmt.Errorf("%s is not available on this device", tool)
	}
	args, err := parseArgs(raw)
	if err != nil {
		return nil, err
	}
	spec := specs[tool]
	if tool == "browser_tabs" {
		spec, err = tabsSpec(args.str("action"), args.str("url") != "")
		if err != nil {
			return nil, err
		}
	}
	n := cfg.Normalized()
	pl := &plan{Tool: tool, Raw: raw, Args: args, Spec: spec, Level: spec.Level, Mode: n.Mode}

	switch spec.Kind {
	case onNothing, onTabsList:
		pl.Auto = "no website is involved"
	case onURL:
		rawURL := args.str("url")
		if strings.EqualFold(strings.TrimSpace(rawURL), "about:blank") {
			pl.Level, pl.Auto = LevelNone, "a blank page involves no website"
			break
		}
		o, clean, err := ParseURL(rawURL)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", tool, err)
		}
		pl.Origin, pl.PageURL = o, clean
	case onPage, onTab:
		tabs, err := p.listTabs(ctx)
		if err != nil {
			return nil, err
		}
		pl.Tabs = tabs
		t, err := pickTab(tabs, pl.Spec.Kind, args)
		if err != nil {
			return nil, err
		}
		pl.Target = &t
		if t.Scheme != "" {
			return nil, fmt.Errorf("the browser is showing a %s page; messh only works with http and https websites", t.Scheme)
		}
		if t.Inert() {
			if pl.Level == LevelRead || pl.Spec.Kind == onTab {
				pl.Level, pl.Auto = LevelNone, "the page is blank"
				break
			}
			return nil, errors.New("the browser is showing a blank page; call browser_navigate first")
		}
		pl.Origin = t.Origin
		_, pl.PageURL, _ = ParseURL(t.URL)
	}

	if !pl.Origin.IsZero() && MatchAny(cfg.DenyOrigins, true, pl.Origin) {
		return nil, fmt.Errorf("%s is blocked on this device (deny_origins in browser.json)", siteName(pl.Origin))
	}
	if tool == "browser_file_upload" {
		ups, err := resolveUploads(p.paths, args.strs("paths"))
		if err != nil {
			return nil, err
		}
		pl.Uploads = ups
	}
	if pl.Auto == "" && pl.Level == LevelRead && MatchAny(cfg.AllowOrigins, false, pl.Origin) {
		pl.Auto = "browser.json: allow_origins lists " + siteName(pl.Origin)
	}
	pl.Exact = exactHash(pl)
	return pl, nil
}

// pickTab chooses the tab a call acts on.
func pickTab(tabs []Tab, k kind, args callArgs) (Tab, error) {
	if len(tabs) == 0 {
		return Tab{}, errNoPage
	}
	if k == onTab {
		if idx, ok := args.num("index"); ok {
			if idx < 0 || idx >= len(tabs) {
				return Tab{}, fmt.Errorf("there is no tab %d (%d open)", idx, len(tabs))
			}
			return tabs[idx], nil
		}
		if _, given := args["index"]; given {
			return Tab{}, errors.New("index must be a whole number")
		}
	}
	for _, t := range tabs {
		if t.Current {
			return t, nil
		}
	}
	return Tab{}, errors.New("cannot tell which tab is current")
}

// exactHash identifies this precise request: tool, arguments (canonical),
// site, level, mode and the content hash of every upload.
func exactHash(pl *plan) string {
	h := sha256.New()
	put := func(parts ...string) {
		for _, s := range parts {
			fmt.Fprintf(h, "%d:%s|", len(s), s)
		}
	}
	put(pl.Tool, pl.Args.canonical(), pl.Origin.String(), string(pl.Level), pl.Mode)
	for _, u := range pl.Uploads {
		put(u.Ref, u.SHA256)
	}
	return "browser:" + hex.EncodeToString(h.Sum(nil))[:40]
}

// scopes is the "Always allow" ladder for a level on an origin, narrowest
// first. Read requests list interact too, so one saved interact rule covers
// both; uploads and scripts are separate on purpose (they send files or run
// code, which clicking does not).
func scopes(o Origin, l Level) []provider.Scope {
	site := siteName(o)
	read := provider.Scope{Key: ScopeKey(o, LevelRead), Label: "view " + site + " only (no clicking or typing)"}
	interact := provider.Scope{Key: ScopeKey(o, LevelInteract), Label: "view and interact with " + site + " (click, type, submit)"}
	upload := provider.Scope{Key: ScopeKey(o, LevelUpload), Label: "upload files to " + site}
	script := provider.Scope{Key: ScopeKey(o, LevelScript), Label: "run JavaScript on " + site}
	any := provider.Scope{Key: ScopeAny, Label: "anything on ANY website: view, click, type, upload", Broad: true}
	switch l {
	case LevelRead:
		return []provider.Scope{read, interact, any}
	case LevelInteract:
		return []provider.Scope{interact, any}
	case LevelUpload:
		return []provider.Scope{upload, any}
	case LevelScript:
		return []provider.Scope{script, any}
	}
	return nil
}

func callerLabel(c provider.Caller) string {
	switch {
	case c.Agent != "" && c.DeviceName != "":
		return c.Agent + " on " + c.DeviceName
	case c.Agent != "":
		return c.Agent
	case c.DeviceName != "":
		return c.DeviceName
	}
	return "an agent"
}

// build renders the approval prompt for a plan.
func (p *Provider) build(pl *plan, caller provider.Caller) provider.Approval {
	who := callerLabel(caller)
	ap := provider.Approval{Exact: pl.Exact, Auto: pl.Auto, Scopes: scopes(pl.Origin, pl.Level)}
	site := pl.siteLabel()
	switch {
	case pl.Reached != "":
		ap.Title = fmt.Sprintf("%s: the browser went to %s — allow viewing it?", who, site)
	case pl.Origin.IsZero():
		ap.Title = who + " wants to " + lowerFirst(action(pl.Tool, pl.Args))
	case pl.Level == LevelRead:
		ap.Title = fmt.Sprintf("%s wants to view %s in your browser", who, site)
	case pl.Level == LevelInteract:
		ap.Title = fmt.Sprintf("%s wants to use %s in your browser", who, site)
	case pl.Level == LevelUpload:
		ap.Title = fmt.Sprintf("%s wants to upload files to %s in your browser", who, site)
	case pl.Level == LevelScript:
		ap.Title = fmt.Sprintf("%s wants to run JavaScript on %s in your browser", who, site)
	}
	d := func(label, value string) {
		if value != "" {
			ap.Details = append(ap.Details, provider.Detail{Label: label, Value: value})
		}
	}
	if pl.Reached != "" {
		d("Reached by", action(pl.Tool, pl.Args))
		d("Moved to", pl.PageURL)
		d("Held back", "nothing from this page is returned until you decide; denying resets the tab to a blank page")
	} else {
		d("Action", action(pl.Tool, pl.Args))
		if pl.Tool == "browser_evaluate" {
			d("Script", clip(pl.Args.str("function"), 400))
		}
	}
	if !pl.Origin.IsZero() {
		d("Site", pl.Origin.String())
		if pl.PageURL != "" && pl.Reached == "" {
			d("Page", pl.PageURL)
		}
		if pl.Origin.LocalNetwork() {
			d("Network", "this address is on this device or your local network, not the public internet")
		}
	}
	for _, u := range pl.Uploads {
		d("File", fmt.Sprintf("%s (%d bytes, sha256 %s)", u.Ref, u.Size, u.SHA256[:16]))
	}
	d("Browser", modeDescription(pl.Mode))
	return ap
}

// Approval implements provider.Gated. It describes the call and remembers
// what was approved so Call can tell whether the page changed in between.
func (p *Provider) Approval(ctx context.Context, tool string, args json.RawMessage, caller provider.Caller) (provider.Approval, error) {
	pl, err := p.plan(ctx, tool, args)
	if err != nil {
		return provider.Approval{}, err
	}
	ap := p.build(pl, caller)
	p.remember(caller, tool, args, pl.Exact)
	return ap, nil
}

// --- remembering what the gate approved --------------------------------------

type approvedCall struct {
	exact string
	until time.Time
}

const approvalTTL = 15 * time.Minute

func approvalKey(c provider.Caller, tool string, args json.RawMessage) string {
	a, err := parseArgs(args)
	canon := string(args)
	if err == nil {
		canon = a.canonical()
	}
	sum := sha256.Sum256([]byte(c.DeviceID + "\x00" + c.Agent + "\x00" + tool + "\x00" + canon))
	return hex.EncodeToString(sum[:])
}

func (p *Provider) remember(c provider.Caller, tool string, args json.RawMessage, exact string) {
	p.apprMu.Lock()
	defer p.apprMu.Unlock()
	now := time.Now()
	for k, list := range p.approved {
		keep := list[:0]
		for _, e := range list {
			if e.until.After(now) {
				keep = append(keep, e)
			}
		}
		if len(keep) == 0 {
			delete(p.approved, k)
		} else {
			p.approved[k] = keep
		}
	}
	k := approvalKey(c, tool, args)
	p.approved[k] = append(p.approved[k], approvedCall{exact: exact, until: now.Add(approvalTTL)})
}

// consume reports whether the gate approved exactly this plan for this
// caller, and uses the approval up.
func (p *Provider) consume(c provider.Caller, tool string, args json.RawMessage, exact string) bool {
	p.apprMu.Lock()
	defer p.apprMu.Unlock()
	k := approvalKey(c, tool, args)
	list := p.approved[k]
	for i, e := range list {
		if e.exact == exact && e.until.After(time.Now()) {
			p.approved[k] = append(list[:i], list[i+1:]...)
			return true
		}
	}
	return false
}
