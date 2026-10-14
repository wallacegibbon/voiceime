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
