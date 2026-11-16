package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// The audio.cpp config is written once by scripts/asr-service.sh and named by the service
// unit, which systemd starts at login - before any voiceime command has run. So it must not
// live in a directory that is emptied at logout: the install would work until the machine was
// rebooted and then fail with nothing to tell anyone why. The check lives here, in the
// program that does not write the file, because the path it must not be is a fact about the
// machine this program reads.
func TestUnitNamesAConfigThatOutlivesTheSessionThatWroteIt(t *testing.T) {
	session := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", session)

	got := generatedConfigPath()
	if want := filepath.Join(config.DataDir(), generatedConfigName); got != want {
		t.Errorf("generatedConfigPath = %q, want %q", got, want)
	}
	if strings.HasPrefix(got, session+string(filepath.Separator)) {
		t.Errorf("generatedConfigPath = %q, which a logout empties", got)
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

	generated := generatedConfigPath()
	if strings.HasPrefix(generated, configDir+string(filepath.Separator)) {
		t.Errorf("the generated config resolves inside the configuration directory: %q", generated)
	}
	if filepath.Dir(generated) == filepath.Dir(config.DefaultPath()) {
		t.Errorf("the generated config shares a directory with %s, the file a person edits", config.DefaultPath())
	}
}
