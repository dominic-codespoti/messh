package main

import "strings"

// tokenCommand is the shell command that prints agent's token, as harnesses
// run it for "!command" secrets (omp, pi) and as shown to other clients.
// stateDir is added as --state only when non-empty (given explicitly).
func tokenCommand(agent string, bearer bool, stateDir string) string {
	cmd := "messh agent token " + agent
	if bearer {
		cmd += " --bearer"
	}
	return cmd + stateFlag(stateDir)
}

// stateFlag renders " --state DIR" for printed commands, or "" for no dir.
func stateFlag(stateDir string) string {
	if stateDir == "" {
		return ""
	}
	return " --state " + shellPath(stateDir)
}

// shellPath renders a path for a POSIX-style shell (omp runs "!" commands
// through one even on Windows): backslashes become forward slashes, which
// Windows accepts, and anything beyond plain path characters is single-quoted.
func shellPath(p string) string {
	p = strings.ReplaceAll(p, `\`, "/")
	safe := p != ""
	for _, r := range p {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_./:@%+=,-", r)) {
			safe = false
			break
		}
	}
	if safe {
		return p
	}
	return "'" + strings.ReplaceAll(p, "'", `'\''`) + "'"
}
