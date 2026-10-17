package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The generated config is the contract with audio.cpp, and this is the whole design in
// one document: lazy_load false (the weights belong to the service's startup, not to a
// request), two entries because the server serialises per model, and the request
// options a multipart upload cannot carry.
func TestWriteConfigDescribesBothEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "asr-server.json")
	spec := Spec{
		StreamModel:    "/models/sense.gguf",
		StreamFamily:   "sense_asr",
		RepairModel:    "/models/qwen.gguf",
		RepairFamily:   "qwen3_asr",
		Port:           18080,
		Backend:        "cpu",
		Threads:        4,
		ChunkSeconds:   3,
		RequestOptions: map[string]string{"audio_chunk_mode": "none"},
	}
	if err := WriteConfig(spec, path); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var config wireConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatalf("not json: %v", err)
	}

	if config.LazyLoad {
		t.Error("lazy_load is true: the models would load on the first request, which is the cost the service exists to remove")
	}
	if config.Port != 18080 || config.Host != "127.0.0.1" {
		t.Errorf("address = %s:%d, want 127.0.0.1:18080", config.Host, config.Port)
	}
	if len(config.Models) != 2 {
		t.Fatalf("%d entries, want 2: the server holds a model for a whole live session, so the two roles cannot share one", len(config.Models))
	}
	stream, repair := config.Models[0], config.Models[1]
	if stream.ID != DefaultStreamID || stream.Mode != "streaming" || stream.Path != "/models/sense.gguf" {
		t.Errorf("streaming entry = %+v, want the streaming id in streaming mode", stream)
	}
	if stream.DefaultRequestOptions["audio_chunk_duration_sec"] != "3" {
		t.Errorf("streaming options = %v, want the configured window", stream.DefaultRequestOptions)
	}
	if repair.ID != DefaultID || repair.Mode != "offline" || repair.Family != "qwen3_asr" {
		t.Errorf("repair entry = %+v, want the offline entry", repair)
	}
	if repair.DefaultRequestOptions["audio_chunk_mode"] != "none" {
		t.Errorf("repair options = %v, want the repair route's chunk mode", repair.DefaultRequestOptions)
	}
}

// Same weights in both roles is a legitimate configuration - a small model used for
// everything - and it still needs two entries.
func TestWriteConfigAllowsTheSameModelTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	spec := Spec{
		StreamModel:  "/models/sense.gguf",
		StreamFamily: "sense_asr",
		RepairModel:  "/models/sense.gguf",
		RepairFamily: "sense_asr",
	}
	if err := WriteConfig(spec, path); err != nil {
		t.Fatalf("WriteConfig: %v", err)
	}
	raw, _ := os.ReadFile(path)
	var config wireConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		t.Fatalf("not json: %v", err)
	}
	if len(config.Models) != 2 {
		t.Fatalf("%d entries, want 2", len(config.Models))
	}
}

// A wave file the server cannot load is refused here rather than as a service that
// starts and then fails every request.
func TestWriteConfigNeedsBothModelsAndFamilies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	full := Spec{
		StreamModel:  "/models/a.gguf",
		StreamFamily: "sense_asr",
		RepairModel:  "/models/b.gguf",
		RepairFamily: "qwen3_asr",
	}
	cases := []struct {
		name   string
		mutate func(*Spec)
		want   string
	}{
		{"no streaming model", func(s *Spec) { s.StreamModel = "" }, "streaming model"},
		{"no streaming family", func(s *Spec) { s.StreamFamily = "" }, "family"},
		{"no repair model", func(s *Spec) { s.RepairModel = "" }, "repair model"},
		{"no repair family", func(s *Spec) { s.RepairFamily = "" }, "family"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec := full
			tc.mutate(&spec)
			err := WriteConfig(spec, path)
			if err == nil {
				t.Fatal("WriteConfig accepted an incomplete spec")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestParseURL(t *testing.T) {
	cases := []struct {
		raw      string
		wantHost string
		wantPort int
		wantErr  bool
	}{
		{raw: "http://127.0.0.1:18080", wantHost: "127.0.0.1", wantPort: 18080},
		{raw: "http://localhost:8080/", wantHost: "localhost", wantPort: 8080},
		{raw: "https://example.com", wantHost: "example.com", wantPort: 443},
		{raw: "http://example.com", wantHost: "example.com", wantPort: 80},
		{raw: "127.0.0.1:18080", wantErr: true},
		{raw: "", wantErr: true},
		{raw: "http://:18080", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			host, port, err := ParseURL(tc.raw)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseURL(%q) succeeded, want an error", tc.raw)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseURL(%q): %v", tc.raw, err)
			}
			if host != tc.wantHost || port != tc.wantPort {
				t.Errorf("ParseURL(%q) = %s:%d, want %s:%d", tc.raw, host, port, tc.wantHost, tc.wantPort)
			}
		})
	}
}

func TestSpecDefaultsAndLocality(t *testing.T) {
	spec := Spec{}
	if got := spec.BaseURL(); got != "http://127.0.0.1:18080" {
		t.Errorf("BaseURL() = %q, want the documented default", got)
	}
	if !IsLocalHost("127.0.0.1") || !IsLocalHost("localhost") || !IsLocalHost("::1") {
		t.Error("loopback addresses are not recognised as local")
	}
	if IsLocalHost("192.0.2.10") {
		t.Error("a remote address is being treated as local")
	}
}
