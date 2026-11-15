package cli

import (
	"os"
	"runtime"
	"strings"
)

// This is what `voiceime apply` asks the machine before it writes anything down: how many
// cores there are, and what the audio.cpp build supports. Neither is a setting - a machine
// has one core count, and a build was compiled for some backends and not others - so the
// answer belongs to the one command that installs, and nothing at run time looks it up again.
//
// There used to be a check here comparing `threads` and `backend` in the settings with the
// CPU. Both settings are gone, so there is nothing to disagree with; the values are in the
// audio.cpp config the service unit names, and `server_unit` reports that file.

// physicalCores is the count to size a worker pool by, or 0 when the machine does not say.
// Logical CPUs are the fallback: an unknown shape is not a reason to run one thread.
func physicalCores() int {
	if raw, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		if cores := parsePhysicalCores(string(raw)); cores > 0 {
			return cores
		}
	}
	return runtime.NumCPU()
}

// parsePhysicalCores counts unique (physical id, core id) pairs in /proc/cpuinfo.
func parsePhysicalCores(cpuinfo string) int {
	type pair struct{ physical, core string }
	seen := map[pair]bool{}
	var current struct{ physical, core string }
	flush := func() {
		if current.physical != "" && current.core != "" {
			seen[pair{current.physical, current.core}] = true
		}
		current.physical, current.core = "", ""
	}
	for _, line := range strings.Split(cpuinfo, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			flush()
			continue
		}
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		switch strings.TrimSpace(key) {
		case "physical id":
			current.physical = strings.TrimSpace(value)
		case "core id":
			current.core = strings.TrimSpace(value)
		}
	}
	flush()
	return len(seen)
}

// parseBackends reads the backend list out of an audio.cpp build's own banner.
func parseBackends(line string) []string {
	_, value, found := strings.Cut(line, "backends:")
	if !found {
		return nil
	}
	var backends []string
	for _, part := range strings.Split(value, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			backends = append(backends, trimmed)
		}
	}
	return backends
}

// supportedBackends asks the binary what it was compiled with, which is the only authority on
// whether a backend can work here: an audio.cpp built without cuda cannot do cuda whatever
// the machine has.
func supportedBackends(binary string) []string {
	if binary == "" {
		return nil
	}
	version, err := probe(binary, "--version")
	if err != nil && version == "" {
		return nil
	}
	return parseBackends(lastLineMatching(version, "backends:"))
}
