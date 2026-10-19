package cli

import (
	"path/filepath"
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
