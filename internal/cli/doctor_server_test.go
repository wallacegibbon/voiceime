package cli

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/models"
)

func TestASRServerCheckStates(t *testing.T) {
	// A server serving one model, as voiceime does.
	serving := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"qwen3-asr-0.6b-q8_0"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer serving.Close()

	// A server that answers but serves more models than voiceime installs.
	wrongEntries := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		default:
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"one"},{"id":"two"}]}`))
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
			name:       "serving one model",
			mutate:     func(c *config.Config) { c.ASRServerURL = serving.URL },
			wantStatus: "ok",
			wantDetail: "is serving qwen3-asr-0.6b-q8_0",
		},
		{
			name:       "a server with more than one model",
			mutate:     func(c *config.Config) { c.ASRServerURL = wrongEntries.URL },
			wantStatus: "fail",
			wantDetail: "serves 2 models",
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
			wantFix:    "config apply",
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

func TestASRServerCheckCatchesAStaleModel(t *testing.T) {
	stale := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"object":"list","data":[
				{"id":"other","path":"/models/other.gguf","mode":"offline"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer stale.Close()

	scanned := []models.Model{
		{Path: "/models/sense.gguf", Inspected: true, Family: "sense_asr", Tasks: []models.Task{{Name: "asr"}}},
		{Path: "/models/qwen.gguf", Inspected: true, Family: "qwen3_asr", Tasks: []models.Task{{Name: "asr"}}},
	}
	cfg := config.Default()
	cfg.ASRServerURL = stale.URL
	cfg.Model = "/models/qwen.gguf"
	cfg.ASRBin = "/bin/true" // not used by the check, but keeps the spec resolvable

	check := asrServerCheck(cfg, scanned, true)
	if check.Status != "fail" {
		t.Fatalf("status = %q, want fail: the service serves other.gguf but the setting names qwen.gguf", check.Status)
	}
	if !strings.Contains(check.Detail, "qwen") || !strings.Contains(check.Detail, "other") {
		t.Errorf("detail %q does not name both the configured and the served model", check.Detail)
	}
	if !strings.Contains(check.Fix, "config apply") {
		t.Errorf("fix %q does not say how to apply the changed setting", check.Fix)
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

func TestServerUnitRunsVoiceimeWithSubcommand(t *testing.T) {
	unit := serverUnit("/home/u/.config/voiceime/config.toml")
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
	// Documentation= is only written when the manual is actually beside the binary; this
	// test runs from a temporary build directory, where it is not.
	if strings.Contains(unit, "Documentation=") {
		t.Errorf("unit cites a manual that is not installed:\n%s", unit)
	}
	// The settings file has to be named. Without it the service reads the default path,
	// so a `server install --config <file>` would install a service that serves
	// something other than the file it was given.
	if !strings.Contains(unit, "--config /home/u/.config/voiceime/config.toml") {
		t.Errorf("unit does not name the configuration file:\n%s", unit)
	}
}

// Every setting is applied by one command, and `config set` says so - for every key.
func TestConfigSetNamesHowToApplyEverySetting(t *testing.T) {
	configPath, _ := freshConfig(t)
	for _, field := range config.Default().Fields() {
		value := fmt.Sprint(field.Value)
		if field.Type == "map[string]string" {
			value = "audio_chunk_mode=none"
		}
		code, stdout, stderr := runForTest(t, "config", "set", field.Key, value, "--json", "--config", configPath)
		if code != ExitOK {
			t.Fatalf("config set %s %q: exit %d, stderr %s", field.Key, value, code, stderr)
		}
		payload := decodeOne(t, stdout)["data"].(map[string]any)
		if payload["apply_with"] != "voiceime config apply" {
			t.Errorf("%s: apply_with = %v, want the one command that applies a setting", field.Key, payload["apply_with"])
		}
	}
}
