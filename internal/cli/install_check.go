package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wallacegibbon/voiceime/internal/ibus"
)

func installChecks(ctx *Context, report func(Check)) {
	componentExecCheck(ctx, report)
	componentEnvironmentCheck(ctx, report)
	runningBuildCheck(ctx, report)
}

func componentExecCheck(ctx *Context, report func(Check)) {
	path := ibus.ComponentPath()
	if !fileExists(path) {
		report(Check{
			ID:     "component_exec",
			Status: "warn",
			Detail: "no component file at " + path + "; dictation has no way into applications yet",
			Fix:    "voiceime engine install --apply",
		})
		return
	}
	exec, err := ibus.ComponentExec(path)
	if err != nil {
		report(Check{
			ID:     "component_exec",
			Status: "fail",
			Detail: err.Error(),
			Fix:    "voiceime engine install --apply",
		})
		return
	}
	if !isExecutable(exec) {
		report(Check{
			ID:     "component_exec",
			Status: "fail",
			Detail: fmt.Sprintf("the component tells IBus to run %s, which does not exist (the checkout was moved or renamed)", exec),
			Fix:    "voiceime engine install --apply",
		})
		return
	}
	report(Check{ID: "component_exec", Status: "ok", Detail: exec})
}

// componentEnvironmentCheck reports whether the engine survives a login. A user-level
// component is found only because IBUS_COMPONENT_PATH names its directory, and that
// variable belongs to the session: `engine install --apply` also writes it to
// environment.d, and an install that skipped that works until the machine is rebooted.
func componentEnvironmentCheck(ctx *Context, report func(Check)) {
	registered, _ := engineRegistered(ctx)
	status, detail, ok := componentEnvironmentVerdict(
		ibus.SystemInstalled(), ibus.Installed(), ibus.EnvironmentSet(), registered)
	if !ok {
		return
	}
	check := Check{ID: "component_env", Status: status, Detail: detail}
	if status != "ok" {
		check.Fix = "voiceime engine install --apply"
	}
	report(check)
}

// componentEnvironmentVerdict is the decision without the machine, so every case can be
// tested: a system-wide component needs no variable, a recorded path is the working
// user-level install, and a path that exists only in this session is either a warning
// (the engine is registered now) or a failure (it is not, which is what a reboot looks
// like).
func componentEnvironmentVerdict(systemInstalled, componentInstalled, environmentSet, registered bool) (status, detail string, ok bool) {
	switch {
	case systemInstalled:
		return "ok", ibus.SystemComponentPath() + " (system-wide, no variable needed)", true
	case !componentInstalled:
		// component_exec already reports the missing component; a second finding with
		// the same fix would only lengthen the report.
		return "", "", false
	case environmentSet:
		return "ok", ibus.EnvironmentFile(), true
	case registered:
		return "warn", fmt.Sprintf(
			"%s does not set %s; this session has it, so the engine will disappear at the next login",
			ibus.EnvironmentFile(), ibus.ComponentPathVariable), true
	default:
		return "fail", fmt.Sprintf(
			"%s does not set %s, so a login starts ibus-daemon without %s and the engine is not registered at all",
			ibus.EnvironmentFile(), ibus.ComponentPathVariable, ibus.ComponentDir()), true
	}
}

// unitExecStart extracts the executable a systemd unit starts.
func unitExecStart(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		value, found := strings.CutPrefix(strings.TrimSpace(line), "ExecStart=")
		if !found {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			return "", fmt.Errorf("%s: ExecStart is empty", path)
		}
		return fields[0], nil
	}
	return "", fmt.Errorf("%s: no ExecStart line", path)
}

// residentKind is one of the two long-lived voiceime processes: how to find it, and where
// the binary it is supposed to run is recorded.
type residentKind struct {
	name    string
	pattern string
	// configured is that record: the exec of the component file for the engine, the
	// ExecStart of the unit for the service.
	configured func() (string, bool)
}

var residentKinds = []residentKind{
	{"the engine", "ibus-engine", componentBinaryPath},
	// The service holds the models, so a stale one serves old inference while every
	// other check passes.
	{"the recognition service", "server run", servedBinaryPath},
}

func componentBinaryPath() (string, bool) {
	exec, err := ibus.ComponentExec(ibus.ComponentPath())
	return exec, err == nil && exec != ""
}

func servedBinaryPath() (string, bool) {
	path := serverUnitPath()
	if path == "" {
		return "", false
	}
	exec, err := unitExecStart(path)
	return exec, err == nil && exec != ""
}

