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
	runningBinaryChecks(ctx, report)
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

var runningKinds = []struct {
	id      string
	pattern string
	fix     string
}{
	{"engine_process", "ibus-engine", "voiceime config apply"},
	// The service holds the models, so a stale one serves old inference while every
	// other check passes: server_unit verifies the unit's path, asr_server the answers.
	{"server_process", "server run", "voiceime config apply"},
}

func runningBinaryChecks(ctx *Context, report func(Check)) {
	ourselves, err := os.Executable()
	if err != nil {
		return
	}
	for _, kind := range runningKinds {
		processes := processesRunning(kind.pattern)
		if len(processes) == 0 {
			continue
		}
		status, detail, ok := runningBinaryVerdict(processes[0], ourselves)
		if !ok {
			continue
		}
		check := Check{ID: kind.id, Status: status, Detail: detail}
		// The fix is only offered when something is wrong: printing a restart
		// command under an ok finding invites an agent to restart a healthy
		// process, which would drop the user's current dictation session.
		if status != "ok" {
			check.Fix = kind.fix
		}
		report(check)
	}
}

// runningBinaryVerdict classifies the binary a running process came from.
func runningBinaryVerdict(running process, ourselves string) (status, detail string, ok bool) {
	binary := running.Binary
	if binary == ourselves {
		return "ok", fmt.Sprintf("pid %d, %s", running.PID, ourselves), true
	}
	if !strings.HasSuffix(binary, " (deleted)") {
		return "warn", fmt.Sprintf("pid %d is running %s, not this binary (%s)", running.PID, binary, ourselves), true
	}
	original := strings.TrimSuffix(binary, " (deleted)")
	if !fileExists(original) {
		return "fail", fmt.Sprintf(
			"pid %d is running %s, which no longer exists: the checkout was moved or renamed, and the process will fail as soon as it is restarted",
			running.PID, original), true
	}
	return "warn", fmt.Sprintf(
		"pid %d is still running the previous build of %s; restart it for rebuilt code to take effect",
		running.PID, original), true
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
	// The installed copy is what `make install` updates, so a unit pointing anywhere else
	// is a service that does not follow the installation.
	if self, err := os.Executable(); err == nil && filepath.Clean(self) != filepath.Clean(exec) {
		report(Check{
			ID:     "server_unit",
			Status: "warn",
			Detail: fmt.Sprintf("the unit runs %s, not this installed binary (%s): `make install` does not update the running service", exec, self),
			Fix:    "voiceime server install",
		})
		return
	}
	report(Check{ID: "server_unit", Status: "ok", Detail: exec})
}
