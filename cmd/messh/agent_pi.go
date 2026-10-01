package main

import (
	"encoding/json"
	"strings"
)

// defaultPiURL is the agent endpoint the pi extension uses when MESSH_URL is unset.
const defaultPiURL = "http://127.0.0.1:7520/mcp"

// piInstructions explains how to connect pi (which has no MCP client of its
// own) through the messh pi extension in integrations/pi. tokenCmd is the
// command that prints the agent's bearer header ("messh agent token NAME
// --bearer [--state DIR]"); its executable and --state value become the
// extension's MESSH_BIN and MESSH_STATE when they are not the defaults.
func piInstructions(agent, tokenCmd, mcpURL string) string {
	bin, stateDir := parseTokenCmd(tokenCmd)
	if bin == "messh" {
		bin = ""
	}
	if mcpURL == defaultPiURL {
		mcpURL = ""
	}

	var b strings.Builder
	b.WriteString("For pi: pi has no MCP client, so messh ships a pi extension, the folder integrations/pi\n")
	b.WriteString("of the messh source tree (copy it to this device first if needed, e.g. scp -r integrations/pi raspi:messh-pi).\n")
	b.WriteString("  1. Install it once:  pi install ./integrations/pi   (or the folder's path, e.g. pi install ~/messh-pi)\n")
	if agent == "pi" && bin == "" && stateDir == "" && mcpURL == "" {
		b.WriteString("  2. Nothing to configure: the extension connects as agent \"pi\" to " + defaultPiURL + ".\n")
	} else {
		cfg, _ := json.Marshal(struct {
			Agent string `json:"agent"`
			URL   string `json:"url,omitempty"`
			Messh string `json:"messh,omitempty"`
			State string `json:"state,omitempty"`
		}{agent, mcpURL, bin, stateDir})
		env := []string{"MESSH_AGENT=" + agent}
		if mcpURL != "" {
			env = append(env, "MESSH_URL="+mcpURL)
		}
		if bin != "" {
			env = append(env, "MESSH_BIN="+bin)
		}
		if stateDir != "" {
			env = append(env, "MESSH_STATE="+stateDir)
		}
		b.WriteString("  2. Point it at this agent: create ~/.pi/agent/messh.json containing\n")
		b.WriteString("       " + string(cfg) + "\n")
		b.WriteString("     or start pi with the environment variables " + strings.Join(env, "  ") + "\n")
	}
	b.WriteString("  3. Check: start pi and type /messh (it lists the messh tools), or ask pi to call mesh_nodes.\n")
	b.WriteString("Details and troubleshooting: integrations/pi/README.md.\n")
	return b.String()
}

// parseTokenCmd returns the executable and the --state value of a token
// command, splitting it the way a POSIX shell would for the quoting
// tokenCommand emits (single quotes, '\” escapes) and older double quotes.
// Go's flag package also accepts -state, so both forms count.
func parseTokenCmd(cmd string) (bin, stateDir string) {
	var words []string
	var cur strings.Builder
	inWord, escaped := false, false
	var quote rune
	for _, r := range cmd {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case quote != 0 && r == quote:
			quote = 0
		case quote != 0:
			cur.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == '\\':
			escaped = true
			inWord = true
		case r == ' ' || r == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	if len(words) > 0 {
		bin = words[0]
	}
	for i, w := range words {
		name, value, hasValue := strings.Cut(strings.TrimLeft(w, "-"), "=")
		if !strings.HasPrefix(w, "-") || name != "state" {
			continue
		}
		if hasValue {
			stateDir = value
		} else if i+1 < len(words) {
			stateDir = words[i+1]
		}
	}
	return bin, stateDir
}
