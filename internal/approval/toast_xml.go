package approval

import (
	"strings"
	"unicode/utf8"
)

// Toast layout limits, found by looking at real toasts: Windows shows the
// title plus about four lines of body text, at roughly 52 characters a line,
// and drops the rest. Everything below stays inside that so the Always-allow
// note is never the part that gets cut off.
const (
	toastLineChars  = 52
	toastBodyLines  = 3 // who + details; the note takes the fourth line
	toastTitleChars = 80
	toastValueChars = 104
)

// toastNote tells the person what the Always allow button saves.
const toastNote = "Always allow = this exact request only"

// toastWho renders who is asking as `raspi / omp`.
func toastWho(p Prompt) string {
	name := p.Caller.DeviceName
	if name == "" {
		name = p.Caller.DeviceID
	}
	who := Sanitize(name, 40)
	if p.Caller.Agent != "" {
		who += " / " + Sanitize(p.Caller.Agent, 40)
	}
	return who
}

// toastLines returns the three text elements of a toast: title, who, and
// the most important details followed by the note. All of it is plain text
// (XML escaping happens in toastXML) and free of control characters.
func toastLines(p Prompt) (title, who, body string) {
	title = Sanitize(p.Title, toastTitleChars)
	who = toastWho(p)
	used := 1 // the who line is counted against the body budget
	var lines []string
	for _, d := range p.Details {
		line := Sanitize(d.Label, 24) + ": " + Sanitize(d.Value, toastValueChars)
		n := (utf8.RuneCountInString(line) + toastLineChars - 1) / toastLineChars
		if len(lines) > 0 && used+n > toastBodyLines {
			break
		}
		lines = append(lines, line)
		used += n
		if used >= toastBodyLines {
			break
		}
	}
	lines = append(lines, toastNote)
	return title, who, strings.Join(lines, "\n")
}

var xmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")

// escapeXML escapes text for use as XML content or an attribute value.
// Sanitize has already removed control characters; the newline that joins
// body lines is the one character allowed through.
func escapeXML(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || (r >= ' ' && r != 0x7f && r != utf8.RuneError && r != 0xFFFE && r != 0xFFFF) {
			return r
		}
		return ' '
	}, s)
	return xmlEscaper.Replace(s)
}

// toastXML renders the toast for p. url maps an Action* constant to the URL
// the matching button (or, for ActionOptions, the toast body) launches.
// Four buttons, never more than the five Windows allows.
func toastXML(p Prompt, url func(action string) string) string {
	title, who, body := toastLines(p)
	var b strings.Builder
	b.WriteString(`<toast scenario="reminder" activationType="protocol" launch="`)
	b.WriteString(escapeXML(url(ActionOptions)))
	b.WriteString(`"><visual><binding template="ToastGeneric"><text>`)
	b.WriteString(escapeXML(title))
	b.WriteString(`</text><text>`)
	b.WriteString(escapeXML(who))
	b.WriteString(`</text><text hint-maxLines="4">`)
	b.WriteString(escapeXML(body))
	b.WriteString(`</text></binding></visual><actions>`)
	for _, a := range []struct{ label, action string }{
		{"Allow once", ActionOnce},
		{"Always allow", ActionAlways},
		{"Deny", ActionDeny},
		{"More options", ActionOptions},
	} {
		b.WriteString(`<action content="`)
		b.WriteString(a.label)
		b.WriteString(`" activationType="protocol" arguments="`)
		b.WriteString(escapeXML(url(a.action)))
		b.WriteString(`"/>`)
	}
	b.WriteString(`</actions></toast>`)
	return b.String()
}
