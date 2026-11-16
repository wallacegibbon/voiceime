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

// The endpoint finding answers the question dictation will ask: which model would a request
// name, and can the endpoint answer at all. Which of the two naming questions applies is
// decided by the settings: a setup that names its model is never cross-checked against the
// endpoint's catalogue, a setup that names nothing has to be told. Either way the endpoint is
// asked, because the status of that one request is the only evidence there is about the
// credential - and a refused credential is the failure a remote setup really meets.
func TestASREndpointCheckStates(t *testing.T) {
	serving := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"qwen3-asr-0.6b-q8_0"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer serving.Close()

	listingMany := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"one"},{"id":"two"}]}`))
	}))
	defer listingMany.Close()

	// A provider that has no model list at all: the model is named, so the list is not
	// needed, and 501 says nothing about the credential.
	noCatalogue := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Unsupported method", http.StatusNotImplemented)
	}))
	defer noCatalogue.Close()

	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided"}}`))
	}))
	defer refusing.Close()

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
			wantDetail: "no endpoint is configured",
			wantFix:    "asr-service.sh",
		},
		{
			name:       "a model on this machine, named by nothing",
			mutate:     func(c *config.Config) { c.ASRServerURL = serving.URL },
			wantStatus: "ok",
			wantDetail: `would name "qwen3-asr-0.6b-q8_0"`,
		},
		{
			name:       "a local endpoint listing more than one model",
			mutate:     func(c *config.Config) { c.ASRServerURL = listingMany.URL },
			wantStatus: "fail",
			wantDetail: "lists 2 models",
		},
		{
			name:       "an address the client cannot use",
			mutate:     func(c *config.Config) { c.ASRServerURL = "127.0.0.1:18080" },
			wantStatus: "fail",
			wantDetail: "asr_server_url",
		},
		{
			name:       "nothing listening, and nothing installed to listen",
			mutate:     func(c *config.Config) { c.ASRServerURL = deadURL(t) },
			wantStatus: "fail",
			wantDetail: "connection refused",
			wantFix:    "asr-service.sh",
		},
		{
			name:       "nothing listening, but the unit exists",
			mutate:     func(c *config.Config) { c.ASRServerURL = deadURL(t) },
			unit:       true,
			wantStatus: "fail",
			wantDetail: "connection refused",
			wantFix:    "systemctl --user restart",
		},
		{
			name: "a provider with no model list, its model named, its credential accepted",
			mutate: func(c *config.Config) {
				c.ASRServerURL = noCatalogue.URL
				c.Model = "gpt-4o-mini-transcribe"
				c.APIKey = "sk-test"
			},
			wantStatus: "ok",
			wantDetail: `would name "gpt-4o-mini-transcribe"`,
		},
		{
			name: "a provider that refuses the credential",
			mutate: func(c *config.Config) {
				c.ASRServerURL = refusing.URL
				c.Model = "gpt-4o-mini-transcribe"
				c.APIKey = "sk-wrong"
			},
			wantStatus: "fail",
			wantDetail: "Incorrect API key provided",
			wantFix:    "config set api_key",
		},
		{
			name: "a provider that needs a credential and was given none",
			mutate: func(c *config.Config) {
				c.ASRServerURL = refusing.URL
				c.Model = "gpt-4o-mini-transcribe"
			},
			wantStatus: "fail",
			wantDetail: "no credential",
			wantFix:    "config set api_key",
		},
		{
			name: "a provider whose host cannot be reached",
			mutate: func(c *config.Config) {
				c.ASRServerURL = "https://api.does-not-resolve.invalid/v1"
				c.Model = "gpt-4o-mini-transcribe"
			},
			wantStatus: "fail",
			wantDetail: "cannot be reached",
			wantFix:    "curl -s",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			tc.mutate(cfg)
			check := asrEndpointCheck(cfg, tc.unit)
			if check.ID != "asr_endpoint" {
				t.Fatalf("check id = %q, want asr_endpoint", check.ID)
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

// An endpoint that requires a credential is only reachable with one. This is the same request
// the engine makes, so the finding cannot say a setup works when the engine would be refused.
func TestEndpointCheckSendsTheCredential(t *testing.T) {
	needsToken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-test" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"no credential"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"cloud-asr-mini"}]}`))
	}))
	defer needsToken.Close()

	cfg := config.Default()
	cfg.ASRServerURL = needsToken.URL
	if check := asrEndpointCheck(cfg, false); check.Status == "ok" {
		t.Errorf("an endpoint that refuses the request was reported ok: %q", check.Detail)
	}

	cfg.APIKey = "sk-test"
	check := asrEndpointCheck(cfg, false)
	if check.Status != "ok" {
		t.Fatalf("status = %q, detail %q: the credential was set and not used", check.Status, check.Detail)
	}
	if !strings.Contains(check.Detail, "cloud-asr-mini") {
		t.Errorf("detail %q does not name the model the credential reached", check.Detail)
	}
}

