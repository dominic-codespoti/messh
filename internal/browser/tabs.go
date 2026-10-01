package browser

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Tab is one open browser tab as listed by browser_tabs.
type Tab struct {
	Index   int
	Current bool
	URL     string // raw page URL as the browser reports it
	Title   string
	Origin  Origin // zero for inert pages (about:blank, error pages)
	// Scheme is the URL scheme when the page is not a web page (and not inert).
	Scheme string
}

var tabLine = regexp.MustCompile(`^- (\d+):( \(current\))? \[(.*)\]\((.*)\)( \[crashed\])?$`)

// ParseTabs reads the "### Result" list of browser_tabs. Titles are chosen by
// the page and may contain "](", so a line whose title/URL boundary is
// ambiguous between different origins is refused rather than guessed:
// the origin decides what the agent may see.
func ParseTabs(text string) ([]Tab, error) {
	body, ok := section(text, "Result")
	if !ok {
		return nil, errors.New("tab list has no result section")
	}
	body = strings.TrimSpace(body)
	if strings.HasPrefix(body, "No open tabs") || body == "" {
		return nil, nil
	}
	var tabs []Tab
	current := 0
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		t, err := parseTabLine(line)
		if err != nil {
			return nil, err
		}
		if t.Index != len(tabs) {
			return nil, fmt.Errorf("tab list is out of order at %q", clip(line, 60))
		}
		if t.Current {
			current++
		}
		tabs = append(tabs, t)
	}
	if len(tabs) > 0 && current != 1 {
		return nil, fmt.Errorf("tab list marks %d tabs as current", current)
	}
	return tabs, nil
}

func parseTabLine(line string) (Tab, error) {
	m := tabLine.FindStringSubmatch(line)
	if m == nil {
		return Tab{}, fmt.Errorf("cannot read tab line %q", clip(line, 60))
	}
	idx, _ := strconv.Atoi(m[1])
	t := Tab{Index: idx, Current: m[2] != ""}
	// Every "](" is a possible title/URL boundary. A reading whose URL does
	// not parse as an absolute URL is impossible (Chrome serialises URLs
	// without spaces); the remaining readings must agree on the site.
	rest := m[3] + "](" + m[4]
	type reading struct {
		title, url, ident string
		origin            Origin
		scheme            string
	}
	var readings []reading
	for i := 0; i+1 < len(rest); i++ {
		if rest[i] != ']' || rest[i+1] != '(' {
			continue
		}
		u := rest[i+2:]
		if strings.ContainsAny(u, " \t") {
			continue
		}
		r := reading{title: rest[:i], url: u}
		if classifyInert(u) != "" {
			r.ident = "inert"
		} else if o, _, err := ParseURL(u); err == nil {
			r.origin, r.ident = o, o.String()
		} else if se := (*SchemeError)(nil); errors.As(err, &se) {
			r.scheme, r.ident = se.Scheme, se.Scheme
		} else {
			continue
		}
		readings = append(readings, r)
	}
	if len(readings) == 0 {
		return Tab{}, fmt.Errorf("cannot find the page address in tab line %q", clip(line, 60))
	}
	last := readings[len(readings)-1]
	for _, r := range readings {
		if r.ident != last.ident {
			return Tab{}, fmt.Errorf("the title of tab %d contains \"](\" and its page address is ambiguous; refusing to guess which site it is", idx)
		}
	}
	t.Title, t.URL, t.Origin, t.Scheme = last.title, last.url, last.origin, last.scheme
	return t, nil
}

// classifyInert names harmless blank pages: about:blank and Chrome's network
// error page. Anything else that is not http(s) is "not a web page".
func classifyInert(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "about:blank":
		return "blank"
	}
	if strings.HasPrefix(strings.ToLower(raw), "chrome-error://") {
		return "error"
	}
	return ""
}

// Inert reports a blank or error page with nothing to protect.
func (t Tab) Inert() bool { return t.Origin.IsZero() && t.Scheme == "" }

// section returns the body of "### name" in a Playwright result.
func section(text, name string) (string, bool) {
	marker := "### " + name + "\n"
	i := -1
	if strings.HasPrefix(text, marker) {
		i = 0
	} else if j := strings.Index(text, "\n"+marker); j >= 0 {
		i = j + 1
	}
	if i < 0 {
		return "", false
	}
	body := text[i+len(marker):]
	if j := strings.Index(body, "\n### "); j >= 0 {
		body = body[:j]
	}
	return body, true
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

var pageURLLine = regexp.MustCompile(`(?m)^- Page URL: (.+)$`)

// PageURL reads the "- Page URL:" line of the Page section.
func PageURL(text string) (string, bool) {
	body, ok := section(text, "Page")
	if !ok {
		return "", false
	}
	m := pageURLLine.FindStringSubmatch(body)
	if m == nil {
		return "", false
	}
	return strings.TrimSpace(m[1]), true
}
