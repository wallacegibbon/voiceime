package cli

import "testing"

// The machine-specific advice is only as good as its parsing, and the parsing
// reads formats that cannot be exercised from the test machine: /proc/cpuinfo
// from an unknown CPU, a version banner from an unknown build.

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

func TestThreadsVerdict(t *testing.T) {
	eightCores := hardwareFacts{PhysicalCores: 8, LogicalCPUs: 16}
	cases := []struct {
		name     string
		threads  int
		facts    hardwareFacts
		want     string
		wantFix  bool
		contains string
	}{
		{"within the core count", 4, eightCores, "ok", false, "8 physical cores"},
		{"exactly the core count", 8, eightCores, "ok", false, "8 physical cores"},
		{"oversubscribed", 16, eightCores, "warn", true, "contend"},
		{"cores unknown", 4, hardwareFacts{LogicalCPUs: 4}, "ok", false, "not reported"},
		{"logical fallback oversubscribed", 8, hardwareFacts{LogicalCPUs: 4}, "warn", true, "contend"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			status, detail, fix := threadsVerdict(test.threads, test.facts)
			if status != test.want {
				t.Fatalf("status = %q, want %q (%s)", status, test.want, detail)
			}
			if (fix != "") != test.wantFix {
				t.Errorf("fix = %q, wantFix = %v", fix, test.wantFix)
			}
			if !containsSubstring(detail, test.contains) {
				t.Errorf("detail = %q, want it to mention %q", detail, test.contains)
			}
		})
	}
	// An unknown core count must not produce a finding: advising against a guess is
	// worse than saying nothing.
	if status, _, _ := threadsVerdict(64, hardwareFacts{}); status != "" {
		t.Errorf("status = %q with no core count, want no finding", status)
	}
}

func TestBackendVerdict(t *testing.T) {
	noGPU := hardwareFacts{}
	withDriver := hardwareFacts{GPUDrivers: []string{"nvidia-smi"}}
	withRenderNode := hardwareFacts{RenderNodes: []string{"/dev/dri/renderD128"}}

	cases := []struct {
		name       string
		configured string
		supported  []string
		facts      hardwareFacts
		want       string
		contains   string
	}{
		{"cpu on a cpu-only build", "cpu", []string{"cpu"}, noGPU, "ok", "supports: cpu"},
		{"unsupported backend", "vulkan", []string{"cpu"}, noGPU, "fail", "was built with"},
		{"unknown support list", "vulkan", nil, noGPU, "ok", "vulkan"},
		{"gpu the binary was built for", "cuda", []string{"cpu", "cuda"}, withDriver, "ok", "supports"},
		{"gpu present but unused", "cpu", []string{"cpu"}, withDriver, "warn", "nvidia-smi"},
		// A render node exists on every laptop with integrated graphics; warning
		// about them would make the check noise, so it is context, not a finding.
		{"render node only", "cpu", []string{"cpu"}, withRenderNode, "ok", "render node"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			status, detail, _ := backendVerdict(test.configured, test.supported, test.facts)
			if status != test.want {
				t.Fatalf("status = %q, want %q (%s)", status, test.want, detail)
			}
			if !containsSubstring(detail, test.contains) {
				t.Errorf("detail = %q, want it to mention %q", detail, test.contains)
			}
		})
	}
}

func containsSubstring(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return needle == ""
}
