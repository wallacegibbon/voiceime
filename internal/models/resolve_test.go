package models

import (
	"path/filepath"
	"testing"
)

// twoModels is a directory with a top-ranked model and a lower-ranked one.
func twoModels() []Model {
	tasks := []Task{{Name: "asr", Modes: []string{"offline", "streaming"}}}
	return []Model{
		{Path: "/models/best.gguf", Family: "qwen3_asr", Tasks: tasks},
		{Path: "/models/other.gguf", Family: "kroko_asr", Tasks: tasks},
	}
}

func TestResolveHonoursTheConfiguredModel(t *testing.T) {
	choice := Resolve(twoModels(), "zh", "/models/other.gguf")
	if choice.Model != "/models/other.gguf" {
		t.Errorf("model = %q, want the configured one", choice.Model)
	}
	if choice.Picked {
		t.Error("Picked = true for a configured model")
	}
}

func TestResolvePicksTheBestInstalledModel(t *testing.T) {
	choice := Resolve(twoModels(), "zh", "")
	if choice.Model != "/models/best.gguf" {
		t.Errorf("model = %q, want the highest-ranked model", choice.Model)
	}
	if !choice.Picked {
		t.Error("Picked = false for a model that came from the ranking")
	}
}

// A model that can read a whole recording offline beats one that only streams, because
// that is what a dictation is.
func TestResolvePrefersAModelThatReadsOffline(t *testing.T) {
	scanned := []Model{
		{Path: "/models/stream-only.gguf", Family: "sense_asr",
			Tasks: []Task{{Name: "asr", Modes: []string{"streaming"}}}},
		{Path: "/models/offline.gguf", Family: "kroko_asr",
			Tasks: []Task{{Name: "asr", Modes: []string{"offline"}}}},
	}
	if got := Resolve(scanned, "en", "").Model; got != "/models/offline.gguf" {
		t.Errorf("model = %q, want the one that reads a whole recording", got)
	}
}

func TestResolveBreaksATieOnLanguage(t *testing.T) {
	tasks := []Task{{Name: "asr", Modes: []string{"offline"}}}
	scanned := []Model{
		{Path: "/models/de.gguf", Family: "kroko_asr", Tasks: tasks, Languages: []string{"de"}},
		{Path: "/models/fr.gguf", Family: "kroko_asr", Tasks: tasks, Languages: []string{"fr"}},
	}
	if got := Resolve(scanned, "fr", "").Model; got != "/models/fr.gguf" {
		t.Errorf("model = %q, want the one that speaks fr", got)
	}
}

func TestResolveWithNothingInstalled(t *testing.T) {
	if choice := Resolve(nil, "zh", ""); choice.Model != "" {
		t.Errorf("resolved %+v with no models installed, want nothing", choice)
	}
	if choice := Resolve(nil, "zh", "/models/only.gguf"); choice.Model != "/models/only.gguf" {
		t.Errorf("resolved %+v, want the configured path", choice)
	}
}

// A leading ~ is a path the user typed, and every command expands it the same way.
func TestResolveExpandsHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	choice := Resolve(nil, "zh", "~/models/x.gguf")
	if want := filepath.Join(home, "models", "x.gguf"); choice.Model != want {
		t.Errorf("model = %q, want %q", choice.Model, want)
	}
}