// runningBuildCheck answers one question - do the resident processes run the binaries this
// machine is configured to run - as one finding with one fix.
//
// The reference is those recorded paths, never os.Executable(). Doctor is usually run from
// a build directory, which makes it a third copy of the program: measured against itself it
// reported the engine, the service and the unit as three warnings pointing at two different
// commands, and no action made them go away.
func runningBuildCheck(ctx *Context, report func(Check)) {
	var healthy, stale []string
	worst := "ok"
	for _, kind := range residentKinds {
		processes := processesRunning(kind.pattern)
		if len(processes) == 0 {
			continue
		}
		want, recorded := kind.configured()
		if !recorded {
			// component_exec and server_unit already report a missing component or unit;
			// a second finding about the same absence would only lengthen the report.
			continue
		}
		status, detail := runningBinaryVerdict(processes[0], want, kind.name)
		switch status {
		case "ok":
			healthy = append(healthy, detail)
		case "fail":
			worst = "fail"
			stale = append(stale, detail)
		default:
			if worst != "fail" {
				worst = "warn"
			}
			stale = append(stale, detail)
		}
	}
	if len(healthy)+len(stale) == 0 {
		return
	}
	if len(stale) == 0 {
		report(Check{ID: "running_build", Status: "ok", Detail: strings.Join(healthy, "; ")})
		return
	}
	// The fix is only offered when something is wrong: printing a restart command under an
	// ok finding invites an agent to restart a healthy process, which would drop the user's
	// current dictation session.
	report(Check{ID: "running_build", Status: worst, Detail: strings.Join(stale, "; "), Fix: "voiceime config apply"})
}

// runningBinaryVerdict classifies the binary a running process came from against the path
// this machine is configured to run for it.
func runningBinaryVerdict(running process, want, name string) (status, detail string) {
	binary := running.Binary
	if binary == want {
		return "ok", fmt.Sprintf("pid %d, %s", running.PID, want)
	}
	original := strings.TrimSuffix(binary, " (deleted)")
	if !fileExists(original) {
		return "fail", fmt.Sprintf(
			"pid %d is running %s, which no longer exists: the checkout was moved or renamed, and the process will fail as soon as it is restarted",
			running.PID, original)
	}
	if original == want {
		return "warn", fmt.Sprintf(
			"pid %d is still running the previous build of %s; restart it for rebuilt code to take effect",
			running.PID, original)
	}
	return "warn", fmt.Sprintf(
		"pid %d is running %s, but %s is configured to run %s", running.PID, original, name, want)
}

// process is one running voiceime process.
type process struct {
	PID int
	// Binary is the resolved path from /proc/<pid>/exe, with " (deleted)" when
	// the file it was started from is gone - which is how a renamed checkout
	// shows up.
	Binary string
}

func processesRunning(pattern string) []process {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	var found []process
	for _, entry := range entries {
		if !entry.IsDir() || !isDigits(entry.Name()) {
			continue
		}
		base := filepath.Join("/proc", entry.Name())
		command, ok := commandLine(filepath.Join(base, "cmdline"))
		if !ok || !strings.Contains(command, pattern) {
			continue
		}
		binary, err := os.Readlink(filepath.Join(base, "exe"))
		if err != nil {
			continue
		}
		// A renamed binary is still named voiceime; a different name here means
		// the pattern matched something unrelated (a shell running this command,
		// for instance), so it is skipped.
		if !strings.Contains(binary, "voiceime") {
			continue
		}
		found = append(found, process{PID: atoi(entry.Name()), Binary: binary})
	}
	return found
}

// commandLine reads a process's arguments as one space-separated string.
func commandLine(path string) (string, bool) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(strings.ReplaceAll(string(raw), "\x00", " ")), true
}

func isDigits(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return value != ""
}

func atoi(value string) int {
	number := 0
	for _, r := range value {
		number = number*10 + int(r-'0')
	}
	return number
}

// isExecutable reports whether path names a file that can be run.
func isExecutable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

func serverUnitCheck(ctx *Context, report func(Check)) {
	path := serverUnitPath()
	if path == "" || !fileExists(path) {
		report(Check{
			ID:     "server_unit",
			Status: "fail",
			Detail: "no recognition server unit; dictation decodes through one, so nothing works without it",
			Fix:    "voiceime server install",
		})
		return
	}
	exec, err := unitExecStart(path)
	if err != nil {
		report(Check{ID: "server_unit", Status: "fail", Detail: err.Error(), Fix: "voiceime server install"})
		return
	}
	if !isExecutable(exec) {
		report(Check{
			ID:     "server_unit",
			Status: "fail",
			Detail: fmt.Sprintf("%s runs %s, which does not exist; systemd will restart it forever with status=203/EXEC", path, exec),
			Fix:    "voiceime server install",
		})
		return
	}
	// The component file and the unit are two records of the same install. When they name
	// different binaries, `make install` updates one of them and the other keeps running the
	// previous build - which no other check sees.
	if want, recorded := componentBinaryPath(); recorded && filepath.Clean(want) != filepath.Clean(exec) {
		report(Check{
			ID:     "server_unit",
			Status: "warn",
			Detail: fmt.Sprintf("the unit runs %s while the component file runs %s: the engine and the service are different builds", exec, want),
			Fix:    "voiceime server install",
		})
		return
	}
	report(Check{ID: "server_unit", Status: "ok", Detail: exec})
}
