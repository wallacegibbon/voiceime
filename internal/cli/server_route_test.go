package cli

import (
	"strings"
	"testing"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// The engine's first log line says what it talks to, and a person reading it months later has
// to be able to believe it. It cannot name the model unit: the engine does not start that
// unit, cannot know whether it exists, and the endpoint may be a provider - the case where a
// local unit in the log would describe a machine that is not involved. And the credential does
// not belong in a file that outlives the session.
func TestServerDescriptionNamesOnlyWhatTheEngineKnows(t *testing.T) {
	cfg := config.Default()
	cfg.ASRServerURL = "http://127.0.0.1:18080/"
	if got := serverDescription(cfg); got != "server http://127.0.0.1:18080" {
		t.Errorf("local endpoint with nothing named: %q", got)
	}

	cfg.ASRServerURL = "https://api.example.com/v1"
	cfg.Model = "gpt-4o-mini-transcribe"
	cfg.APIKey = "sk-not-for-the-log"
	got := serverDescription(cfg)
	want := "server https://api.example.com/v1 (model gpt-4o-mini-transcribe)"
	if got != want {
		t.Errorf("provider endpoint: %q, want %q", got, want)
	}
	if strings.Contains(got, unitName) || strings.Contains(got, "service") {
		t.Errorf("%q names a service this process does not own", got)
	}
	if strings.Contains(got, cfg.APIKey) {
		t.Errorf("%q leaks the credential into a log file", got)
	}
}
