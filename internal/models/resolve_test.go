package models

import (
	"os"
	"path/filepath"
	"testing"
)

// twoStreamingModels returns a top-ranked model and a lower-ranked one, which is what a
// user's directory looks like once they have installed more than the recommended package.
func twoStreamingModels() []Model {
	tasks := []Task{{Name: "asr", Modes: []string{"offline", "streaming"}}}
	return []Model{
		{Path: "/models/best.gguf", Family: "sense_asr", Tasks: tasks},
		{Path: "/models/chosen.gguf", Family: "kroko_asr", Tasks: tasks},
	}
}

// An empty refine_model reuses the *configured* streaming model, not the auto-picked one.
//
// This is the bug the shared resolution exists to prevent: the generated server config
// resolved repair on its own, so a user who chose a streaming model that was not the
// top-ranked one got a service repairing with a different checkpoint than
// `config show` promised - "empty means reuse the streaming model: chosen.gguf" against a
// service serving best.gguf for repair.
func TestResolveReusesTheConfiguredStreamingModel(t *testing.T) {
	scanned := twoStreamingModels()
	choice := Resolve(scanned, "zh", "/models/chosen.gguf", "")

	if choice.Stream != "/models/chosen.gguf" {
		t.Errorf("stream = %q, want the configured model", choice.Stream)
	}
	if choice.Repair != choice.Stream {
		t.Errorf("repair = %q, want the streaming model %q", choice.Repair, choice.Stream)
	}
	if !choice.RepairReused {
		t.Error("RepairReused = false for an empty refine_model")
	}
	if choice.StreamPicked {
		t.Error("StreamPicked = true for a configured streaming_model")
	}
}

// With nothing configured, the ranking decides both roles - and both get the same model,
// because one resident checkpoint is cheaper than two (D22).
func TestResolvePicksOneModelForBothRoles(t *testing.T) {
	choice := Resolve(twoStreamingModels(), "zh", "", "")
	if choice.Stream != "/models/best.gguf" {
		t.Errorf("stream = %q, want the highest-ranked model", choice.Stream)
	}
	if choice.Repair != choice.Stream {
		t.Errorf("repair = %q, want the same model as streaming", choice.Repair)
	}
	if !choice.StreamPicked || !choice.RepairReused {
		t.Errorf("sources = picked:%v reused:%v, want both", choice.StreamPicked, choice.RepairReused)
	}
}

// An explicit refine_model wins over reuse, even when nothing streams.
func TestResolveHonoursAnExplicitRefineModel(t *testing.T) {
	choice := Resolve(twoStreamingModels(), "en", "", "/models/qwen.gguf")
	if choice.Repair != "/models/qwen.gguf" {
		t.Errorf("repair = %q, want the explicit refine_model", choice.Repair)
	}
	if choice.RepairReused {
		t.Error("RepairReused = true for an explicit refine_model")
	}
	if choice.Stream != "/models/best.gguf" {
		t.Errorf("stream = %q, want the top-ranked model", choice.Stream)
	}
}

// The ranking is family first and language second, which is what makes two checkpoints of
// the same family separable.
func TestResolveBreaksATieOnLanguage(t *testing.T) {
	tasks := []Task{{Name: "asr", Modes: []string{"streaming"}}}
	scanned := []Model{
		{Path: "/models/de.gguf", Family: "kroko_asr", Tasks: tasks, Languages: []string{"de"}},
		{Path: "/models/fr.gguf", Family: "kroko_asr", Tasks: tasks, Languages: []string{"fr"}},
	}
	if got := Resolve(scanned, "fr", "", "").Stream; got != "/models/fr.gguf" {
		t.Errorf("stream = %q, want the model that speaks fr", got)
	}
	if got := Resolve(scanned, "de", "", "").Stream; got != "/models/de.gguf" {
		t.Errorf("stream = %q, want the model that speaks de", got)
	}
}

// Nothing installed is reported as nothing, not as a guess: the caller decides what to
// say about it.
func TestResolveWithNothingInstalled(t *testing.T) {
	choice := Resolve(nil, "zh", "", "")
	if choice.Stream != "" || choice.Repair != "" {
		t.Errorf("resolved %+v with no models installed, want both empty", choice)
	}
	// A refine_model on its own still resolves, so a service can serve one checkpoint for
	// both roles.
	choice = Resolve(nil, "zh", "", "/models/only.gguf")
	if choice.Stream != "" || choice.Repair != "/models/only.gguf" {
		t.Errorf("resolved %+v, want only the repair path", choice)
	}
}

// A leading ~ in a setting is a path the user typed, and every command has to expand it
// the same way.
func TestResolveExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	choice := Resolve(nil, "zh", "~/models/x.gguf", "")
	if want := filepath.Join(home, "models", "x.gguf"); choice.Stream != want {
		t.Errorf("stream = %q, want %q", choice.Stream, want)
	}
	if choice.Repair != choice.Stream {
		t.Errorf("repair = %q, want the expanded streaming path", choice.Repair)
	}
	_ = os.Getenv("HOME")
}
