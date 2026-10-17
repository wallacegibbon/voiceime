package cli

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/models"
)

// The server is the one prerequisite dictation cannot work without, and nothing else
// in an installation verifies it: voiceime no longer starts processes, so a missing
// service is not a slow repair, it is no dictation at all. These are the states the
// check has to tell apart, and the fix each one is worth.
func TestASRServerCheckStates(t *testing.T) {
	// A server serving both entries the checks ask for.
	serving := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"stream"},{"id":"refine"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer serving.Close()

	// A server that answers but offers neither entry.
	wrongEntries := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"something-else"}]}`))
		}
	}))
	defer wrongEntries.Close()

	cases := []struct {
		name       string
		mutate     func(*config.Config)
		unit       bool
		wantFix    string
		wantStatus string
		wantDetail string
	}{
		{
			name:       "nothing configured",
			mutate:     func(c *config.Config) { c.ASRServerURL = "" },
			wantStatus: "fail",
			wantDetail: "no server is configured",
		},
		{
			name:       "serving both entries",
			mutate:     func(c *config.Config) { c.ASRServerURL = serving.URL },
			wantStatus: "ok",
			wantDetail: "is serving stream (partials) and refine (repair)",
		},
		{
			name:       "a server without the entries",
			mutate:     func(c *config.Config) { c.ASRServerURL = wrongEntries.URL },
			wantStatus: "fail",
			wantDetail: "has no entry",
		},
		{
			name:       "unparseable address",
			mutate:     func(c *config.Config) { c.ASRServerURL = "127.0.0.1:18080" },
			wantStatus: "fail",
			wantDetail: "asr_server_url",
		},
		{
			name:       "nothing listening and no unit installed",
			mutate:     func(c *config.Config) { c.ASRServerURL = deadURL(t) },
			wantStatus: "fail",
			wantDetail: "no service on this machine is installed",
		},
		{
			name:       "nothing listening but the unit exists",
			mutate:     func(c *config.Config) { c.ASRServerURL = deadURL(t) },
			unit:       true,
			wantStatus: "fail",
			wantDetail: "is not answering",
			wantFix:    "systemctl --user start",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			tc.mutate(cfg)
			check := asrServerCheck(cfg, []models.Model{}, tc.unit)
			if check.ID != "asr_server" {
				t.Fatalf("check id = %q, want asr_server", check.ID)
			}
			if check.Status != tc.wantStatus {
				t.Fatalf("status = %q, want %q (detail: %s)", check.Status, tc.wantStatus, check.Detail)
			}
			if !strings.Contains(check.Detail, tc.wantDetail) {
				t.Errorf("detail %q does not mention %q", check.Detail, tc.wantDetail)
			}
			if check.Status == "fail" && check.Fix == "" {
				t.Error("a failing check without a fix is not actionable")
			}
			if tc.wantFix != "" && !strings.Contains(check.Fix, tc.wantFix) {
				t.Errorf("fix %q does not mention %q", check.Fix, tc.wantFix)
			}
		})
	}
}

// A service left on the previous checkpoint is the failure this check exists for: the
// entry ids do not change when a setting does, so nothing else notices, and the symptom
// is slightly different text rather than an error.
func TestASRServerCheckCatchesAStaleModel(t *testing.T) {
	stale := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"object":"list","data":[
				{"id":"stream","path":"/models/sense.gguf","mode":"streaming"},
				{"id":"refine","path":"/models/other.gguf","mode":"offline"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer stale.Close()

	scanned := []models.Model{
		{Path: "/models/sense.gguf", Family: "sense_asr", Tasks: []models.Task{{Name: "asr"}}},
		{Path: "/models/qwen.gguf", Family: "qwen3_asr", Tasks: []models.Task{{Name: "asr"}}},
	}
	cfg := config.Default()
	cfg.ASRServerURL = stale.URL
	cfg.StreamingModel = "/models/sense.gguf"
	cfg.RefineModel = "/models/qwen.gguf"
	cfg.ASRBin = "/bin/true" // not used by the check, but keeps the spec resolvable

	check := asrServerCheck(cfg, scanned, true)
	if check.Status != "fail" {
		t.Fatalf("status = %q, want fail: the service serves other.gguf but the setting names qwen.gguf", check.Status)
	}
	if !strings.Contains(check.Detail, "qwen.gguf") || !strings.Contains(check.Detail, "other.gguf") {
		t.Errorf("detail %q does not name both files", check.Detail)
	}
	if !strings.Contains(check.Fix, "server install") {
		t.Errorf("fix %q does not say how to fix it", check.Fix)
	}
}

// deadURL returns an address on this machine that nothing listens on.
func deadURL(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	address := listener.Addr().String()
	listener.Close()
	return "http://" + address
}

// The unit file is what makes the server exist across reboots, and what it starts
// matters as much as whether it starts: it runs voiceime, which renders the server
// config from the settings, so a changed model needs only a restart and a stale config
// is impossible.
func TestServerUnitRunsVoiceimeWithSubcommand(t *testing.T) {
	unit := serverUnit(nil, "/opt/audio.cpp/build/linux-cpu-release/bin/audiocpp_server", "/home/u/.config/voiceime/asr-server.json")
	if !strings.Contains(unit, " server run") {
		t.Errorf("unit does not run the server subcommand:\n%s", unit)
	}
	if strings.Contains(unit, "audiocpp_server --config") {
		t.Error("unit starts audio.cpp directly, so a changed setting would need the config rewritten by hand")
	}
	if !strings.Contains(unit, "Restart=on-failure") {
		t.Error("unit does not restart a server that dies")
	}
	if !strings.Contains(unit, "WantedBy=default.target") {
		t.Error("unit is not enabled for the user session")
	}
	if !strings.Contains(unit, "Documentation=file:///opt/audio.cpp/build/linux-cpu-release/docs/AGENT.md") {
		t.Errorf("unit does not document itself beside the binary:\n%s", unit)
	}
}

// Settings that only reach the service at startup say so, because a `config set` that
// looks like it did nothing is the failure this project keeps running into.
func TestServiceSettingsAreNamed(t *testing.T) {
	for _, key := range []string{"refine_model", "streaming_model", "chunk_seconds", "threads", "backend"} {
		if !serviceSetting(key) {
			t.Errorf("%s is compiled into the service but is not reported as such", key)
		}
	}
	for _, key := range []string{"endpoint_silence_ms", "hotkey", "silence_rms_threshold"} {
		if serviceSetting(key) {
			t.Errorf("%s is read per session but is reported as a service setting", key)
		}
	}
}
