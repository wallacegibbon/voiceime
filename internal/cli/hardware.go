package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// This file answers the questions about the *machine* that the reference
// machine's defaults silently answer wrongly everywhere else: how many cores it
// has, and whether it has a GPU the local audio.cpp build could use.
//
// Nothing here changes a setting. Detection is reported, and the checks in
// hardware_check.go turn it into advice with a runnable fix, because both numbers
// are guesses about intent: more threads is not always faster, and a GPU that
// exists is not necessarily a GPU audio.cpp was built for.

// hardwareFacts is what can be learned without running a benchmark.
type hardwareFacts struct {
	// PhysicalCores is 0 when it cannot be determined; callers fall back to the
	// logical count rather than guessing.
	PhysicalCores int
	LogicalCPUs   int
	// GPUDrivers are the vendor compute tools found on PATH (nvidia-smi and
	// friends), which say a GPU is there to be used.
	GPUDrivers []string
	// RenderNodes are /dev/dri/renderD* nodes. They are much weaker evidence: an
	// integrated GPU that cannot help, or cannot be driven by the local audio.cpp
	// build, has one too, so they are reported but never treated as a finding.
	RenderNodes []string
}

func detectHardware() hardwareFacts {
	cores := 0
	if raw, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		cores = parsePhysicalCores(string(raw))
	}
	return hardwareFacts{
		PhysicalCores: cores,
		LogicalCPUs:   runtime.NumCPU(),
		GPUDrivers:    gpuDrivers(),
		RenderNodes:   renderNodes(),
	}
}

// Cores is the count to reason with: physical cores when they are known, logical
// CPUs otherwise.
func (h hardwareFacts) Cores() int {
	if h.PhysicalCores > 0 {
		return h.PhysicalCores
	}
	return h.LogicalCPUs
}

// parsePhysicalCores counts unique (physical id, core id) pairs in /proc/cpuinfo.
//
// A container, a VM or an architecture that does not report those fields yields
// 0, which the caller treats as "unknown" rather than as "one core".
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

// gpuDrivers lists vendor compute tools on PATH. It is deliberately not "any hint
// of a GPU": a /dev/dri render node exists on every laptop with integrated
// graphics, and warning about those would make this check noise. What the local
// audio.cpp build actually supports is asked of the binary, not inferred from
// here.
func gpuDrivers() []string {
	var drivers []string
	for _, tool := range []string{"nvidia-smi", "rocm-smi", "rocminfo"} {
		if _, err := exec.LookPath(tool); err == nil {
			drivers = append(drivers, tool)
		}
	}
	return drivers
}

// renderNodes lists the DRM render nodes, which are reported as context rather
// than as evidence.
func renderNodes() []string {
	nodes, _ := filepath.Glob("/dev/dri/renderD*")
	sort.Strings(nodes)
	return nodes
}

// parseBackends reads the `backends: cpu,cuda` line an audio.cpp CLI prints in its
// version banner. An absent or empty line yields nothing, which callers treat as
// "the build did not say" rather than as "no backends".
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

// supportedBackends asks the local binary which backends it was built with, which
// is the only authority on whether `backend = vulkan` can work here.
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

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
