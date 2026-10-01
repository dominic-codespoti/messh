package browser

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Level is how much a call does to a website. A saved rule for a higher
// level (interact) also satisfies requests for a lower one (read).
type Level string

const (
	LevelNone     Level = ""         // touches no website (close the browser, blank tab)
	LevelRead     Level = "read"     // view the page: snapshot, screenshot, console, network
	LevelInteract Level = "interact" // click, type, submit, navigate history
	LevelUpload   Level = "upload"   // send files from this device to the site
	LevelScript   Level = "script"   // run JavaScript in the page
)

// kind says which page a call acts on.
type kind int

const (
	onPage     kind = iota // the current tab
	onURL                  // the site named by the url argument
	onTab                  // the tab named by the index argument
	onNothing              // no page involved
	onTabsList             // lists tabs; answered with origins only
)

// toolSpec is the fixed policy for one published tool.
type toolSpec struct {
	Level Level
	Kind  kind
	// Check says the call can change what the tab shows, so the page that
	// results is vetted before any of its content is returned.
	Check bool
}

// specs is the allowlist: only these upstream tools are ever published or
// callable. Everything else (browser_run_code_unsafe, install, config,
// network interception, storage, devtools, tracing, video) is absent on purpose.
var specs = map[string]toolSpec{
	"browser_snapshot":         {LevelRead, onPage, true},
	"browser_take_screenshot":  {LevelRead, onPage, true},
	"browser_find":             {LevelRead, onPage, true},
	"browser_console_messages": {LevelRead, onPage, true},
	"browser_network_requests": {LevelRead, onPage, true},
	"browser_network_request":  {LevelRead, onPage, true},
	"browser_wait_for":         {LevelRead, onPage, true},
	"browser_pdf_save":         {LevelRead, onPage, true},

	"browser_click":          {LevelInteract, onPage, true},
	"browser_type":           {LevelInteract, onPage, true},
	"browser_fill_form":      {LevelInteract, onPage, true},
	"browser_select_option":  {LevelInteract, onPage, true},
	"browser_press_key":      {LevelInteract, onPage, true},
	"browser_hover":          {LevelInteract, onPage, true},
	"browser_drag":           {LevelInteract, onPage, true},
	"browser_handle_dialog":  {LevelInteract, onPage, true},
	"browser_navigate_back":  {LevelInteract, onPage, true},
	"browser_resize":         {LevelInteract, onPage, true},
	"browser_mouse_click_xy": {LevelInteract, onPage, true},
	"browser_mouse_move_xy":  {LevelInteract, onPage, true},
	"browser_mouse_drag_xy":  {LevelInteract, onPage, true},
	"browser_mouse_down":     {LevelInteract, onPage, true},
	"browser_mouse_up":       {LevelInteract, onPage, true},
	"browser_mouse_wheel":    {LevelInteract, onPage, true},

	"browser_file_upload": {LevelUpload, onPage, true},
	"browser_evaluate":    {LevelScript, onPage, true},

	"browser_navigate": {LevelRead, onURL, true},
	"browser_close":    {LevelNone, onNothing, false},
	// browser_tabs is classified per action by tabsSpec.
}

// tabsSpec classifies browser_tabs by its action.
func tabsSpec(action string, hasURL bool) (toolSpec, error) {
	switch action {
	case "list":
		return toolSpec{LevelNone, onTabsList, false}, nil
	case "new":
		if hasURL {
			return toolSpec{LevelRead, onURL, true}, nil
		}
		return toolSpec{LevelNone, onNothing, false}, nil
	case "select":
		return toolSpec{LevelRead, onTab, false}, nil
	case "close":
		return toolSpec{LevelInteract, onTab, false}, nil
	}
	return toolSpec{}, fmt.Errorf("browser_tabs: unknown action %q (use list, new, select or close)", action)
}

// Published reports whether tool is on the allowlist (browser_tabs included).
func Published(tool string) bool {
	_, ok := specs[tool]
	return ok || tool == "browser_tabs"
}

