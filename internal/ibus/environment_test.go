package ibus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An install that is only in the session's activation environment is gone at the next
// login, which is why the search path is written down at all.
func TestWriteEnvironmentRecordsTheSearchPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if EnvironmentSet() {
		t.Fatal("a session with no file reported the search path as recorded")
	}

	path, err := WriteEnvironment()
	if err != nil {
		t.Fatalf("WriteEnvironment: %v", err)
	}
	if want := EnvironmentFile(); path != want {
		t.Errorf("WriteEnvironment wrote %q, want %q", path, want)
	}
	if !EnvironmentSet() {
		t.Error("EnvironmentSet is false right after a write")
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), ComponentPathVariable+"="+EnvironmentPath()) {
		t.Errorf("file does not name the search path:\n%s", body)
	}

	// Install is idempotent: running it again must not accumulate lines.
	if _, err := WriteEnvironment(); err != nil {
		t.Fatalf("second WriteEnvironment: %v", err)
	}
	again, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(again) != string(body) {
		t.Errorf("a second write changed the file:\nfirst:\n%s\nsecond:\n%s", body, again)
	}
}

// A file left by an older install names a directory that no longer holds the component,
// so it is not the same as one written now.
func TestEnvironmentSetRejectsAStalePath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "environment.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := ComponentPathVariable + "=/home/u/old-checkout/component:/usr/share/ibus/component\n"
	if err := os.WriteFile(EnvironmentFile(), []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	if EnvironmentSet() {
		t.Error("a file naming another directory was accepted as the current one")
	}
}

func TestRemoveEnvironmentReportsWhatItFound(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	removed, err := RemoveEnvironment()
	if err != nil || removed {
		t.Fatalf("RemoveEnvironment on nothing = %v, %v; want false, nil", removed, err)
	}
	if _, err := WriteEnvironment(); err != nil {
		t.Fatal(err)
	}
	removed, err = RemoveEnvironment()
	if err != nil || !removed {
		t.Fatalf("RemoveEnvironment = %v, %v; want true, nil", removed, err)
	}
	if EnvironmentSet() {
		t.Error("the search path is still recorded after removal")
	}
}

// environment.d is parsed like Environment=: a value the parser would split has to be
// quoted, and read back as it was written.
func TestEnvironmentPathRoundTripsThroughQuoting(t *testing.T) {
	for _, value := range []string{
		"/home/u/.local/share/voiceime/component:/usr/share/ibus/component",
		"/home/u/has space/component:/usr/share/ibus/component",
		`/home/u/quote"d/component:/usr/share/ibus/component`,
		`/home/u/back\slash/component:/usr/share/ibus/component`,
	} {
		line := ComponentPathVariable + "=" + environmentValue(value)
		got, found := environmentPath(line)
		if !found {
			t.Errorf("environmentPath found nothing in %q", line)
			continue
		}
		if got != value {
			t.Errorf("value %q round-tripped to %q (line %q)", value, got, line)
		}
	}
}

// Lines that are not the variable are skipped: environment.d is shared and may be
// edited by hand.
func TestEnvironmentPathIgnoresOtherLines(t *testing.T) {
	raw := "# a comment mentioning IBUS_COMPONENT_PATH\nQT_IM_MODULE=ibus\n\n" +
		ComponentPathVariable + "=/somewhere:/usr/share/ibus/component\n"
	got, found := environmentPath(raw)
	if !found {
		t.Fatal("environmentPath found nothing")
	}
	if want := "/somewhere:/usr/share/ibus/component"; got != want {
		t.Errorf("environmentPath = %q, want %q", got, want)
	}
}
