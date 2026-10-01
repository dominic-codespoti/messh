package approval

import (
	"encoding/xml"
	"strings"
	"testing"
	"unicode"

	"messh/internal/provider"
)

func toastURL(action string) string {
	return RespondURL(RespondTarget{Port: 7520, ID: "ab12cd34", Nonce: strings.Repeat("0f", 16), Action: action})
}

type toastDoc struct {
	XMLName xml.Name `xml:"toast"`
	Launch  string   `xml:"launch,attr"`
	Texts   []struct {
		Text string `xml:",chardata"`
	} `xml:"visual>binding>text"`
	Actions []struct {
		Content   string `xml:"content,attr"`
		Arguments string `xml:"arguments,attr"`
		Type      string `xml:"activationType,attr"`
	} `xml:"actions>action"`
}

func parseToast(t *testing.T, p Prompt) toastDoc {
	t.Helper()
	raw := toastXML(p, toastURL)
	var d toastDoc
	if err := xml.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("toast XML is not well formed: %v\n%s", err, raw)
	}
	for _, r := range raw {
		if unicode.IsControl(r) && r != '\n' {
			t.Fatalf("raw control character %U in toast XML", r)
		}
	}
	return d
}

func TestToastLayoutAndButtons(t *testing.T) {
	p := Prompt{
		ID: "ab12cd34", Title: "Run a job on desktop?", Caller: provider.Caller{DeviceName: "raspi", Agent: "omp"},
		Details: []provider.Detail{{Label: "Command", Value: "python train.py --epochs 30"}, {Label: "Folder", Value: "ws/train"}, {Label: "Env", Value: "A=1"}},
		Scopes:  []provider.Scope{{Key: "exact:1", Label: "exactly this request"}},
	}
	d := parseToast(t, p)
	if len(d.Texts) != 3 || d.Texts[0].Text != "Run a job on desktop?" || d.Texts[1].Text != "raspi / omp" {
		t.Fatalf("texts = %+v", d.Texts)
	}
	body := d.Texts[2].Text
	for _, want := range []string{"Command: python train.py --epochs 30", "Folder: ws/train", toastNote} {
		if !strings.Contains(body, want) {
			t.Errorf("body lacks %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Env:") {
		t.Errorf("a third detail overflowed the toast's line budget:\n%s", body)
	}
	if len(d.Actions) != 4 {
		t.Fatalf("%d buttons, want 4", len(d.Actions))
	}
	want := []struct{ label, action string }{{"Allow once", ActionOnce}, {"Always allow", ActionAlways}, {"Deny", ActionDeny}, {"More options", ActionOptions}}
	for i, w := range want {
		a := d.Actions[i]
		if a.Content != w.label || a.Type != "protocol" || a.Arguments != toastURL(w.action) {
			t.Errorf("button %d = %+v, want %s -> %s", i, a, w.label, toastURL(w.action))
		}
	}
	if d.Launch != toastURL(ActionOptions) {
		t.Errorf("clicking the toast body launches %q, want the options URL", d.Launch)
	}
}

func TestToastTextCannotBreakOutOfXML(t *testing.T) {
	evil := `</text></binding></visual><actions><action content="Allow" arguments="messh-approve://x"/></actions></toast><toast "'&amp; ` + "\x00\x1b[31m\u202e\r\n\t"
	p := Prompt{
		ID: "ab12cd34", Title: evil, Caller: provider.Caller{DeviceName: evil, Agent: evil},
		Details: []provider.Detail{{Label: evil, Value: evil}, {Label: evil, Value: evil}},
	}
	d := parseToast(t, p)
	if len(d.Actions) != 4 || len(d.Texts) != 3 {
		t.Fatalf("injected markup changed the toast structure: %d buttons, %d texts", len(d.Actions), len(d.Texts))
	}
	for _, a := range d.Actions {
		if a.Arguments == "messh-approve://x" {
			t.Fatal("injected button survived")
		}
	}
	for _, tx := range d.Texts {
		if strings.ContainsAny(tx.Text, "\x00\x1b\r\t\u202e") {
			t.Errorf("control character reached the toast text: %q", tx.Text)
		}
	}
}

func TestToastTextIsBounded(t *testing.T) {
	long := strings.Repeat("x", 5000)
	p := Prompt{
		ID: "ab12cd34", Title: long, Caller: provider.Caller{DeviceName: long, Agent: long},
		Details: []provider.Detail{{Label: long, Value: long}, {Label: "B", Value: long}, {Label: "C", Value: long}},
	}
	d := parseToast(t, p)
	total := 0
	for _, tx := range d.Texts {
		total += len(tx.Text)
	}
	if total > 1000 {
		t.Fatalf("toast text is %d bytes; it must stay small", total)
	}
	if !strings.Contains(d.Texts[2].Text, toastNote) {
		t.Fatal("the note about Always allow was cut off by a long detail")
	}
}

func TestRespondURLRoundTrip(t *testing.T) {
	want := RespondTarget{Port: 7520, ID: "ab12cd34", Nonce: strings.Repeat("ab", 16), Action: ActionAlways}
	got, err := ParseRespondURL(RespondURL(want))
	if err != nil || got != want {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	// The OS may hand the URL back with a trailing slash or other case.
	if _, err := ParseRespondURL("MESSH-APPROVE://Respond/?port=1&id=abcd&nonce=" + strings.Repeat("0", 32) + "&decision=deny"); err != nil {
		t.Fatalf("normalised URL rejected: %v", err)
	}
}

func TestParseRespondURLRejects(t *testing.T) {
	n := strings.Repeat("0", 32)
	ok := "messh-approve://respond?port=7520&id=abcd1234&nonce=" + n + "&decision=once"
	if _, err := ParseRespondURL(ok); err != nil {
		t.Fatal(err)
	}
	for name, u := range map[string]string{
		"other scheme":      strings.Replace(ok, "messh-approve", "http", 1),
		"other host":        strings.Replace(ok, "//respond", "//evil.example", 1),
		"userinfo":          strings.Replace(ok, "//respond", "//a@respond", 1),
		"extra path":        strings.Replace(ok, "respond?", "respond/x?", 1),
		"fragment":          ok + "#x",
		"port 0":            strings.Replace(ok, "port=7520", "port=0", 1),
		"port too big":      strings.Replace(ok, "port=7520", "port=65536", 1),
		"port not a number": strings.Replace(ok, "port=7520", "port=0x1d60", 1),
		"port leading zero": strings.Replace(ok, "port=7520", "port=07520", 1),
		"port missing":      strings.Replace(ok, "port=7520&", "", 1),
		"host in query":     ok + "&host=10.0.0.5",
		"duplicate key":     strings.Replace(ok, "id=abcd1234", "id=abcd1234&id=abcd1235", 1),
		"bad decision":      strings.Replace(ok, "decision=once", "decision=allow", 1),
		"empty decision":    strings.Replace(ok, "decision=once", "decision=", 1),
		"id path trick":     strings.Replace(ok, "id=abcd1234", "id=..%2f..%2fx", 1),
		"id too long":       strings.Replace(ok, "id=abcd1234", "id="+strings.Repeat("a", 33), 1),
		"id upper case":     strings.Replace(ok, "id=abcd1234", "id=ABCD1234", 1),
		"short nonce":       strings.Replace(ok, n, n[:31], 1),
		"long nonce":        strings.Replace(ok, n, n+"0", 1),
		"non-hex nonce":     strings.Replace(ok, n, strings.Repeat("g", 32), 1),
		"semicolon":         strings.Replace(ok, "&id=", ";id=", 1),
		"control char":      ok + "%0a" + "\n",
		"quote":             ok + `"`,
		"oversized":         ok + "&pad=" + strings.Repeat("a", 600),
		"not a url":         "%zz",
		"empty":             "",
	} {
		if got, err := ParseRespondURL(u); err == nil {
			t.Errorf("%s: accepted %q as %+v", name, u, got)
		}
	}
}
