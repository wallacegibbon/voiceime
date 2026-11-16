package cli

import (
	"strings"
	"testing"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// The engine's first log line says what it talks to, and a person reading it months later has
// to be able to believe it: the address appears exactly as the settings hold it, because that is
// the address the next request went to. It cannot name the model unit: the engine does not start
// that unit, cannot know whether it exists, and the endpoint may be a provider - the case where a
// local unit in the log would describe a machine that is not involved. And the credential does
// not belong in a file that outlives the session.
func TestServerDescriptionNamesOnlyWhatTheEngineKnows(t *testing.T) {
	cfg := config.Default()
	cfg.ASRServerURL = "http://127.0.0.1:18080/v1/audio/transcriptions"
	cfg.Model = "sensevoice-small-q8-audiocpp-v1"
	got := serverDescription(cfg)
	want := "server http://127.0.0.1:18080/v1/audio/transcriptions (model sensevoice-small-q8-audiocpp-v1)"
	if got != want {
		t.Errorf("local endpoint: %q, want %q", got, want)
	}

	cfg.ASRServerURL = "https://open.bigmodel.cn/api/paas/v4/audio/transcriptions"
	cfg.Model = "glm-asr-2512"
	cfg.APIKey = "sk-not-for-the-log"
	got = serverDescription(cfg)
	want = "server https://open.bigmodel.cn/api/paas/v4/audio/transcriptions (model glm-asr-2512)"
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
