package cli

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/models"
)

// streamingModels returns two installed checkpoints whose ranking is not the order the
// user chose: sense_asr outranks kroko_asr, so the auto-pick is the first.
func streamingModels() []models.Model {
	tasks := []models.Task{{Name: "asr", Modes: []string{"offline", "streaming"}}}
	return []models.Model{
		{Path: "/models/best.gguf", Inspected: true, Family: "sense_asr", Tasks: tasks},
		{Path: "/models/chosen.gguf", Inspected: true, Family: "kroko_asr", Tasks: tasks},
	}
}

// The service has to serve what the configuration says, not what the ranking would pick.
//
// A user who sets streaming_model to a checkpoint the ranking does not prefer, and leaves
// refine_model empty ("reuses the streaming model"), used to get a service repairing with
// the auto-picked model: the generated spec resolved the two roles independently, so
// `config show` printed "reuses the streaming model: chosen.gguf" over a service
// repairing with best.gguf. Pinned here because the spec is what the unit runs.
func TestServerSpecReusesTheConfiguredStreamingModel(t *testing.T) {
	cfg := config.Default()
	cfg.StreamingModel = "/models/chosen.gguf"
	cfg.RefineModel = ""

	spec, err := buildServerSpec(cfg, streamingModels(), 18080, "", "")
	if err != nil {
		t.Fatalf("buildServerSpec: %v", err)
	}
	if spec.StreamModel != "/models/chosen.gguf" {
		t.Errorf("stream = %q, want the configured model", spec.StreamModel)
	}
	if spec.RepairModel != spec.StreamModel {
		t.Errorf("repair = %q, want the streaming model %q", spec.RepairModel, spec.StreamModel)
	}
	if spec.RepairFamily != "kroko_asr" {
		t.Errorf("repair family = %q, want the family of the model actually served", spec.RepairFamily)
	}
}

// A flag is a one-off override, and it has to beat the setting it duplicates.
func TestServerSpecPrefersTheFlag(t *testing.T) {
	cfg := config.Default()
	cfg.StreamingModel = "/models/chosen.gguf"

	spec, err := buildServerSpec(cfg, streamingModels(), 18080, "/models/best.gguf", "")
	if err != nil {
		t.Fatal(err)
	}
	if spec.StreamModel != "/models/best.gguf" {
		t.Errorf("stream = %q, want the --stream-model flag", spec.StreamModel)
	}
	if spec.RepairModel != "/models/best.gguf" {
		t.Errorf("repair = %q, want the same model: refine_model is empty", spec.RepairModel)
	}
}

