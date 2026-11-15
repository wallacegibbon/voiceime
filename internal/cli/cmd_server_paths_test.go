package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// The audio.cpp config is rendered by `voiceime apply` and named by the service unit, which
// systemd starts at login - before any voiceime command has run. So it must not live in a
// directory that is emptied at logout: the install would work until the machine was rebooted
// and then fail with nothing to tell anyone why.
func TestServerConfigOutlivesTheSessionThatWroteIt(t *testing.T) {
	session := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", session)

	got := serverConfigFile()
	if want := filepath.Join(config.DataDir(), serverConfigName); got != want {
		t.Errorf("serverConfigFile = %q, want %q", got, want)
	}
	if strings.HasPrefix(got, session+string(filepath.Separator)) {
		t.Errorf("serverConfigFile = %q, which a logout empties", got)
	}
}

// The point of moving it: the configuration directory holds the one file a person or an
// agent edits, and nothing that only looks like it. This is asserted with no session
// directory available, because that is the fallback where the two would collide.
func TestServerConfigIsNotASecondConfigurationFile(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("XDG_CACHE_HOME", filepath.Join(t.TempDir(), "cache"))

	generated := serverConfigFile()
	if strings.HasPrefix(generated, configDir+string(filepath.Separator)) {
		t.Errorf("the generated config resolves inside the configuration directory: %q", generated)
	}
	if filepath.Dir(generated) == filepath.Dir(config.DefaultPath()) {
		t.Errorf("the generated config shares a directory with %s, the file a person edits", config.DefaultPath())
	}
}
