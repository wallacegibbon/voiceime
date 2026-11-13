package cli

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

const probeTimeout = 10 * time.Second

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

// listEngineContains reports whether `ibus list-engine` lists an engine name.
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