// Nothing streaming, but a repair checkpoint named: serve it for both roles rather than
// refuse to start.
func TestServerSpecFallsBackToTheRepairModel(t *testing.T) {
	offlineOnly := []models.Model{{
		Path:      "/models/chosen.gguf",
		Inspected: true,
		Family:    "qwen3_asr",
		Tasks:     []models.Task{{Name: "asr", Modes: []string{"offline"}}},
	}}
	cfg := config.Default()
	cfg.StreamingModel = ""
	cfg.RefineModel = "/models/chosen.gguf"

	spec, err := buildServerSpec(cfg, offlineOnly, 18080, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if spec.StreamModel != "/models/chosen.gguf" || spec.RepairModel != "/models/chosen.gguf" {
		t.Errorf("serving %q and %q, want the repair checkpoint for both", spec.StreamModel, spec.RepairModel)
	}
}

// With nothing installed there is nothing to serve, and the message says so.
func TestServerSpecWithoutModels(t *testing.T) {
	cfg := config.Default()
	if _, err := buildServerSpec(cfg, nil, 18080, "", ""); err == nil {
		t.Fatal("buildServerSpec succeeded with no models installed")
	} else if got := err.Error(); got == "" || filepath.Base(got) == "" {
		t.Errorf("error %q says nothing", got)
	}
}

// "No model to serve" must not be said about models nobody has looked at.
//
// With an empty cache, nothing can be picked because nothing is classified - and the fix is
// the refresh. Measured before this test: `server install` on a cold cache said "install one
// first" while printing `models list --refresh` as the fix, which is two different stories
// in one message.
func TestServerSpecSaysWhenModelsAreUninspected(t *testing.T) {
	cfg := config.Default()
	unclassified := []models.Model{{
		Path:      "/models/sense.gguf",
		Inspected: false,
	}}
	_, err := buildServerSpec(cfg, unclassified, 18080, "", "")
	if err == nil {
		t.Fatal("buildServerSpec succeeded with nothing classified")
	}
	if !strings.Contains(err.Error(), "have not been inspected") {
		t.Errorf("error %q does not say what is missing", err)
	}
	if strings.Contains(err.Error(), "install one first") {
		t.Errorf("error %q tells the user to install a model that is already there", err)
	}
	// With nothing on disk at all, the install advice is the right one.
	if _, err := buildServerSpec(cfg, nil, 18080, "", ""); err == nil ||
		!strings.Contains(err.Error(), "install one first") {
		t.Errorf("error = %v, want the install advice when no file exists", err)
	}
}

// The failure names the command that fixes it, and it is not the one that just failed.
//
// Measured before this test: `voiceime server run` on a cold cache printed "15 model file(s)
// have not been inspected yet; run `voiceime models list --refresh`" and, on the next line,
// `fix: voiceime server install` - the command that had just failed, which would fail the
// same way. Three call sites each guessed a fix for the spec builder's errors; the builder
// owns the reason, so it owns the fix now.
func TestServerRunNamesTheRefreshWhenModelsAreUninspected(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheDir)
	configPath, modelsDir := freshConfig(t)
	if err := os.WriteFile(filepath.Join(modelsDir, "sample.gguf"), []byte("gguf"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runForTest(t, "--config", configPath, "server", "run")
	if code != ExitPrecondition {
		t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", code, ExitPrecondition, stdout, stderr)
	}
	if !strings.Contains(stderr, "have not been inspected") {
		t.Fatalf("the failure does not say what is missing:\n%s", stderr)
	}
	if !strings.Contains(stderr, "fix: voiceime models list --refresh") {
		t.Errorf("the fix is not the command that classifies models:\n%s", stderr)
	}
	if strings.Contains(stderr, "fix: voiceime server install") {
		t.Errorf("the fix re-runs the command that failed:\n%s", stderr)
	}
	// Nothing was inspected and nothing was written: `server run` reads, and refuses.
	if _, err := os.Stat(filepath.Join(cacheDir, "voiceime", "model_index.json")); !os.IsNotExist(err) {
		t.Error("server run wrote the cache")
	}
}

// With nothing installed at all, the fix is the install, and it names a package.
func TestServerRunSaysToInstallWhenNothingIsThere(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	configPath, _ := freshConfig(t)

	code, _, stderr := runForTest(t, "--config", configPath, "server", "run")
	if code != ExitPrecondition {
		t.Fatalf("exit %d, want %d\nstderr: %s", code, ExitPrecondition, stderr)
	}
	if !strings.Contains(stderr, "fix: voiceime models install ") {
		t.Errorf("the fix does not name a package to install:\n%s", stderr)
	}
}

// A failed install leaves no unit behind.
//
// `server install` used to write the unit before it saved the settings, so a configuration
// that cannot be written left a unit that would start a service with no idea which models to
// serve - at the next boot, long after the command had reported the failure. Measured with
// `--config /dev/null`: the unit was replaced, the save failed (a temporary file cannot be
// created in /dev), and the unit stayed.
func TestServerInstallWritesNoUnitWhenTheSettingsCannotBeSaved(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	// A models directory with one file the cache already answers for, and a binary pair the
	// service lookup can find, so the install gets as far as saving the settings.
	modelsDir := t.TempDir()
	modelFile := filepath.Join(modelsDir, "sample.gguf")
	if err := os.WriteFile(modelFile, []byte("gguf"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(modelFile)
	if err != nil {
		t.Fatal(err)
	}
	cacheDir := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "voiceime")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	index := map[string]any{
		"version": 1,
		"models": map[string]any{
			modelFile: map[string]any{
				"size_bytes": info.Size(), "modified_unix": info.ModTime().Unix(),
				"family": "fake_asr", "tasks": []map[string]any{{"name": "asr", "modes": []string{"streaming", "offline"}}},
				"languages": []string{"zh"},
			},
		},
	}
	body, _ := json.Marshal(index)
	if err := os.WriteFile(filepath.Join(cacheDir, "model_index.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	cli := filepath.Join(binDir, "audiocpp_cli")
	for _, name := range []string{cli, filepath.Join(binDir, "audiocpp_server")} {
		if err := os.WriteFile(name, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cfg := config.Default()
	cfg.ModelsDir = modelsDir
	cfg.ASRBin = cli
	ctx := &Context{
		Config:     cfg,
		ConfigPath: os.DevNull, // readable, never writable
		Stdout:     io.Discard,
		Stderr:     io.Discard,
	}

	err = runServerInstall(ctx, 18080, "", "")
	if err == nil {
		t.Fatal("installing with an unwritable configuration reported success")
	}
	if !strings.Contains(err.Error(), os.DevNull) {
		t.Errorf("error %q does not name the configuration it could not write", err)
	}
	unit := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "systemd", "user", serverUnitName)
	if _, statErr := os.Stat(unit); !os.IsNotExist(statErr) {
		t.Errorf("a unit was written at %s even though the install failed", unit)
	}
}
