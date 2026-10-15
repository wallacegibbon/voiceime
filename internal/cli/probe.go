package cli

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// probeTimeout bounds every external program voiceime runs for informational
// purposes. A probe that hangs must not hang a doctor or a status call, because
// an agent waiting on it would have no way to tell the difference between a slow
// machine and a dead one.
const probeTimeout = 10 * time.Second

// probe runs a command and returns its combined output. The error is returned
// together with whatever output was produced, since several of the tools used
// here report problems on stderr while still exiting zero.
func probe(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

// firstLine is used for version banners, which are multi-line but whose first
// line is the only part worth putting in a report.
func firstLine(text string) string {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		return strings.TrimSpace(text[:index])
	}
	return strings.TrimSpace(text)
}

// containsLine reports whether any line of text matches trimmed exactly.
func containsLine(text, want string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

// listEngineContains reports whether `ibus list-engine` lists an engine name.
//
// The listing is a tree with a language header per group, and each engine is one
// indented line of the form "<name> - <description>":
//
//	language: English
//	  xkb:us::eng - English (US)
//	  voiceime - Voiceime (speech)
//
// so an exact whole-line match never succeeds, and a substring search would take
// "voiceime-probe" for "voiceime" (the probe component docs/ibus-protocol.md
// describes). The name is therefore the part before the " - " separator, which
// also skips the "language: ..." headers: they carry no engine.
func listEngineContains(text, name string) bool {
	if name == "" {
		return false
	}
	for _, line := range strings.Split(text, "\n") {
		listed, _, found := strings.Cut(strings.TrimSpace(line), " - ")
		if found && strings.TrimSpace(listed) == name {
			return true
		}
	}
	return false
}
