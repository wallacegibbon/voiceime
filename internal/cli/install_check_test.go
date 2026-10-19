package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The three install checks exist because a moved checkout breaks voiceime
// silently. These tests pin the parsing and the classification, which are the
// parts that can be wrong without anything crashing.

func TestUnitExecStartReadsTheCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "voiceime-hotkey.service")
	unit := `[Unit]
Description=voiceime push-to-talk listener

[Service]
Type=simple
ExecStart=/home/u/.local/bin/voiceime hotkey run --keys <F9>
Restart=on-failure

[Install]
WantedBy=default.target
`
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := unitExecStart(path)
	if err != nil {
		t.Fatalf("unitExecStart: %v", err)
	}
	if want := "/home/u/.local/bin/voiceime"; got != want {
		t.Errorf("unitExecStart = %q, want %q", got, want)
	}
}

func TestUnitExecStartRejectsAUnitWithoutOne(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.service")
	if err := os.WriteFile(path, []byte("[Unit]\nDescription=nothing\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := unitExecStart(path); err == nil {
		t.Error("a unit with no ExecStart was accepted; systemd would report 203/EXEC with no explanation")
	}
}

func TestRunningBinaryVerdict(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "voiceime")
	if err := os.WriteFile(existing, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(dir, "gone", "voiceime")
	ourselves := filepath.Join(dir, "other-voiceime")

	cases := []struct {
		name     string
		running  string
		want     string
		contains string
	}{
		{"same binary", ourselves, "ok", "pid"},
		{"a different install", existing, "warn", "not this binary"},
		{"rebuilt in place", existing + " (deleted)", "warn", "previous build"},
		{"checkout moved", moved + " (deleted)", "fail", "moved or renamed"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			status, detail, ok := runningBinaryVerdict(process{PID: 42, Binary: test.running}, ourselves)
			if !ok {
				t.Fatal("verdict was skipped")
			}
			if status != test.want {
				t.Errorf("status = %q, want %q (detail: %s)", status, test.want, detail)
			}
			if !contains(detail, test.contains) {
				t.Errorf("detail = %q, want it to mention %q", detail, test.contains)
			}
		})
	}
}

// processesRunning reads /proc, so this checks it does not match a process that
// merely has voiceime in the command line it was launched with.
func TestProcessesRunningRequiresTheBinaryToBeOurs(t *testing.T) {
	for _, running := range processesRunning("--nonexistent-pattern--") {
		t.Errorf("matched an unrelated process: %+v", running)
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

// /proc/<pid>/cmdline is NUL-separated, which is easy to get wrong: matching with
// spaces made the listener check find nothing while the engine check - a single
// argument - worked.
func TestCommandLineNormalizesNulSeparatedArguments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cmdline")
	raw := []byte("/home/u/.local/bin/voiceime\x00hotkey\x00run\x00--keys\x00<F9>\x00")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := commandLine(path)
	if !ok {
		t.Fatal("commandLine failed on a readable file")
	}
	if !contains(got, "hotkey run") {
		t.Errorf("commandLine = %q, want it to contain %q", got, "hotkey run")
	}
	if !strings.HasPrefix(got, "/home/u/.local/bin/voiceime ") {
		t.Errorf("commandLine = %q, want it to start with the binary path", got)
	}
	if strings.Contains(got, "\\x00") {
		t.Errorf("commandLine = %q, want the NUL separators replaced", got)
	}
}

// A process that merely mentions the pattern is not an engine.
//
// `config apply` used to stop the engine with `pkill -f "voiceime ibus-engine"`, which
// matches any process whose *command line* contains that string - measured: it killed
// the shell that ran the command, twice, and would as easily kill an editor with the docs
// open. Matching is on the executable now, which only this binary can be.
func TestProcessesRunningIgnoresProcessesThatMerelyMentionThePattern(t *testing.T) {
	// A shell whose command line contains the pattern: exactly what pkill -f matched.
	command := exec.Command("/bin/sh", "-c", "sleep 5 # voiceime ibus-engine")
	if err := command.Start(); err != nil {
		t.Fatalf("start the decoy: %v", err)
	}
	defer func() {
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
	}()

	for _, found := range processesRunning("ibus-engine") {
		if found.PID == command.Process.Pid {
			t.Fatalf("pid %d mentions the pattern but was taken for an engine (exe %s)", found.PID, found.Binary)
		}
	}
}
