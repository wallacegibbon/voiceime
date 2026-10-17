package cli

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/models"
)

// The resident-server route is the one part of a working installation that nothing
// else verifies: when it cannot start, dictation falls back to a process per
// utterance and keeps working, so a clean `doctor` would be telling the user their
// faster route is fine when it is not. These cases are the states it has to tell
// apart, and the answer each one is worth.
func TestASRServerCheckStates(t *testing.T) {
	// A server that answers /health and offers the id the route asks for.
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"refine"}]}`))
	})
	serving := httptest.NewServer(mux)
	defer serving.Close()

	// A server that answers but offers a different id: the hand-written config
	// mistake that otherwise shows up per utterance as a repair that fell back.
	otherMux := http.NewServeMux()
	otherMux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	otherMux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"stream"}]}`))
	})
	other := httptest.NewServer(otherMux)
	defer other.Close()

	installed := []models.Model{{
		Path:   "/models/refine.gguf",
		Family: "qwen3_asr",
		Tasks:  []models.Task{{Name: "asr", Modes: []string{"offline"}}},
	}}

	cases := []struct {
		name       string
		mutate     func(*config.Config)
		wantStatus string
		wantDetail string
	}{
		{
			name:       "not configured is not a problem",
			mutate:     func(c *config.Config) { c.ASRServerURL = "" },
			wantStatus: "ok",
			wantDetail: "off:",
		},
		{
			name:       "a server that is up and serving the model",
			mutate:     func(c *config.Config) { c.ASRServerURL = serving.URL },
			wantStatus: "ok",
			wantDetail: "is serving model refine",
		},
		{
			name:       "a server that is up but does not offer the id",
			mutate:     func(c *config.Config) { c.ASRServerURL = other.URL },
			wantStatus: "fail",
			wantDetail: "has no model",
		},
		{
			name: "unparseable address",
			mutate: func(c *config.Config) {
				c.ASRServerURL = "127.0.0.1:18080"
			},
			wantStatus: "fail",
			wantDetail: "asr_server_url",
		},
		{
			name: "nothing listening and autostart off",
			mutate: func(c *config.Config) {
				c.ASRServerURL = deadURL(t)
				c.ASRServerAutostart = false
			},
			wantStatus: "warn",
			wantDetail: "asr_server_autostart is off",
		},
		{
			name: "autostart with no model to serve",
			mutate: func(c *config.Config) {
				c.ASRServerURL = deadURL(t)
				c.RefineModel = ""
			},
			wantStatus: "fail",
			wantDetail: "refine_model is empty",
		},
		{
			name: "autostart with a model it cannot see",
			mutate: func(c *config.Config) {
				c.ASRServerURL = deadURL(t)
				c.RefineModel = "/models/missing.gguf"
			},
			wantStatus: "fail",
			wantDetail: "not a model voiceime can see",
		},
		{
			name: "autostart with everything it needs",
			mutate: func(c *config.Config) {
				c.ASRServerURL = deadURL(t)
				c.RefineModel = "/models/refine.gguf"
			},
			wantStatus: "ok",
			wantDetail: "started on demand",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Default()
			cfg.ASRServerModel = "refine"
			cfg.ASRServerAutostart = true
			// The binary has to resolve as if audio.cpp were installed, whatever this
			// machine has: the check is about configuration, not about the developer's
			// checkout.
			cfg.ASRBin = selfBinary(t)
			tc.mutate(cfg)

			check := asrServerCheck(cfg, installed)
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
		})
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

// selfBinary returns an audiocpp_cli path whose directory also holds
// audiocpp_server, which is how an audio.cpp build tree looks and what
// FindServerBinary() discovery expects. The check is about configuration, so it must
// not depend on the developer's own checkout.
func selfBinary(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"audiocpp_cli", "audiocpp_server"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	return filepath.Join(dir, "audiocpp_cli")
}
