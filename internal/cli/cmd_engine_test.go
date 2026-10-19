package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// The engine is spawned by ibus-daemon, which puts its stdout and stderr on pipes it
// discards - measured: the process's fds are pipes, and the journal holds no line from
// it. Without a file, a dictation has no record of itself, which is why a user reporting
// "I pressed the key and nothing happened" could not be helped: the session that would
// have said what it heard was gone.
func TestEngineLogPathIsInTheCacheDir(t *testing.T) {
	if got, want := engineLogPath(), filepath.Join(config.CacheDir(), "engine.log"); got != want {
		t.Errorf("engineLogPath() = %q, want %q", got, want)
	}
}

// The log is written, timestamped, and readable by nobody else: the lines quote what the
// user said.
func TestAppendLogLineTimestampsAndRestricts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "engine.log")
	appendLogLine(path, "session: listening")
	appendLogLine(path, "session: committed 2 character(s): 你好")

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("log not written: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("mode = %o, want 600: the log holds dictated text", perm)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(lines) != 2 {
		t.Fatalf("wrote %d lines, want 2:\n%s", len(lines), body)
	}
	for _, line := range lines {
		// RFC3339 prefix, then the message: "2026-10-20T00:40:00+08:00 session: ..."
		if len(line) < 21 || line[4] != '-' || line[10] != 'T' || !strings.Contains(line, " session: ") {
			t.Errorf("line %q is not timestamped", line)
		}
	}
	if !strings.Contains(string(body), "你好") {
		t.Error("the payload was not written verbatim")
	}
}

// A log that grows without bound is a disk leak with a useful name, so a file past the
// bound is dropped rather than appended to forever - and one under it is kept.
func TestRotateLogOnlyDropsWhatIsTooBig(t *testing.T) {
	dir := t.TempDir()
	small := filepath.Join(dir, "small.log")
	if err := os.WriteFile(small, []byte("recent history\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rotateLog(small, maxEngineLogBytes)
	if body, _ := os.ReadFile(small); !strings.Contains(string(body), "recent history") {
		t.Error("rotation dropped a log that was under the bound")
	}

	big := filepath.Join(dir, "big.log")
	if err := os.WriteFile(big, make([]byte, maxEngineLogBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	rotateLog(big, maxEngineLogBytes)
	if info, err := os.Stat(big); err != nil || info.Size() != 0 {
		t.Errorf("an oversized log was not emptied: %v %v", info, err)
	}

	// Missing files are not an error: the first dictation creates the log.
	rotateLog(filepath.Join(dir, "absent.log"), maxEngineLogBytes)
}

// Stderr is written only when a human is looking at it.
//
// ibus-daemon spawns the engine with its stdout and stderr on pipes (measured), and a
// write to a pipe whose reader never drains it blocks once the buffer fills - so a
// long-lived install could freeze a dictation mid-sentence on a log line. A terminal (or
// /dev/null) is a character device; a pipe and a regular file are not.
func TestIsTerminalFileDistinguishesPipesAndFiles(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	if isTerminalFile(write) {
		t.Error("a pipe was taken for a terminal: a session could block on a full pipe")
	}

	regular, err := os.CreateTemp(t.TempDir(), "log")
	if err != nil {
		t.Fatal(err)
	}
	defer regular.Close()
	if isTerminalFile(regular) {
		t.Error("a regular file was taken for a terminal")
	}

	null, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Skipf("no %s on this machine: %v", os.DevNull, err)
	}
	defer null.Close()
	if !isTerminalFile(null) {
		t.Errorf("%s was not recognised as a character device", os.DevNull)
	}
}