// allowlisted returns the allowlist sorted by name.
func allowlisted() []string {
	names := []string{"browser_tabs"}
	for n := range specs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// callArgs is the parsed argument object of a call.
type callArgs map[string]any

// parseArgs decodes args into an object and rejects parameters messh does not
// pass through. `filename` would let an agent choose where the server writes.
func parseArgs(raw json.RawMessage) (callArgs, error) {
	a := callArgs{}
	if len(raw) == 0 || string(raw) == "null" {
		return a, nil
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("arguments must be a JSON object: %v", err)
	}
	if _, ok := a["filename"]; ok {
		return nil, fmt.Errorf("filename is not supported: screenshots and PDFs are saved as artifacts and returned as artifacts/browser/... references")
	}
	return a, nil
}

func (a callArgs) str(key string) string {
	s, _ := a[key].(string)
	return s
}

func (a callArgs) num(key string) (int, bool) {
	f, ok := a[key].(float64)
	if !ok || f != float64(int(f)) {
		return 0, false
	}
	return int(f), true
}

func (a callArgs) strs(key string) []string {
	arr, _ := a[key].([]any)
	var out []string
	for _, v := range arr {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// canonical renders the arguments with sorted keys so the same request always
// hashes the same, whatever the client's key order or spacing.
func (a callArgs) canonical() string {
	b, _ := json.Marshal(map[string]any(a))
	return string(b)
}

// action describes what a call does, for the approval prompt.
func action(tool string, a callArgs) string {
	elem := func() string {
		if e := a.str("element"); e != "" {
			return "“" + clip(e, 80) + "”"
		}
		if t := a.str("target"); t != "" {
			return "element " + clip(t, 80)
		}
		return "an element"
	}
	switch tool {
	case "browser_navigate":
		return "Open " + shortURL(a.str("url"))
	case "browser_navigate_back":
		return "Go back in the page history"
	case "browser_snapshot":
		return "Read the page (accessibility snapshot)"
	case "browser_take_screenshot":
		return "Take a screenshot"
	case "browser_find":
		return "Search the page for " + clip(firstNonEmpty(a.str("text"), a.str("regex")), 80)
	case "browser_console_messages":
		return "Read the page's console messages"
	case "browser_network_requests":
		return "List the page's network requests"
	case "browser_network_request":
		return "Read the details of a network request"
	case "browser_wait_for":
		switch {
		case a.str("text") != "":
			return "Wait for the text “" + clip(a.str("text"), 60) + "” to appear"
		case a.str("textGone") != "":
			return "Wait for the text “" + clip(a.str("textGone"), 60) + "” to disappear"
		}
		return "Wait for the page"
	case "browser_pdf_save":
		return "Save the page as a PDF"
	case "browser_click":
		verb := "Click "
		if b, _ := a["doubleClick"].(bool); b {
			verb = "Double-click "
		}
		return verb + elem()
	case "browser_type":
		txt := a.str("text")
		s := fmt.Sprintf("Type %d characters into %s: %q", len([]rune(txt)), elem(), clip(txt, 80))
		if b, _ := a["submit"].(bool); b {
			s += ", then press Enter"
		}
		return s
	case "browser_fill_form":
		var names []string
		if arr, ok := a["fields"].([]any); ok {
			for _, f := range arr {
				if m, ok := f.(map[string]any); ok {
					n, _ := m["name"].(string)
					if n == "" {
						n, _ = m["element"].(string)
					}
					names = append(names, clip(n, 40))
				}
			}
		}
		return fmt.Sprintf("Fill %d form fields (%s)", len(names), clip(strings.Join(names, ", "), 120))
	case "browser_select_option":
		return "Select " + clip(strings.Join(a.strs("values"), ", "), 80) + " in " + elem()
	case "browser_press_key":
		return "Press the key " + clip(a.str("key"), 40)
	case "browser_hover":
		return "Hover over " + elem()
	case "browser_drag":
		return "Drag " + firstNonEmpty(clip(a.str("startElement"), 60), clip(a.str("startTarget"), 60)) + " to " + firstNonEmpty(clip(a.str("endElement"), 60), clip(a.str("endTarget"), 60))
	case "browser_handle_dialog":
		if b, _ := a["accept"].(bool); b {
			return "Accept the page's dialog"
		}
		return "Dismiss the page's dialog"
	case "browser_resize":
		return "Resize the browser window"
	case "browser_mouse_click_xy":
		return fmt.Sprintf("Click at position %v, %v", a["x"], a["y"])
	case "browser_mouse_move_xy":
		return fmt.Sprintf("Move the mouse to %v, %v", a["x"], a["y"])
	case "browser_mouse_drag_xy":
		return fmt.Sprintf("Drag the mouse from %v, %v to %v, %v", a["startX"], a["startY"], a["endX"], a["endY"])
	case "browser_mouse_down", "browser_mouse_up":
		return "Press or release a mouse button"
	case "browser_mouse_wheel":
		return "Scroll with the mouse wheel"
	case "browser_file_upload":
		return "Upload files to the page"
	case "browser_evaluate":
		return "Run JavaScript in the page"
	case "browser_close":
		return "Close the browser"
	case "browser_tabs":
		switch a.str("action") {
		case "list":
			return "List the open tabs"
		case "new":
			if u := a.str("url"); u != "" {
				return "Open " + shortURL(u) + " in a new tab"
			}
			return "Open a blank tab"
		case "select":
			return fmt.Sprintf("Switch to tab %v", a["index"])
		case "close":
			if _, ok := a["index"]; ok {
				return fmt.Sprintf("Close tab %v", a["index"])
			}
			return "Close the current tab"
		}
	}
	return tool
}

// shortURL strips credentials, query and fragment from a URL for display.
func shortURL(raw string) string {
	if _, clean, err := ParseURL(raw); err == nil {
		return clean
	}
	return clip(raw, 100)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

func lowerFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToLower(s[:1]) + s[1:]
}
