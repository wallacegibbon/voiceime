package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wallacegibbon/voiceime/internal/gnome"
	"github.com/wallacegibbon/voiceime/internal/ibus"
)

func installChecks(report func(Check)) {
	componentExecCheck(report)
	componentEnvironmentCheck(report)
	sessionEngineCheck(report)
	runningBuildCheck(report)
}

func componentExecCheck(report func(Check)) {
	path := ibus.ComponentPath()
	if !fileExists(path) {
		report(Check{
			ID:     "component_exec",
			Status: "warn",
			Detail: "no component file at " + path + "; dictation has no way into applications yet",
			Fix:    "voiceime apply",
		})
		return
	}
	exec, err := ibus.ComponentExec(path)
	if err != nil {
		report(Check{
			ID:     "component_exec",
			Status: "fail",
			Detail: err.Error(),
			Fix:    "voiceime apply",
		})
		return
	}
	if !isExecutable(exec) {
		report(Check{
			ID:     "component_exec",
			Status: "fail",
			Detail: fmt.Sprintf("the component tells IBus to run %s, which does not exist (the checkout was moved or renamed)", exec),
			Fix:    "voiceime apply",
		})
		return
	}
	report(Check{ID: "component_exec", Status: "ok", Detail: exec})
}

// componentEnvironmentCheck reports whether the engine survives a login. A user-level
// component is found only because IBUS_COMPONENT_PATH names its directory, and that
// variable belongs to the session: `voiceime apply` writes it to
// environment.d, and an install that skipped that works until the machine is rebooted.
func componentEnvironmentCheck(report func(Check)) {
	registered, _ := engineRegistered()
	status, detail, ok := componentEnvironmentVerdict(
		ibus.SystemInstalled(), ibus.Installed(), ibus.EnvironmentSet(), registered)
	if !ok {
		return
	}
	check := Check{ID: "component_env", Status: status, Detail: detail}
	if status != "ok" {
		check.Fix = "voiceime apply"
	}
	report(check)
}

// sessionEngineCheck answers "I chose the voice input method and nothing happens".
//
// The engine process is spawned by the daemon, and an engine process that goes away takes
// the session's input method with it: when its bus name disappears the daemon clears the
// global engine, and every application then keeps showing the input method in the desktop's
// switcher while receiving no keys at all (measured: `ibus engine` answers "No engine is
// set." afterwards). Nothing puts it back but selecting an input method again, which is the
// gesture a person discovers for themselves and mistakes for a broken engine.
func sessionEngineCheck(report func(Check)) {
	if !ibus.Installed() {
		// component_exec already reports an install that has no way into applications.
		return
	}
	name, err := currentEngine()
	switch {
	case err != nil:
		report(Check{ID: "session_engine", Status: "warn", Detail: err.Error(), Fix: "ibus restart"})
		return
	case name == ibus.EngineName:
		report(Check{ID: "session_engine", Status: "ok", Detail: "keys reach " + name})
		return
	case name != "":
		// A different input method is selected. That is the person's choice, not a finding.
		return
	}
	listed, listErr := gnome.HasInputSource(engineInputSource(ibus.EngineName))
	if listErr != nil || !listed {
		// The desktop is not asking for voiceime, so nothing is missing.
		return
	}
	report(Check{
		ID:     "session_engine",
		Status: "warn",
		Detail: "no engine is selected for this session, though the desktop lists voiceime: applications show the input method and receive no keys",
		Fix:    "ibus engine " + ibus.EngineName,
	})
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

// componentBinaryPath is the binary the component file tells ibus-daemon to start. It is the
// record of what this machine was configured to run, which is what a running process is compared
// against.
func componentBinaryPath() (string, bool) {
	exec, err := ibus.ComponentExec(ibus.ComponentPath())
	return exec, err == nil && exec != ""
}

// runningBuildCheck answers one question about the one long-lived process this tool owns: is the
// engine running the binary this machine is configured to run?
//
// The reference is the exec recorded in the component file, never os.Executable(). Doctor is
// usually run from a build directory, which makes it a third copy of the program: measured
// against itself it reported warnings pointing at two different commands, and no action made
// them go away.
func runningBuildCheck(report func(Check)) {
	processes := processesRunning("ibus-engine", "voiceime")
	if len(processes) == 0 {
		return
	}
	want, recorded := componentBinaryPath()
	if !recorded {
		// component_exec already reports a missing component file; a second finding about the
		// same absence would only lengthen the report.
		return
	}
	status, detail := runningBinaryVerdict(processes[0], want, "the engine")
	if status == "ok" {
		report(Check{ID: "running_build", Status: "ok", Detail: detail})
		return
	}
	// The fix is only offered when something is wrong: printing a restart command under an
	// ok finding invites an agent to restart a healthy process, which would drop the user's
	// current dictation session.
	report(Check{ID: "running_build", Status: status, Detail: detail, Fix: "voiceime apply"})
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

// process is one long-lived process this tool owns.
type process struct {
	PID int
	// Binary is the resolved path from /proc/<pid>/exe, with " (deleted)" when
	// the file it was started from is gone - which is how a renamed checkout
	// shows up.
	Binary string
}

func processesRunning(pattern, binaryName string) []process {
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
		if !strings.Contains(filepath.Base(binary), binaryName) {
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
