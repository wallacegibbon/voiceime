package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wallacegibbon/voiceime/internal/config"
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

// unitExecStart extracts the command a systemd unit starts. The binary is the first word and
// the rest are its arguments; the unit that runs audio.cpp names the config file in them, and
// a check that only looked at the binary could not tell that the file it points at is gone.
func unitExecStart(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		value, found := strings.CutPrefix(strings.TrimSpace(line), "ExecStart=")
		if !found {
			continue
		}
		fields := splitUnitCommand(value)
		if len(fields) == 0 {
			return nil, fmt.Errorf("%s: ExecStart is empty", path)
		}
		return fields, nil
	}
	return nil, fmt.Errorf("%s: no ExecStart line", path)
}

// splitUnitCommand splits an ExecStart value the way systemd's word splitter does for the one
// form it has to read back: whitespace-separated words, with one layer of single or double
// quotes removed. serverUnit quotes a path when it contains a space, and a plain split would
// turn such a path into two words and report a file that does not exist.
func splitUnitCommand(value string) []string {
	var fields []string
	var current strings.Builder
	var quote rune
	started := false
	for _, r := range value {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			current.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
			started = true
		case r == ' ' || r == '\t':
			if started || current.Len() > 0 {
				fields = append(fields, current.String())
				current.Reset()
				started = false
			}
		default:
			current.WriteRune(r)
		}
	}
	if started || current.Len() > 0 {
		fields = append(fields, current.String())
	}
	return fields
}

// unitArgument returns the value after the first occurrence of flag in a command.
func unitArgument(fields []string, flag string) string {
	for index, field := range fields {
		if field == flag && index+1 < len(fields) {
			return fields[index+1]
		}
	}
	return ""
}

// residentKind is one of the two long-lived processes this tool owns: how to find it, which
// executable it has to be, and where the binary it is supposed to run is recorded.
type residentKind struct {
	name    string
	pattern string
	// binaryName is the second filter on the match: the pattern is looked for in a
	// process's whole command line, and a shell that merely mentions it is not the process.
	binaryName string
	// configured is that record: the exec of the component file for the engine, the
	// ExecStart of the unit for the service.
	configured func() (string, bool)
}

var residentKinds = []residentKind{
	{"the engine", "ibus-engine", "voiceime", componentBinaryPath},
	// The service holds the models, so a stale one serves old inference while every
	// other check passes. The generated config's name identifies it: audio.cpp is an
	// executable this tool did not build, and the file it is pointed at is the one
	// `voiceime apply` wrote.
	{"the recognition service", generatedConfigName, modelServiceBinary, servedBinaryPath},
}

func componentBinaryPath() (string, bool) {
	exec, err := ibus.ComponentExec(ibus.ComponentPath())
	return exec, err == nil && exec != ""
}

func servedBinaryPath() (string, bool) {
	path := unitPath()
	if path == "" {
		return "", false
	}
	fields, err := unitExecStart(path)
	return firstField(fields), err == nil && len(fields) > 0
}

func firstField(fields []string) string {
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

// runningBuildCheck answers one question - do the resident processes run the binaries this
// machine is configured to run - as one finding with one fix.
//
// The reference is those recorded paths, never os.Executable(). Doctor is usually run from
// a build directory, which makes it a third copy of the program: measured against itself it
// reported the engine, the service and the unit as three warnings pointing at two different
// commands, and no action made them go away.
func runningBuildCheck(report func(Check)) {
	var healthy, stale []string
	worst := "ok"
	for _, kind := range residentKinds {
		processes := processesRunning(kind.pattern, kind.binaryName)
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
	report(Check{ID: "running_build", Status: worst, Detail: strings.Join(stale, "; "), Fix: "voiceime apply"})
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

// unitCheck reads back the record scripts/asr-service.sh wrote: a unit names one audio.cpp
// and one config file, and both have to be there for the service to start. The component file
// is not compared with it - the unit runs audio.cpp and the component runs this engine, so
// they are meant to name different binaries.
//
// What it does not decide is whether a model on this machine is wanted: the settings' endpoint
// decides that, and asrEndpointCheck is the finding that asks it. So a missing unit is a
// failure only when the endpoint is this machine - and a unit that exists while the endpoint
// is elsewhere is a warning, because it is a model loaded into memory that nothing posts to.
func unitCheck(report func(Check), cfg *config.Config) {
	const id = "server_unit"
	fix := "scripts/asr-service.sh /path/to/audiocpp_server /path/to/model.gguf"
	path := unitPath()
	local := endpointIsLocal(strings.TrimSpace(cfg.ASRServerURL))
	if path == "" || !fileExists(path) {
		if !local {
			// Nothing to say: this setup has no model on this machine, by design.
			return
		}
		report(Check{
			ID:     id,
			Status: "fail",
			Detail: fmt.Sprintf("nothing here is installed to start a model, and %s is this machine", cfg.ASRServerURL),
			Fix:    fix,
		})
		return
	}
	fields, err := unitExecStart(path)
	if err != nil {
		report(Check{ID: id, Status: "fail", Detail: err.Error(), Fix: fix})
		return
	}
	binary := firstField(fields)
	if !isExecutable(binary) {
		report(Check{
			ID:     id,
			Status: "fail",
			Detail: fmt.Sprintf("%s runs %s, which does not exist; systemd will restart it forever with status=203/EXEC", path, binary),
			Fix:    fix,
		})
		return
	}
	// The audio.cpp config is not an input anyone edits: the script renders it and the unit
	// names it. A missing one means the service was started by an install that has since been
	// removed - `voiceime remove` deletes that file - and systemd's restart loop is silent.
	configFile := unitArgument(fields, "--config")
	if configFile == "" {
		report(Check{
			ID:     id,
			Status: "fail",
			Detail: fmt.Sprintf("%s runs %s without naming the audio.cpp config to serve", path, binary),
			Fix:    fix,
		})
		return
	}
	if !fileExists(configFile) {
		report(Check{
			ID:     id,
			Status: "fail",
			Detail: fmt.Sprintf("%s points %s at %s, which does not exist: audio.cpp would refuse to start", path, binary, configFile),
			Fix:    fix,
		})
		return
	}
	if !local {
		report(Check{
			ID:     id,
			Status: "warn",
			Detail: fmt.Sprintf("%s starts a model here, and the settings post to %s instead: nothing sends this machine's audio to it",
				binary, cfg.ASRServerURL),
			Fix: "systemctl --user disable --now " + unitName,
		})
		return
	}
	report(Check{ID: id, Status: "ok", Detail: fmt.Sprintf("%s serving from %s", binary, configFile)})
}
