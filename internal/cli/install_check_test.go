package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallacegibbon/voiceime/internal/ibus"
)

// An install that is only in the session's activation environment is the one that stops
// working at the next login, so the four states have to be distinguishable.
func TestComponentEnvironmentVerdict(t *testing.T) {
	componentDir := ibus.ComponentDir()
	cases := []struct {
		name        string
		system      bool
		component   bool
		environment bool
		registered  bool
		wantStatus  string
		wantDetail  string
	}{
		{"system-wide", true, false, false, false, "ok", "system-wide"},
		{"nothing installed", false, false, false, false, "", ""},
		{"path recorded", false, true, true, false, "ok", ibus.EnvironmentFile()},
		{"session only, engine registered", false, true, false, true, "warn", "next login"},
		{"session only, engine gone", false, true, false, false, "fail", componentDir},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			status, detail, ok := componentEnvironmentVerdict(
				test.system, test.component, test.environment, test.registered)
			if test.wantStatus == "" {
				if ok {
					t.Fatalf("a check with nothing to say reported %q: %s", status, detail)
				}
				return
			}
			if !ok {
				t.Fatal("the verdict was skipped")
			}
			if status != test.wantStatus {
				t.Errorf("status = %q, want %q (detail: %s)", status, test.wantStatus, detail)
			}
			if !contains(detail, test.wantDetail) {
				t.Errorf("detail %q does not mention %q", detail, test.wantDetail)
			}
		})
	}
}

// The reference is the binary the machine is configured to run, not the binary running
// doctor: the function has no parameter for that, because a doctor run out of a build
// directory is a third copy of the program and measuring against it reported a healthy
// install as three warnings that nothing could clear.
func TestRunningBinaryVerdict(t *testing.T) {
	dir := t.TempDir()
	installed := filepath.Join(dir, "installed", "voiceime")
	build := filepath.Join(dir, "checkout", "bin", "voiceime")
	for _, path := range []string{installed, build} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	moved := filepath.Join(dir, "gone", "voiceime")

	cases := []struct {
		name     string
		running  string
		want     string
		status   string
		contains string
	}{
		{"the configured binary", installed, installed, "ok", "pid"},
		{"started by hand from a build directory", build, installed, "warn", "configured to run"},
		{"rebuilt in place", installed + " (deleted)", installed, "warn", "previous build"},
		{"the checkout moved", moved + " (deleted)", installed, "fail", "moved or renamed"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			status, detail := runningBinaryVerdict(process{PID: 42, Binary: test.running}, test.want, "the engine")
			if status != test.status {
				t.Errorf("status = %q, want %q (detail: %s)", status, test.status, detail)
			}
			if !contains(detail, test.contains) {
				t.Errorf("detail = %q, want it to mention %q", detail, test.contains)
			}
		})
	}
}

// One finding covers both resident processes, so a table of kinds is what keeps them in
// the report rather than a list of check IDs.
// processesRunning reads /proc, so this checks it does not match a process that merely has the
// pattern in the command line it was launched with - which, for the recognition service, is a
// path this tool wrote rather than one of its own executables.
func TestProcessesRunningRequiresTheBinaryToBeOurs(t *testing.T) {
	for _, running := range processesRunning("--nonexistent-pattern--", "voiceime") {
		t.Errorf("matched an unrelated process: %+v", running)
	}
	// The pattern here is the file the service is started with, which on a running machine
	// does match a live process: what has to keep a shell, an editor or a `grep` out of the
	// finding is the executable name.
	for _, running := range processesRunning("audiocpp_server", "no-such-binary-name") {
		t.Errorf("matched a process that is not the audio.cpp this tool started: %+v", running)
	}
	if atoi("12") != 12 || isDigits("1a") || isDigits("") {
		t.Error("pid parsing is wrong")
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return needle == ""
}

func TestCommandLineNormalizesNulSeparatedArguments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cmdline")
	raw := []byte("/home/u/audio.cpp/build/linux-cpu-release/bin/audiocpp_server\x00--config\x00/home/u/.local/share/audiocpp-asr/serve.json\x00--log\x00")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := commandLine(path)
	if !ok {
		t.Fatal("commandLine failed on a readable file")
	}
	if !contains(got, "--config /home/u/.local/share/audiocpp-asr/serve.json") {
		t.Errorf("commandLine = %q, want it to contain the arguments", got)
	}
	if !strings.HasPrefix(got, "/home/u/audio.cpp/build/linux-cpu-release/bin/audiocpp_server ") {
		t.Errorf("commandLine = %q, want it to start with the binary path", got)
	}
	if strings.Contains(got, "\\x00") {
		t.Errorf("commandLine = %q, want the NUL separators replaced", got)
	}
}

// A process that merely mentions the pattern is not an engine.
func TestProcessesRunningIgnoresProcessesThatMerelyMentionThePattern(t *testing.T) {
	// A shell whose command line contains the pattern: exactly what pkill -f matched.
	command := exec.Command("/bin/sh", "-c", "sleep 5 # voiceime ibus-engine audiocpp_server")
	if err := command.Start(); err != nil {
		t.Fatalf("start the decoy: %v", err)
	}
	defer func() {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
	}()

	for _, found := range processesRunning("ibus-engine", "voiceime") {
		if found.PID == command.Process.Pid {
			t.Fatalf("pid %d mentions the pattern but was taken for the engine (exe %s)",
				found.PID, found.Binary)
		}
	}
}
