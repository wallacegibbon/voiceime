// Package server describes the resident audio.cpp server that serves both recognition
// roles: writing its config, and asking it whether it is there.
//
// It exists because the alternative - a process per utterance, and a process per
// dictation - pays the model load every time. Measured on the reference machine with
// Qwen3-ASR-0.6B: ~4.4s of CPU per fresh process whatever the audio length, against
// ~1.4s once, which is the difference between a large refine model being affordable
// and not.
//
// The server is a *service*: a systemd user unit with `lazy_load: false`, so both
// models are resident when the machine starts the unit and a dictation session only
// opens a connection. Nothing here starts, stops or supervises it - that is systemd's
// job, which is why this package is a config document and a probe rather than a
// process manager.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultID is the entry the offline repair decode uses.
	DefaultID = "refine"
	// DefaultStreamID is the entry that serves live partials.
	DefaultStreamID = "stream"
	// DefaultPort is where the unit listens unless told otherwise.
	DefaultPort = 18080
)

// Spec is what has to be decided before a server config can be written.
type Spec struct {
	// StreamModel and RepairModel are the GGUFs the two entries serve. They may be
	// the same file: audio.cpp holds one model per *request*, and a live session
	// holds its entry for the whole dictation, so the two roles need two entries even
	// when they want the same weights.
	StreamModel  string
	StreamFamily string
	RepairModel  string
	RepairFamily string
	StreamID     string
	RepairID     string
	Host         string
	Port         int
	Backend      string
	Threads      int
	// ChunkSeconds is the streaming window: how much audio a draft is decoded from.
	ChunkSeconds float64
	// RequestOptions are the options every repair request carries. They cannot ride a
	// multipart transcription upload - only the JSON route reads an options object -
	// so they belong in the entry's default_request_options, which is also where the
	// CLI's old --request-option values map to.
	RequestOptions map[string]string
}

// BaseURL is where a client reaches a server with this spec.
func (s Spec) BaseURL() string {
	host := s.Host
	if host == "" {
		host = "127.0.0.1"
	}
	port := s.Port
	if port == 0 {
		port = DefaultPort
	}
	return fmt.Sprintf("http://%s:%d", host, port)
}

// StreamIDOrDefault and RepairIDOrDefault name the entries.
func (s Spec) StreamIDOrDefault() string { return defaultID(s.StreamID, DefaultStreamID) }
func (s Spec) RepairIDOrDefault() string { return defaultID(s.RepairID, DefaultID) }

func defaultID(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// WriteConfig writes the server's config and returns its path.
//
// lazy_load is false, and that is the point of the service: the weights belong to its
// startup, not to a request. A repair or a dictation that arrives afterwards finds
// them resident, which is the whole reason the route costs 0.5s instead of 5s.
func WriteConfig(spec Spec, path string) error {
	switch {
	case spec.StreamModel == "":
		return fmt.Errorf("no streaming model to serve")
	case spec.StreamFamily == "":
		return fmt.Errorf("%s: audio.cpp did not report a family", spec.StreamModel)
	case spec.RepairModel == "":
		return fmt.Errorf("no repair model to serve")
	case spec.RepairFamily == "":
		return fmt.Errorf("%s: audio.cpp did not report a family", spec.RepairModel)
	}
	backend := spec.Backend
	if backend == "" {
		backend = "cpu"
	}
	port := spec.Port
	if port == 0 {
		port = DefaultPort
	}
	streamOptions := map[string]string{}
	if spec.ChunkSeconds > 0 {
		streamOptions["audio_chunk_mode"] = "fixed"
		streamOptions["audio_chunk_duration_sec"] = trimFloat(spec.ChunkSeconds)
	}
	config := wireConfig{
		Host:            "127.0.0.1",
		Port:            port,
		Backend:         backend,
		Threads:         spec.Threads,
		LazyLoad:        false,
		MaxLoadedModels: 0,
		UI:              false,
		UIManagement:    false,
		Models: []wireModel{
			{
				ID:                    spec.StreamIDOrDefault(),
				Family:                spec.StreamFamily,
				Path:                  spec.StreamModel,
				Task:                  "asr",
				Mode:                  "streaming",
				DefaultRequestOptions: streamOptions,
			},
			{
				ID:                    spec.RepairIDOrDefault(),
				Family:                spec.RepairFamily,
				Path:                  spec.RepairModel,
				Task:                  "asr",
				Mode:                  "offline",
				DefaultRequestOptions: spec.RequestOptions,
			},
		},
	}
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o644)
}

// Probe reports whether a server answers at the address.
func Probe(ctx context.Context, base string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/health", nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s/health: %s", base, response.Status)
	}
	return nil
}

// ParseURL splits the configured server address into host and port.
func ParseURL(raw string) (host string, port int, err error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", 0, fmt.Errorf("asr_server_url %q: %v (needs a scheme and a host, e.g. http://127.0.0.1:18080)", raw, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", 0, fmt.Errorf("asr_server_url %q: needs a scheme and a host, e.g. http://127.0.0.1:18080", raw)
	}
	host = parsed.Hostname()
	if host == "" {
		return "", 0, fmt.Errorf("asr_server_url %q: no host", raw)
	}
	if parsed.Port() == "" {
		if parsed.Scheme == "https" {
			return host, 443, nil
		}
		return host, 80, nil
	}
	port, err = net.LookupPort("tcp", parsed.Port())
	if err != nil {
		return "", 0, fmt.Errorf("asr_server_url %q: %v", raw, err)
	}
	return host, port, nil
}

// IsLocalHost reports whether an address is this machine, which decides whether
// voiceime may offer to run the server for it.
func IsLocalHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	if parsed := net.ParseIP(host); parsed != nil {
		return parsed.IsLoopback()
	}
	return false
}

func trimFloat(value float64) string {
	text := strconv.FormatFloat(value, 'f', 3, 64)
	return strings.TrimRight(strings.TrimRight(text, "0"), ".")
}

// wireConfig is the server's config document.
type wireConfig struct {
	Host            string      `json:"host"`
	Port            int         `json:"port"`
	Backend         string      `json:"backend"`
	Threads         int         `json:"threads"`
	LazyLoad        bool        `json:"lazy_load"`
	MaxLoadedModels int         `json:"max_loaded_models"`
	UI              bool        `json:"ui"`
	UIManagement    bool        `json:"ui_management"`
	Models          []wireModel `json:"models"`
}

type wireModel struct {
	ID                    string            `json:"id"`
	Family                string            `json:"family"`
	Path                  string            `json:"path"`
	Task                  string            `json:"task"`
	Mode                  string            `json:"mode"`
	DefaultRequestOptions map[string]string `json:"default_request_options,omitempty"`
}
