package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// The audio.cpp config is rendered from the settings and never read back by voiceime, so
// it belongs with the session's disposable state. Where it lands is a contract: `server
// status` reports this path, and an agent uses it to see which model is really served.
func TestServerConfigIsGeneratedInTheRuntimeDir(t *testing.T) {
	session := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", session)

	got := serverConfigFile()
	if want := filepath.Join(session, "voiceime", serverConfigName); got != want {
		t.Errorf("serverConfigFile = %q, want it under the session directory %q", got, want)
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
