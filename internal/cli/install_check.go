package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wallacegibbon/voiceime/internal/ibus"
)

// installChecks reports the failures that look like "voiceime is broken" but are
// really "an installed artifact points at something that is gone".
//
// All three have happened on the reference machine, and none of them was
// detectable before this check existed:
//
//   - the component file ibus-daemon reads named a binary from a checkout that
//     had been renamed, so a restart of the engine would have failed while the
//     already-running process kept working;
//   - the same rename left the systemd unit with an ExecStart that no longer
//     existed, which systemd reports as a restart loop with status=203/EXEC;
//   - after every rebuild, the running engine keeps running the binary it was
//     started from, so fixed code appears not to have taken effect.
//
// Each is offline and cheap: a stat, a readlink, and one small file read.
func installChecks(ctx *Context, report func(Check)) {
	componentExecCheck(ctx, report)
	hotkeyUnitCheck(ctx, report)
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

func hotkeyUnitCheck(ctx *Context, report func(Check)) {
	path := servicePath()
	if path == "" || !fileExists(path) {
		report(Check{
			ID:     "hotkey_unit",
			Status: "warn",
			Detail: "no shortcut listener installed; switching the input method by hand still works",
			Fix:    "voiceime hotkey install",
		})
		return
	}
	exec, err := unitExecStart(path)
	if err != nil {
		report(Check{ID: "hotkey_unit", Status: "fail", Detail: err.Error(), Fix: "voiceime hotkey install"})
		return
	}
	if !isExecutable(exec) {
		report(Check{
			ID:     "hotkey_unit",
			Status: "fail",
			Detail: fmt.Sprintf("%s runs %s, which does not exist; systemd will restart it forever with status=203/EXEC", path, exec),
			Fix:    "voiceime hotkey install",
		})
		return
	}
	report(Check{ID: "hotkey_unit", Status: "ok", Detail: exec})
}

// unitExecStart extracts the executable a systemd unit starts.
//
// It reads only the first ExecStart line's first field: enough to answer "does
// the binary exist", which is the failure that produces 203/EXEC. Parsing systemd
// syntax properly is not worth it for one field, and a wrong answer here is a
// false alarm rather than a missed one.
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

// runningBinaryChecks compares the binary the running processes came from with
// the one running this command.
//
// A process keeps the binary it was started from, so a rebuild does not take
// effect until it is restarted, and a deleted binary keeps working until then
// too. Both are invisible from the outside, which is what made them expensive.
func runningBinaryChecks(ctx *Context, report func(Check)) {
	ourselves, err := os.Executable()
	if err != nil {
		return
	}
	for _, kind := range []struct {
		id      string
		pattern string
		fix     string
	}{
		{"engine_process", "ibus-engine", "ibus engine xkb:us::eng && pkill -f 'voiceime ibus-engine' && ibus engine voiceime"},
		{"hotkey_process", "hotkey run", "systemctl --user restart voiceime-hotkey"},
	} {
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
//
// Three cases, and the difference between them matters:
//
//   - the same file as this command: nothing to say (ok);
//   - a file that no longer exists: the checkout was moved or renamed, and the
//     process will fail the moment it is restarted (fail);
//   - a file that exists but is not this one: either a rebuild replaced the
//     binary under a running process (/proc/<pid>/exe then reads "path
//     (deleted)") or the two are genuinely different installs. Both mean the
//     process is running other code than this command (warn).
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
			"pid %d is running %s, which no longer exists: the checkout was moved or renamed, and the engine will fail as soon as it is restarted",
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

// processesRunning finds this user's voiceime processes whose command line
// contains pattern.
//
// Reading /proc directly keeps this cgo-free, which the whole project depends on.
// A process that cannot be inspected (it exited, or belongs to another user) is
// skipped rather than reported: the answer is only interesting when it is a
// voiceime process.
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
//
// /proc/<pid>/cmdline separates arguments with NUL bytes, so a pattern that spans
// two of them ("hotkey run") matches nothing without this - which is how the
// listener check silently reported nothing at all while the engine check, whose
// pattern is a single argument, worked.
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
