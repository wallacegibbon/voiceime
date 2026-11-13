// Package server writes the audio.cpp server's config and asks it whether it is there.
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
	"strings"
	"time"
)

const (
	// DefaultHost is where the unit binds.
	DefaultHost = "127.0.0.1"
	// DefaultPort is where the unit listens unless told otherwise.
	DefaultPort = 18080
)

// Spec is what has to be decided before a server config can be written.
type Spec struct {
	// Model is the GGUF the server serves, and Family is what audio.cpp reports for it.
	Model  string
	Family string
	Port   int
	// Backend and Threads are passed through to audio.cpp.
	Backend string
	Threads int
	// RequestOptions are the options every transcription request carries. Multipart
	// uploads have no field for them, so they belong to the served entry.
	RequestOptions map[string]string
}

// BaseURL is where a client reaches a server with this spec.
func (s Spec) BaseURL() string {
	port := s.Port
	if port == 0 {
		port = DefaultPort
	}
	return fmt.Sprintf("http://%s:%d", DefaultHost, port)
}

// EntryID is the id the served entry is published under: the model file's own name.
func EntryID(modelPath string) string {
	name := strings.TrimSuffix(filepath.Base(modelPath), filepath.Ext(modelPath))
	if name == "" || name == "." || name == "/" {
		return "model"
	}
	return name
}

// WriteConfig writes the server's config and returns its path.
func WriteConfig(spec Spec, path string) error {
	switch {
	case spec.Model == "":
		return fmt.Errorf("no model to serve")
	case spec.Family == "":
		return fmt.Errorf("%s: audio.cpp did not report a family", spec.Model)
	}
	backend := spec.Backend
	if backend == "" {
		backend = "cpu"
	}
	port := spec.Port
	if port == 0 {
		port = DefaultPort
	}
	document := wireConfig{
		Host:            DefaultHost,
		Port:            port,
		Backend:         backend,
		Threads:         spec.Threads,
		LazyLoad:        false,
		MaxLoadedModels: 0,
		UI:              false,
		UIManagement:    false,
		Models: []wireModel{{
			ID:                    EntryID(spec.Model),
			Family:                spec.Family,
			Path:                  spec.Model,
			Task:                  "asr",
			Mode:                  "offline",
			DefaultRequestOptions: spec.RequestOptions,
		}},
	}
	encoded, err := json.MarshalIndent(document, "", "  ")
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

// Entry is one model the server is serving, as /v1/models reports it.
type Entry struct {
	ID     string `json:"id"`
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	Family string `json:"family"`
	Loaded bool   `json:"loaded"`
}

// Entries asks the server what it serves.
func Entries(ctx context.Context, base string) ([]Entry, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/v1/models", nil)
	if err != nil {
		return nil, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s/v1/models: %s", base, response.Status)
	}
	var listing struct {
		Data []Entry `json:"data"`
	}
	if err := json.Unmarshal(payload, &listing); err != nil {
		return nil, fmt.Errorf("decode %s/v1/models: %w", base, err)
	}
	return listing.Data, nil
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

// IsLocalHost reports whether an address is this machine.
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
