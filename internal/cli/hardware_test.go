package cli

import (
	"os"
	"path/filepath"
	"testing"
)

const cpuinfoSample = `processor	: 0
vendor_id	: AuthenticAMD
cpu family	: 25
model		: 80
physical id	: 0
core id		: 0

processor	: 1
physical id	: 0
core id		: 1

processor	: 2
physical id	: 0
core id		: 0

processor	: 3
physical id	: 0
core id		: 1

processor	: 4
physical id	: 0
core id		: 0

processor	: 5
physical id	: 0
core id		: 1

processor	: 6
physical id	: 0
core id		: 0

processor	: 7
physical id	: 0
core id		: 1
`

func TestParsePhysicalCores(t *testing.T) {
	// This sample is the reference machine's shape: 8 logical CPUs that are two
	// cores with SMT, which is why counting "processor" lines would be wrong.
	if got := parsePhysicalCores(cpuinfoSample); got != 2 {
		t.Errorf("parsePhysicalCores = %d, want 2", got)
	}
	if got := parsePhysicalCores("processor : 0\n"); got != 0 {
		t.Errorf("parsePhysicalCores with no core id = %d, want 0 (unknown, not one)", got)
	}
	if got := parsePhysicalCores(""); got != 0 {
		t.Errorf("parsePhysicalCores of nothing = %d, want 0", got)
	}
}

// The worker count is sized by physical cores, and a machine that does not report them still
// gets an answer: one worker per logical CPU is the shape of a machine without SMT.
func TestPhysicalCoresAnswers(t *testing.T) {
	if got := physicalCores(); got < 1 {
		t.Errorf("physicalCores = %d on this machine, want at least one", got)
	}
}

func TestParseBackends(t *testing.T) {
	cases := map[string]int{
		"backends: cpu":                 1,
		"backends: cpu,cuda":            2,
		"backends: cpu, vulkan , metal": 3,
		"backends:":                     0,
		"no backends line here":         0,
	}
	for line, want := range cases {
		if got := parseBackends(line); len(got) != want {
			t.Errorf("parseBackends(%q) = %v, want %d entries", line, got, want)
		}
	}
	if got := parseBackends("backends: cpu,cuda"); got[1] != "cuda" {
		t.Errorf("parseBackends split wrong: %v", got)
	}
}

// A build's banner is several lines with the backends last, which is why the reader looks for
// the line rather than reading one line of output.
func TestSupportedBackendsReadsTheBanner(t *testing.T) {
	stub := filepath.Join(t.TempDir(), "audiocpp_server")
	script := "#!/bin/sh\nprintf 'audio.cpp dev\\ngit: 000000 2026-01-01\\nbuild: x\\nbackends: cpu, vulkan\\n'\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	got := supportedBackends(stub)
	if len(got) != 2 || got[0] != "cpu" || got[1] != "vulkan" {
		t.Errorf("supportedBackends = %v, want [cpu vulkan]", got)
	}
	if got := supportedBackends(""); got != nil {
		t.Errorf("supportedBackends(\"\") = %v, want nothing", got)
	}
}
