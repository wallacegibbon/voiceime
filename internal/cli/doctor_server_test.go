package cli

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// The endpoint finding reports on the address dictation posts to: is it reachable, does it
// accept the credential, and is there a model id to name. The address and the id are used
// exactly as configured - the program asks an address nothing - so the finding is the request
// the engine would make, with ten milliseconds of silence in place of the audio.
func TestASREndpointCheckStates(t *testing.T) {
	answering := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"text":""}`))
	}))
	defer answering.Close()

	noRoute := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "Unsupported method", http.StatusNotImplemented)
	}))
	defer noRoute.Close()

	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"Incorrect API key provided"}}`))
	}))
	defer refusing.Close()

	cases := []struct {
		name       string
		mutate     func(*config.Config)
		wantFix    string
		wantStatus string
		wantDetail string
	}{
		{
			name:       "nothing configured",
			mutate:     func(c *config.Config) { c.ASRServerURL = "" },
			wantStatus: "fail",
			wantDetail: "no address is configured",
			wantFix:    `write asr_server_url = "the whole address, path included"`,
		},
		{
			name: "an address with nothing to ask it for",
			mutate: func(c *config.Config) {
				c.ASRServerURL = answering.URL + "/v1/audio/transcriptions"
			},
			wantStatus: "fail",
			wantDetail: "nothing names the model",
			wantFix:    `write model = "<id>"`,
		},
		{
			name: "a provider with nothing to ask it for",
			mutate: func(c *config.Config) {
				c.ASRServerURL = "https://api.example.com/v1/audio/transcriptions"
			},
			wantStatus: "fail",
			wantDetail: "nothing names the model",
			wantFix:    "write model = \"",
		},
		{
			name: "an address, and the id to ask it for",
			mutate: func(c *config.Config) {
				c.ASRServerURL = answering.URL + "/v1/audio/transcriptions"
				c.Model = "sensevoice-small-q8-audiocpp-v1"
			},
			wantStatus: "ok",
			wantDetail: `would name "sensevoice-small-q8-audiocpp-v1"`,
		},
		{
			name: "an address that is not a URL",
			mutate: func(c *config.Config) {
				c.ASRServerURL = "127.0.0.1:18080"
				c.Model = "x"
			},
			wantStatus: "fail",
			wantDetail: "asr_server_url",
		},
		{
			name: "nothing is listening at the address",
			mutate: func(c *config.Config) {
				c.ASRServerURL = deadURL(t) + "/v1/audio/transcriptions"
				c.Model = "x"
			},
			wantStatus: "fail",
			wantDetail: "cannot be reached",
			wantFix:    "curl -s",
		},
		{
			name: "a provider that answers, with its model named and its credential accepted",
			mutate: func(c *config.Config) {
				c.ASRServerURL = answering.URL + "/api/paas/v4/audio/transcriptions"
				c.Model = "glm-asr-2512"
				c.APIKey = "sk-test"
			},
			wantStatus: "ok",
			wantDetail: `would name "glm-asr-2512"`,
		},
		{
			name: "an address that has no such route, which is not about the credential",
			mutate: func(c *config.Config) {
				c.ASRServerURL = noRoute.URL + "/v1/audio/transcriptions"
				c.Model = "x"
				c.APIKey = "sk-test"
			},
			wantStatus: "ok",
			wantDetail: "would name",
		},
		{
			name: "a provider that refuses the credential",
			mutate: func(c *config.Config) {
				c.ASRServerURL = refusing.URL + "/v1/audio/transcriptions"
				c.Model = "glm-asr-2512"
				c.APIKey = "sk-wrong"
			},
			wantStatus: "fail",
			wantDetail: "Incorrect API key provided",
			wantFix:    "write api_key = \"",
		},
		{
			name: "a provider that needs a credential and was given none",
			mutate: func(c *config.Config) {
				c.ASRServerURL = refusing.URL + "/v1/audio/transcriptions"
				c.Model = "glm-asr-2512"
			},
			wantStatus: "fail",
			wantDetail: "no credential",
			wantFix:    "write api_key = \"",
		},
		{
			name: "a host that cannot be reached",
			mutate: func(c *config.Config) {
				c.ASRServerURL = "https://api.does-not-resolve.invalid/v1/audio/transcriptions"
				c.Model = "glm-asr-2512"
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
			check := asrEndpointCheck(cfg)
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

// The probe carries the credential and the model id exactly as the request would. A finding
// that says a setup works has to be the request that works, or it is worth nothing.
func TestEndpointCheckSendsTheCredential(t *testing.T) {
	var gotAuth, gotModel string
	needsToken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if gotAuth != "Bearer sk-test" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"no credential"}}`))
			return
		}
		_ = r.ParseMultipartForm(1 << 20)
		gotModel = r.FormValue("model")
		_, _ = w.Write([]byte(`{"text":""}`))
	}))
	defer needsToken.Close()

	cfg := config.Default()
	cfg.ASRServerURL = needsToken.URL + "/v1/audio/transcriptions"
	cfg.Model = "glm-asr-2512"
	if check := asrEndpointCheck(cfg); check.Status == "ok" {
		t.Errorf("an endpoint that refuses the request was reported ok: %q", check.Detail)
	}

	cfg.APIKey = "sk-test"
	check := asrEndpointCheck(cfg)
	if check.Status != "ok" {
		t.Fatalf("status = %q, detail %q: the credential was set and not used", check.Status, check.Detail)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("asked with Authorization %q, want the credential a real request carries", gotAuth)
	}
	if gotModel != "glm-asr-2512" {
		t.Errorf("asked with model %q, want the id from the settings", gotModel)
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
