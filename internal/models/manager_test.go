package models

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/wallacegibbon/voiceime/internal/config"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	if config.FindExecutable("python3", "python") == "" {
		t.Skip("no python interpreter on PATH")
	}
	script := filepath.Join(t.TempDir(), "model_manager_v2.py")
	if err := os.WriteFile(script, []byte("# stand-in\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.ModelManager = script
	cfg.ModelsDir = t.TempDir()
	return cfg
}

func TestEnvironmentPinsTheConfiguredEndpoint(t *testing.T) {
	manager := &Manager{Endpoint: "https://mirror.example.com"}
	environment := manager.environment()
	if len(environment) != 1 || environment[0] != "HF_ENDPOINT=https://mirror.example.com" {
		t.Fatalf("environment() = %v, want exactly the configured endpoint", environment)
	}
}

func TestNewManagerCarriesTheEndpointAndNormalizesIt(t *testing.T) {
	cfg := testConfig(t)
	cfg.DownloadMirror = "https://hf-mirror.com/"
	manager, err := NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if manager.Endpoint != "https://hf-mirror.com" {
		t.Errorf("Endpoint = %q, want the trailing slash dropped", manager.Endpoint)
	}
	if manager.ModelsDir != cfg.ResolvedModelsDir() {
		t.Errorf("ModelsDir = %q, want %q", manager.ModelsDir, cfg.ResolvedModelsDir())
	}
}

func TestNewManagerReportsAnUnusableEndpointAsItsOwnError(t *testing.T) {
	cfg := testConfig(t)
	cfg.DownloadMirror = "hf-mirror"
	_, err := NewManager(cfg)
	if err == nil {
		t.Fatal("NewManager accepted a value that is not an endpoint URL")
	}
	if !errors.Is(err, ErrBadEndpoint) {
		t.Errorf("error = %v, want it to wrap ErrBadEndpoint", err)
	}
}
