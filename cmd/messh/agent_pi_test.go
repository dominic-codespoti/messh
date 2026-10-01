package main

import (
	"strings"
	"testing"
)

func TestPiInstructionsDefault(t *testing.T) {
	out := piInstructions("pi", "messh agent token pi --bearer", "http://127.0.0.1:7520/mcp")
	for _, want := range []string{
		"pi install ./integrations/pi",
		"Nothing to configure",
		"/messh",
		"mesh_nodes",
		"integrations/pi/README.md",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("default instructions lack %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"messh.json", "MESSH_STATE", "MESSH_URL", "MESSH_BIN"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("default instructions mention %q although nothing differs from the defaults:\n%s", unwanted, out)
		}
	}
}

func TestPiInstructionsOtherAgentNeedsOnlyAgent(t *testing.T) {
	out := piInstructions("raspi-pi", "messh agent token raspi-pi --bearer", "http://127.0.0.1:7520/mcp")
	if !strings.Contains(out, `{"agent":"raspi-pi"}`) || !strings.Contains(out, "MESSH_AGENT=raspi-pi") {
		t.Errorf("agent name missing from config:\n%s", out)
	}
	for _, unwanted := range []string{"MESSH_STATE", "MESSH_URL", "MESSH_BIN", "Nothing to configure"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("instructions mention %q:\n%s", unwanted, out)
		}
	}
}

func TestPiInstructionsNonDefaultStateAndURL(t *testing.T) {
	out := piInstructions("pi",
		tokenCommand("pi", true, `C:\Users\Test'user\messh state`),
		"http://127.0.0.1:7530/mcp")
	for _, want := range []string{
		`{"agent":"pi","url":"http://127.0.0.1:7530/mcp","state":"C:/Users/Test'user/messh state"}`,
		"MESSH_URL=http://127.0.0.1:7530/mcp",
		`MESSH_STATE=C:/Users/Test'user/messh state`,
		"~/.pi/agent/messh.json",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("instructions lack %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Nothing to configure") || strings.Contains(out, "MESSH_BIN") {
		t.Errorf("unexpected default/bin text:\n%s", out)
	}
}

func TestPiInstructionsCustomBinary(t *testing.T) {
	out := piInstructions("pi", "/opt/messh/bin/messh agent token pi --bearer -state=/srv/messh", "http://127.0.0.1:7520/mcp")
	for _, want := range []string{
		`"messh":"/opt/messh/bin/messh"`,
		`"state":"/srv/messh"`,
		"MESSH_BIN=/opt/messh/bin/messh",
		"MESSH_STATE=/srv/messh",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("instructions lack %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "MESSH_URL") {
		t.Errorf("default URL should not be configured:\n%s", out)
	}
}
