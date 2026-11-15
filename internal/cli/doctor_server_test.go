package cli

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallacegibbon/voiceime/internal/config"
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
			wantFix:    "voiceime apply",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			tc.mutate(cfg)
			check := asrServerCheck(cfg, tc.unit)
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

	cfg := config.Default()
	cfg.ASRServerURL = stale.URL
	// A checkpoint the file itself says is qwen3_asr: the check has to notice the running
	// service is serving something else.
	cfg.Model = ggufFixture(t, "qwen3_asr")

	check := asrServerCheck(cfg, true)
	if check.Status != "fail" {
		t.Fatalf("status = %q, want fail: the service serves other.gguf but the setting names qwen.gguf", check.Status)
	}
	if !strings.Contains(check.Detail, "qwen") || !strings.Contains(check.Detail, "other") {
		t.Errorf("detail %q does not name both the configured and the served model", check.Detail)
	}
	if !strings.Contains(check.Fix, "voiceime apply") {
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

// The unit is the record of a deployment decision: it names the audio.cpp binary and the
// config file `voiceime apply` resolved. Nothing at start-up looks either of them up again.
func TestServerUnitNamesTheAudioCPPAndTheConfigItWasGiven(t *testing.T) {
	unit := serverUnit("/home/u/playground/audio.cpp/build/linux-cpu-release/bin/audiocpp_server",
		"/home/u/.local/share/voiceime/asr-server.json")
	want := "ExecStart=/home/u/playground/audio.cpp/build/linux-cpu-release/bin/audiocpp_server " +
		"--config /home/u/.local/share/voiceime/asr-server.json --log"
	if !strings.Contains(unit, want) {
		t.Errorf("unit does not name what deployment resolved:\n%s", unit)
	}
	// This tool is not in the service's command line: it resolved the answer once, and
	// re-deriving it at every start is what made a start at login able to fail.
	fields, err := unitExecStart(writeUnitForTest(t, unit))
	if err != nil {
		t.Fatalf("the unit cannot be read back: %v", err)
	}
	if firstField(fields) != "/home/u/playground/audio.cpp/build/linux-cpu-release/bin/audiocpp_server" {
		t.Errorf("ExecStart runs %q, not the audio.cpp deployment chose", firstField(fields))
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
	// A path with a space in it survives being read back: `doctor` parses this line to
	// report what runs, and an unquoted space would be two words.
	spaced := serverUnit("/home/u/my audio.cpp/bin/audiocpp_server", "/home/u/asr server.json")
	if !strings.Contains(spaced, "'/home/u/my audio.cpp/bin/audiocpp_server'") {
		t.Errorf("paths with spaces are not quoted:\n%s", spaced)
	}
	spacedFields, err := unitExecStart(writeUnitForTest(t, spaced))
	if err != nil {
		t.Fatalf("a unit with spaces in its paths cannot be read back: %v", err)
	}
	if firstField(spacedFields) != "/home/u/my audio.cpp/bin/audiocpp_server" {
		t.Errorf("the binary reads back as %q", firstField(spacedFields))
	}
	if got := unitArgument(spacedFields, "--config"); got != "/home/u/asr server.json" {
		t.Errorf("the config reads back as %q", got)
	}
}

// writeUnitForTest puts a rendered unit where the check can read it back.
func writeUnitForTest(t *testing.T, unit string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), serverUnitName)
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Every setting is applied by one command, and `config set` says so - for every key.
func TestConfigSetNamesHowToApplyEverySetting(t *testing.T) {
	configPath := freshConfig(t)
	for _, field := range config.Default().Fields() {
		value := fmt.Sprint(field.Value)
		if field.Type == "string" {
			// A checkpoint is a file that exists; the default is empty because no
			// default can know where the user put one.
			if field.Key == "model" {
				value = ggufFixture(t, "sense_asr")
			}
		}
		code, stdout, stderr := runForTest(t, "config", "set", field.Key, value, "--json", "--config", configPath)
		if code != ExitOK {
			t.Fatalf("config set %s %q: exit %d, stderr %s", field.Key, value, code, stderr)
		}
		payload := decodeOne(t, stdout)["data"].(map[string]any)
		if payload["apply_with"] != "voiceime apply" {
			t.Errorf("%s: apply_with = %v, want the one command that applies a setting", field.Key, payload["apply_with"])
		}
	}
}
