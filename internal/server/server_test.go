package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The generated config is the contract with audio.cpp: one entry, offline mode, the
// model's own name as its id, and the request options a multipart upload cannot carry.
func TestWriteConfigDescribesOneModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "asr-server.json")
	spec := Spec{
		Model:          "/models/Qwen3-ASR-0.6B-GGUF/qwen3-asr-0.6b-q8_0.gguf",
		Family:         "qwen3_asr",
		Port:           18080,
		Backend:        "cpu",
		Threads:        4,
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
		t.Error("lazy_load is true: the model would load on the first request")
	}
	if config.Port != 18080 || config.Host != "127.0.0.1" {
		t.Errorf("address = %s:%d, want 127.0.0.1:18080", config.Host, config.Port)
	}
	if len(config.Models) != 1 {
		t.Fatalf("%d entries, want 1", len(config.Models))
	}
	entry := config.Models[0]
	if entry.ID != "qwen3-asr-0.6b-q8_0" {
		t.Errorf("id = %q, want the model file's own name", entry.ID)
	}
	if entry.Mode != "offline" || entry.Task != "asr" || entry.Family != "qwen3_asr" {
		t.Errorf("entry = %+v, want offline asr", entry)
	}
	if entry.DefaultRequestOptions["audio_chunk_mode"] != "none" {
		t.Errorf("options = %v, want the request options passed through", entry.DefaultRequestOptions)
	}
}

func TestWriteConfigRefusesAModelItCannotName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.json")
	if err := WriteConfig(Spec{}, path); err == nil {
		t.Error("WriteConfig accepted a spec with no model")
	}
	if err := WriteConfig(Spec{Model: "/models/x.gguf"}, path); err == nil {
		t.Error("WriteConfig accepted a model with no family")
	}
}

func TestEntryIDIsTheModelName(t *testing.T) {
	cases := map[string]string{
		"/models/Qwen3-ASR-0.6B-GGUF/qwen3-asr-0.6b-q8_0.gguf": "qwen3-asr-0.6b-q8_0",
		"/models/sense.gguf": "sense",
		"sense.gguf":         "sense",
		"/":                  "model",
	}
	for path, want := range cases {
		if got := EntryID(path); got != want {
			t.Errorf("EntryID(%q) = %q, want %q", path, got, want)
		}
	}
}

// The address a client is given is the one the config binds: one place decides it.
func TestBaseURLMatchesTheBoundAddress(t *testing.T) {
	if got := (Spec{Port: 9999}).BaseURL(); got != "http://127.0.0.1:9999" {
		t.Errorf("BaseURL = %q", got)
	}
	if got := (Spec{}).BaseURL(); got != "http://127.0.0.1:18080" {
		t.Errorf("BaseURL = %q, want the default port", got)
	}
}
