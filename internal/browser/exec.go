package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/provider"
)

// acquire serialises browser actions: there is one browser and one current
// tab, so two agents must not interleave a click and the check after it.
func (p *Provider) acquire(ctx context.Context) (release func(), err error) {
	select {
	case p.busy <- struct{}{}:
		return func() { <-p.busy }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// listTabs asks the browser which tabs are open. It never starts the browser:
// with none running there is no page.
func (p *Provider) listTabs(ctx context.Context) ([]Tab, error) {
	if p.up.running() == nil {
		return nil, errNoPage
	}
	res, err := p.up.call(ctx, p.config(), "browser_tabs", json.RawMessage(`{"action":"list"}`))
	if err != nil {
		return nil, fmt.Errorf("cannot tell which website the browser is showing: %v", err)
	}
	tabs, err := ParseTabs(textOf(res))
	if err != nil {
		return nil, fmt.Errorf("cannot tell which website the browser is showing: %v", err)
	}
	return tabs, nil
}

// Call implements provider.Provider.
func (p *Provider) Call(ctx context.Context, tool string, args json.RawMessage, caller provider.Caller) (*mcp.CallToolResult, error) {
	if tool == ToolStatus {
		return p.statusResult(ctx)
	}
	release, err := p.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	defer p.up.begin()() // keep the idle timer off while an approval is pending

	pl, err := p.plan(ctx, tool, args)
	if err != nil {
		return provider.ErrorResult("%v", err), nil
	}
	// The gate approved the page as it was; if the page is somewhere else now
	// (a timer, another agent), that approval does not cover this.
	if !p.consume(caller, tool, args, pl.Exact) {
		if pl.Auto == "" {
			ok, reason := p.askAgain(ctx, caller, pl, "the page changed after you approved this request")
			if !ok {
				return provider.ErrorResult("not approved: %s", reason), nil
			}
		}
	}
	return p.execute(ctx, caller, pl)
}

// askAgain requests a fresh approval for pl mid-call.
func (p *Provider) askAgain(ctx context.Context, caller provider.Caller, pl *plan, why string) (bool, string) {
	if p.ask == nil {
		return false, "this device cannot ask for approval now"
	}
	ap := p.build(pl, caller)
	if pl.Reached == "" {
		ap.Details = append([]provider.Detail{{Label: "Why again", Value: why}}, ap.Details...)
	}
	ok, reason, err := p.ask(ctx, caller, pl.Tool, ap)
	if err != nil {
		return false, err.Error()
	}
	return ok, reason
}

// execute runs an approved plan and vets where the browser ended up.
func (p *Provider) execute(ctx context.Context, caller provider.Caller, pl *plan) (*mcp.CallToolResult, error) {
	cfg := p.config()
	raw := pl.Raw
	if len(pl.Uploads) > 0 {
		staged, cleanup, err := stageUploads(filepath.Join(p.up.workDir(), "uploads"), pl.Uploads)
		defer cleanup()
		if err != nil {
			return provider.ErrorResult("%v", err), nil
		}
		paths := make([]string, len(staged))
		for i, s := range staged {
			paths[i] = s.staged
		}
		raw, _ = json.Marshal(map[string]any{"paths": paths})
	}
	var vetNotes []string
	if pl.Spec.Check && pl.Tabs == nil {
		// Calls that name their target (navigate, new tab) did not list the
		// tabs while planning; vetting needs the "before" picture.
		pl.Tabs, _ = p.listTabs(ctx)
	}
	res, err := p.up.call(ctx, cfg, pl.Tool, raw)
	if err != nil {
		return provider.ErrorResult("%v", err), nil
	}
	if pl.Spec.Check {
		withhold, notes := p.vet(ctx, caller, pl)
		if withhold != "" {
			p.discard(res)
			return provider.ErrorResult("%s", withhold), nil
		}
		vetNotes = notes
	}
	out, _, cerr := convert(p.paths, p.up.outDir(), res, pl.Tool == "browser_tabs")
	if cerr != nil {
		p.log.Warn("saving browser output", "error", cerr)
	}
	for _, n := range vetNotes {
		out.Content = append(out.Content, &mcp.TextContent{Text: "Note: " + n})
	}
	return out, nil
}

// discard deletes files the browser wrote for a result that is being withheld.
func (p *Provider) discard(res *mcp.CallToolResult) {
	c := &resultCleaner{paths: p.paths, outDir: p.up.outDir()}
	for _, content := range res.Content {
		if t, ok := content.(*mcp.TextContent); ok {
			for _, m := range mdLink.FindAllStringSubmatch(t.Text, -1) {
				if abs, ok := c.insideOut(m[2]); ok {
					os.Remove(abs)
				}
			}
		}
	}
}

// vet checks where the browser is after a call that can change the page. The
// result is withheld (withhold is its replacement text) unless the current
// tab belongs to a site approved for viewing; a new tab that is not approved
// is closed and reported in notes. Popups, new tabs and redirect targets are
// all just tabs with an origin.
func (p *Provider) vet(ctx context.Context, caller provider.Caller, pl *plan) (withhold string, notes []string) {
	cfg := p.config()
	tabs, err := p.listTabs(ctx)
	if err != nil {
		// Cannot tell where the page is: do not return its content.
		p.resetCurrent()
		return fmt.Sprintf("the result was withheld because the browser's state could not be checked (%v); the tab was reset to a blank page", err), nil
	}
	var denied []Tab
	var why []string
	cleared := map[Origin]bool{pl.Origin: !pl.Origin.IsZero()}
	for _, t := range tabs {
		if !pl.flagged(t) {
			continue
		}
		switch {
		case t.Inert():
			continue
		case t.Scheme != "":
			denied = append(denied, t)
			why = append(why, fmt.Sprintf("the browser went to a %s page, which messh does not allow", t.Scheme))
			continue
		case cleared[t.Origin]:
			continue
		case MatchAny(cfg.DenyOrigins, true, t.Origin):
			denied = append(denied, t)
			why = append(why, fmt.Sprintf("the browser went to %s, which is blocked on this device (deny_origins)", siteName(t.Origin)))
			continue
		case MatchAny(cfg.AllowOrigins, false, t.Origin):
			cleared[t.Origin] = true
			continue
		}
		reached := &plan{Tool: pl.Tool, Args: pl.Args, Level: LevelRead, Origin: t.Origin, Mode: pl.Mode, Reached: t.Origin.String()}
		_, reached.PageURL, _ = ParseURL(t.URL)
		reached.Exact = exactHash(reached)
		ok, reason := p.askAgain(ctx, caller, reached, "the page moved to a site you have not approved")
		if ok {
			cleared[t.Origin] = true
			continue
		}
		denied = append(denied, t)
		why = append(why, fmt.Sprintf("the browser went to %s, which was not approved (%s)", siteName(t.Origin), reason))
	}
	if len(denied) == 0 {
		return "", nil
	}
	type item struct {
		tab Tab
		why string
	}
	items := make([]item, len(denied))
	for i := range denied {
		items[i] = item{denied[i], why[i]}
	}
	// Close extra tabs from the end so indexes stay valid; the current tab is
	// sent to a blank page.
	slices.SortFunc(items, func(a, b item) int { return b.tab.Index - a.tab.Index })
	var reasons []string
	for _, it := range items {
		p.remediate(it.tab)
		if it.tab.Current {
			reasons = append(reasons, it.why)
		} else {
			notes = append(notes, it.why+"; that tab was closed")
		}
	}
	if len(reasons) > 0 {
		return "navigated away from the approved site: " + strings.Join(reasons, "; ") + ". Nothing from that page was returned and the tab was reset.", nil
	}
	return "", notes
}

// flagged reports whether a tab needs vetting: the current tab, a tab that
// was not there before the call, or one whose site changed.
func (pl *plan) flagged(t Tab) bool {
	if t.Current {
		return true
	}
	if t.Index >= len(pl.Tabs) {
		return true
	}
	b := pl.Tabs[t.Index]
	return b.Origin != t.Origin || b.Scheme != t.Scheme
}

// remediate removes an unapproved page from the browser: extra tabs are
// closed, the current tab is sent to about:blank.
func (p *Provider) remediate(t Tab) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cfg := p.config()
	var args string
	var tool string
	if t.Current {
		tool, args = "browser_navigate", `{"url":"about:blank"}`
	} else {
		tool, args = "browser_tabs", fmt.Sprintf(`{"action":"close","index":%d}`, t.Index)
	}
	if _, err := p.up.call(ctx, cfg, tool, json.RawMessage(args)); err != nil {
		p.log.Warn("could not reset an unapproved tab; stopping the browser", "error", err)
		p.up.stop()
	}
}

// resetCurrent blanks the current tab without knowing what it shows.
func (p *Provider) resetCurrent() {
	p.remediate(Tab{Current: true})
}
