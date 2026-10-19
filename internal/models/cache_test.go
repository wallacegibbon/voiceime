package models

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// fakeCLI writes an executable that records every invocation and answers like audio.cpp's
// `--inspect`. The marker is how a test proves whether anything ran at all.
func fakeCLI(t *testing.T, dir string) (binary, marker string) {
	t.Helper()
	marker = filepath.Join(dir, "invocations")
	binary = filepath.Join(dir, "audiocpp_cli")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + marker + "\n" +
		"echo family=fake_asr\n" +
		"echo \"task=asr modes=offline,streaming\"\n" +
		"echo languages=zh\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binary, marker
}

// modelDir creates a models directory holding one GGUF file.
func modelDir(t *testing.T) (dir, model string) {
	t.Helper()
	dir = t.TempDir()
	model = filepath.Join(dir, "sample.gguf")
	if err := os.WriteFile(model, []byte("gguf"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, model
}

func invocations(t *testing.T, marker string) int {
	t.Helper()
	body, err := os.ReadFile(marker)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return len(strings.Fields(strings.ReplaceAll(string(body), "\n", " ")))
}

// oneInspection is how many arguments `audiocpp_cli --model <path> --inspect` contributes
// to the marker: the flag, the path, and the mode.
const oneInspection = 3

// Cached runs nothing and writes nothing.
//
// It is the function every diagnostic uses, so a `doctor` that cannot write its cache - a
// read-only cache directory, a full disk - has to keep working: measured before this split,
// a read-only cache made doctor report "models: fail" on a machine where every model was
// fine. It also has to work without audio.cpp, because a missing CLI is a finding, not a
// reason to report nothing.
func TestCachedRunsNothingAndWritesNothing(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	dir, model := modelDir(t)
	binary, marker := fakeCLI(t, t.TempDir())

	cfg := config.Default()
	cfg.ModelsDir = dir
	cfg.ASRBin = binary

	cached, err := Cached(cfg)
	if err != nil {
		t.Fatalf("Cached: %v", err)
	}
	if len(cached) != 1 || cached[0].Path != model {
		t.Fatalf("Cached returned %+v, want the one file", cached)
	}
	if cached[0].Inspected {
		t.Error("a file the cache has never seen was reported as inspected")
	}
	if got := invocations(t, marker); got != 0 {
		t.Errorf("Cached ran audio.cpp %d time(s), want none", got)
	}
	if _, err := os.Stat(config.ModelIndexPath()); !os.IsNotExist(err) {
		t.Errorf("Cached wrote the index: %v", err)
	}

	// Without a CLI at all: still reports the files, because "not inspected" is a finding.
	cfg.ASRBin = filepath.Join(t.TempDir(), "absent")
	cached, err = Cached(cfg)
	if err != nil {
		t.Fatalf("Cached without audio.cpp: %v", err)
	}
	if len(cached) != 1 || cached[0].Inspected {
		t.Errorf("Cached without audio.cpp returned %+v, want one uninspected file", cached)
	}
}

// Refresh is the cache's writer: it inspects what it cannot answer, records it, and does
// not ask again until the file changes.
func TestRefreshInspectsOnceAndRemembers(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	dir, model := modelDir(t)
	binary, marker := fakeCLI(t, t.TempDir())

	cfg := config.Default()
	cfg.ModelsDir = dir
	cfg.ASRBin = binary

	refreshed, err := Refresh(cfg, false, nil)
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if len(refreshed) != 1 || !refreshed[0].Inspected || refreshed[0].Family != "fake_asr" {
		t.Fatalf("Refresh returned %+v, want the inspected model", refreshed)
	}
	if got := invocations(t, marker); got != oneInspection {
		t.Errorf("audio.cpp invoked with %d argument(s), want one inspection", got)
	}
	if _, err := os.Stat(config.ModelIndexPath()); err != nil {
		t.Errorf("Refresh did not write the index: %v", err)
	}

	// A second refresh finds the cache current and asks nothing.
	if _, err := Refresh(cfg, false, nil); err != nil {
		t.Fatal(err)
	}
	if got := invocations(t, marker); got != oneInspection {
		t.Errorf("a second refresh ran audio.cpp again (%d arguments)", got)
	}

	// force re-inspects an unchanged file: an audio.cpp upgrade can change the answer.
	if _, err := Refresh(cfg, true, nil); err != nil {
		t.Fatal(err)
	}
	if got := invocations(t, marker); got != 2*oneInspection {
		t.Errorf("--force did not re-inspect (%d arguments total)", got)
	}

	// And a changed file is re-inspected without force.
	if err := os.WriteFile(model, []byte("gguf, replaced"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Refresh(cfg, false, nil); err != nil {
		t.Fatal(err)
	}
	if got := invocations(t, marker); got != 3*oneInspection {
		t.Errorf("a replaced file was not re-inspected (%d arguments total)", got)
	}

	// The cached answer is what Cached reports afterwards.
	after, err := Cached(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !after[0].Inspected || after[0].Family != "fake_asr" {
		t.Errorf("Cached after Refresh = %+v, want the recorded answer", after)
	}
}

// A file that was replaced but not refreshed must not be described with its old answer.
func TestCachedDoesNotServeAStaleAnswer(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	dir, model := modelDir(t)
	binary, _ := fakeCLI(t, t.TempDir())

	cfg := config.Default()
	cfg.ModelsDir = dir
	cfg.ASRBin = binary
	if _, err := Refresh(cfg, false, nil); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(model, []byte("a different checkpoint, same name"), 0o644); err != nil {
		t.Fatal(err)
	}
	cached, err := Cached(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if cached[0].Inspected {
		t.Error("a replaced file was reported as inspected: the old family would be a lie")
	}
	if cached[0].Family != "" {
		t.Errorf("family = %q for a file the cache cannot answer for", cached[0].Family)
	}
}

// A deleted model's record goes with it, or the index would describe files that are gone.
func TestRefreshDropsDeletedModels(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	dir, model := modelDir(t)
	binary, _ := fakeCLI(t, t.TempDir())

	cfg := config.Default()
	cfg.ModelsDir = dir
	cfg.ASRBin = binary
	if _, err := Refresh(cfg, false, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(model); err != nil {
		t.Fatal(err)
	}
	if _, err := Refresh(cfg, false, nil); err != nil {
		t.Fatal(err)
	}

	loaded := loadIndex(config.ModelIndexPath())
	if _, found := loaded.Models[model]; found {
		t.Error("the index still describes a deleted model")
	}
}