// A model named in the settings is the model a request names, whatever the endpoint's list
// says: a provider's catalogue is not a promise about what that account accepts. The endpoint
// is still asked once - for the credential - so the request that asks has to carry it.
func TestNamedModelIsNotCrossCheckedWithTheEndpoint(t *testing.T) {
	var gotAuth string
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"something-else"}]}`))
	}))
	defer endpoint.Close()

	cfg := config.Default()
	cfg.ASRServerURL = endpoint.URL
	cfg.Model = "sensevoice-small-q8-audiocpp-v1"
	cfg.APIKey = "sk-test"
	check := asrEndpointCheck(cfg, true)
	if check.Status != "ok" {
		t.Fatalf("status = %q, detail %q", check.Status, check.Detail)
	}
	if !strings.Contains(check.Detail, `"sensevoice-small-q8-audiocpp-v1"`) {
		t.Errorf("detail %q: the settings name a model, and the endpoint's list overrode it", check.Detail)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("asked the endpoint with Authorization %q, want the credential a real request would carry", gotAuth)
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
// config file that decision resolved. The program does not write it - scripts/asr-service.sh
// does - but it reads it back, because a unit whose paths have gone missing starts forever
// and fails forever. So the reader has to survive what the writer writes, including a path
// with a space in it.
func TestUnitIsReadBackAsWritten(t *testing.T) {
	unit := "[Unit]\nDescription=voiceime recognition server\n\n[Service]\n" +
		"ExecStart=/home/u/playground/audio.cpp/build/linux-cpu-release/bin/audiocpp_server " +
		"--config /home/u/.local/share/voiceime/asr-server.json --log\n" +
		"Restart=on-failure\nRestartSec=2\n\n[Install]\nWantedBy=default.target\n"
	fields, err := unitExecStart(writeUnitForTest(t, unit))
	if err != nil {
		t.Fatalf("the unit cannot be read back: %v", err)
	}
	if firstField(fields) != "/home/u/playground/audio.cpp/build/linux-cpu-release/bin/audiocpp_server" {
		t.Errorf("ExecStart runs %q", firstField(fields))
	}
	if got := unitArgument(fields, "--config"); got != "/home/u/.local/share/voiceime/asr-server.json" {
		t.Errorf("the config reads back as %q", got)
	}

	// A path with a space in it is one quoted word, not two.
	spaced := "[Service]\nExecStart='/home/u/my audio.cpp/bin/audiocpp_server' " +
		"--config '/home/u/asr server.json' --log\n"
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

// writeUnitForTest puts a unit where the check can read it back.
func writeUnitForTest(t *testing.T, unit string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), unitName)
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// Every setting is accepted in the form the tool prints it in, and `config set` says what
// has to run for it to take effect - for every key.
func TestConfigSetNamesHowToApplyEverySetting(t *testing.T) {
	configPath := freshConfig(t)
	for _, field := range config.Default().Fields() {
		value := fmt.Sprint(field.Value)
		if field.Key == "asr_server_url" {
			value = "http://127.0.0.1:18080"
		}
		code, stdout, stderr := runForTest(t, "config", "set", field.Key, value, "--json", "--config", configPath)
		if code != ExitOK {
			t.Fatalf("config set %s %q: exit %d, stderr %s", field.Key, value, code, stderr)
		}
		payload := decodeOne(t, stdout)
		data, _ := payload["data"].(map[string]any)
		if data["valid"] != true {
			t.Errorf("config set %s %q reported an invalid configuration: %s", field.Key, value, stdout)
		}
		if applied, _ := data["apply_with"].(string); applied == "" {
			t.Errorf("config set %s does not say what applies the value", field.Key)
		}
	}
}
