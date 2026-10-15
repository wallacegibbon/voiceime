package cli

import (
	"fmt"
	"strings"
)

// hardwareChecks reports the two settings whose right value is a property of the
// machine rather than of voiceime: how many threads to use, and which backend to
// run on.
//
// Both defaults are the reference machine's (threads = 4 on an 8-core CPU,
// backend = cpu). Neither is wrong anywhere - they are simply not tuned anywhere
// else, and before this check nothing said so.
func hardwareChecks(ctx *Context, report func(Check)) {
	facts := detectHardware()

	if status, detail, fix := threadsVerdict(ctx.Config.Threads, facts); status != "" {
		report(Check{ID: "threads", Status: status, Detail: detail, Fix: fix})
	}

	supported := []string{}
	if binary, err := ctx.Config.FindASRBinary(); err == nil {
		supported = supportedBackends(binary)
	}
	if status, detail, fix := backendVerdict(ctx.Config.Backend, supported, facts); status != "" {
		report(Check{ID: "backend", Status: status, Detail: detail, Fix: fix})
	}
}

// threadsVerdict compares the configured thread count with the CPU.
//
// Only one direction is a finding: asking for more threads than there are
// physical cores cannot help, because the streaming decoder and the repair pass
// already compete for the same cores and both are compute-bound. Fewer threads is
// a legitimate choice - it leaves the machine responsive and the measured
// latency headroom is large (docs/measurements.md) - so it is reported as
// information, not as a problem.
func threadsVerdict(threads int, facts hardwareFacts) (status, detail, fix string) {
	cores := facts.Cores()
	if cores <= 0 || threads <= 0 {
		return "", "", ""
	}
	shape := fmt.Sprintf("%d physical cores (%d logical)", facts.PhysicalCores, facts.LogicalCPUs)
	if facts.PhysicalCores == 0 {
		shape = fmt.Sprintf("%d logical CPUs (physical cores not reported)", facts.LogicalCPUs)
	}
	if threads > cores {
		return "warn", fmt.Sprintf(
				"threads = %d on a machine with %s; the streaming decoder and the repair pass share the same cores, so the extra threads contend rather than help",
				threads, shape),
			fmt.Sprintf("voiceime config set threads %d", cores)
	}
	return "ok", fmt.Sprintf("threads = %d on %s", threads, shape), ""
}

// backendVerdict checks the configured backend against what the local binary was
// built with, and reports a GPU it is not being asked to use.
//
// Only a vendor compute tool on PATH is treated as a finding. A DRM render node
// exists on any machine with integrated graphics, and a check that warns on every
// laptop is a check that gets ignored.
func backendVerdict(configured string, supported []string, facts hardwareFacts) (status, detail, fix string) {
	if configured == "" {
		return "", "", ""
	}
	if len(supported) > 0 && !containsString(supported, configured) {
		return "fail", fmt.Sprintf("backend = %s, but the local audiocpp_cli was built with: %s",
				configured, strings.Join(supported, ", ")),
			"voiceime config set backend " + supported[0]
	}
	described := configured
	if len(supported) > 0 {
		described = fmt.Sprintf("%s (the local audiocpp_cli supports: %s)", configured, strings.Join(supported, ", "))
	}
	if configured == "cpu" && len(facts.GPUDrivers) > 0 {
		return "warn", fmt.Sprintf(
				"backend = %s, and this machine has %s. Using it needs an audio.cpp build with that backend; no GPU backend has been measured for this project",
				configured, strings.Join(facts.GPUDrivers, ", ")),
			"voiceime config set backend vulkan   # or cuda / hip / metal, after building audio.cpp with it"
	}
	if configured == "cpu" && len(facts.RenderNodes) > 0 {
		described += fmt.Sprintf("; a render node is present (%s), so a Vulkan build could use it", strings.Join(facts.RenderNodes, ", "))
	}
	return "ok", described, ""
}
