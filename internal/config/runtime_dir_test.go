package config

import (
	"os"
	"path/filepath"
	"testing"
)

// A desktop session owns the runtime directory, so it is used when it is there.
func TestRuntimeDirUsesTheSessionDirectory(t *testing.T) {
	session := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", session)

	got := RuntimeDir()
	if want := filepath.Join(session, AppName); got != want {
		t.Errorf("RuntimeDir = %q, want %q", got, want)
	}
}

// A declared directory that does not exist is not a session directory: falling back to
// it would send the generated service config to nowhere.
func TestRuntimeDirIgnoresASessionDirectoryThatIsNotThere(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(t.TempDir(), "gone"))
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	got := RuntimeDir()
	if filepath.Dir(got) != filepath.Join(os.Getenv("XDG_CACHE_HOME"), "voiceime") {
		t.Errorf("RuntimeDir = %q, want the cache fallback", got)
	}
}

// Without a session directory the answer is still disposable, and still absolute: a
// generated path that moved with the working directory would be one file to the
// installer and a different file to the service.
func TestRuntimeDirWithoutASessionOrAHomeStaysAbsolute(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "")

	got := RuntimeDir()
	if !filepath.IsAbs(got) {
		t.Errorf("RuntimeDir = %q, want an absolute path", got)
	}
}

// A relative cache setting must not make the runtime path relative either.
func TestRuntimeDirWithARelativeCacheStaysAbsolute(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("XDG_CACHE_HOME", "relative/cache")

	if got := RuntimeDir(); !filepath.IsAbs(got) {
		t.Errorf("RuntimeDir = %q, want an absolute path", got)
	}
}
