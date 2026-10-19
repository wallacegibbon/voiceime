package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// fakeInspect writes an executable that records its invocations and answers like
// `audiocpp_cli --inspect`. The marker is how these tests prove whether it ran.
func fakeInspect(t *testing.T, marker string) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "audiocpp_cli")
	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + marker + "\n" +
		"echo family=fake_asr\n" +
		"echo \"task=asr modes=offline,streaming\"\n" +
		"echo languages=zh\n"
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binary
}

// modelsListFixture prepares a config whose models directory holds one unclassified file
// and whose audio.cpp is a recorder.
func modelsListFixture(t *testing.T) (configPath, cacheDir, modelFile, marker string) {
	t.Helper()
	cacheDir = t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheDir)
	configPath, modelsDir := freshConfig(t)
	modelFile = filepath.Join(modelsDir, "sample.gguf")
	if err := os.WriteFile(modelFile, []byte("gguf"), 0o644); err != nil {
		t.Fatal(err)
	}
	marker = filepath.Join(t.TempDir(), "calls")
	appendToFile(t, configPath, "asr_bin = \""+fakeInspect(t, marker)+"\"\n")
	return configPath, cacheDir, modelFile, marker
}

// A plain `models list` neither asks audio.cpp nor writes the cache.
//
// It says what it cannot tell instead: "1 file has not been inspected", with the command
// that would classify it. That is the difference this split exists for - before it, the
// list silently spent six seconds per file, and a read-only cache made the same command
// fail and tell the user to run `voiceime models refresh`, a command that no longer exists.
func TestModelsListDoesNotInspectOrWrite(t *testing.T) {
	configPath, cacheDir, _, marker := modelsListFixture(t)

	code, stdout, stderr := runForTest(t, "--config", configPath, "--json", "models", "list")
	if code != ExitPrecondition {
		t.Fatalf("exit %d, want %d: nothing is classified yet\nstdout: %s\nstderr: %s",
			code, ExitPrecondition, stdout, stderr)
	}
	payload := decodeOne(t, stdout)["data"].(map[string]any)
	if payload["uninspected"] != float64(1) {
		t.Errorf("uninspected = %v, want 1", payload["uninspected"])
	}
	// The fix is the command that exists, and it is the only writer.
	if !strings.Contains(stderr, "models list --refresh") {
		t.Errorf("the failure does not name the command that classifies a model:\n%s", stderr)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Error("a plain list ran audio.cpp")
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "voiceime", "model_index.json")); !os.IsNotExist(err) {
		t.Error("a plain list wrote the cache")
	}
}

// `models list --refresh` is the only command that writes it, and afterwards a plain list
// is a cache read.
func TestModelsListRefreshIsTheOnlyWriter(t *testing.T) {
	configPath, cacheDir, modelFile, marker := modelsListFixture(t)

	code, stdout, stderr := runForTest(t, "--config", configPath, "--json", "models", "list", "--refresh")
	if code != ExitOK {
		t.Fatalf("refresh exited %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("--refresh did not run audio.cpp")
	}
	indexPath := filepath.Join(cacheDir, "voiceime", "model_index.json")
	if _, err := os.Stat(indexPath); err != nil {
		t.Fatalf("--refresh did not write the cache: %v", err)
	}
	models := decodeOne(t, stdout)["data"].(map[string]any)["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("models = %v, want the one file", models)
	}
	entry := models[0].(map[string]any)
	if entry["inspected"] != true || entry["family"] != "fake_asr" {
		t.Errorf("entry = %v, want it inspected and classified", entry)
	}

	// A plain list now answers from the cache, still without running anything.
	before := fileSize(t, marker)
	code, stdout, _ = runForTest(t, "--config", configPath, "--json", "models", "list")
	if code != ExitOK {
		t.Fatalf("list after refresh exited %d", code)
	}
	if after := fileSize(t, marker); after != before {
		t.Errorf("a plain list ran audio.cpp again (%d -> %d bytes of invocations)", before, after)
	}
	models = decodeOne(t, stdout)["data"].(map[string]any)["models"].([]any)
	if len(models) != 1 || models[0].(map[string]any)["path"] != modelFile {
		t.Errorf("cached list = %v, want the one file", models)
	}
}

// `--force` is meaningless without `--refresh`, and guessing which one a user meant is how
// a flag comes to do nothing.
func TestModelsListRejectsForceWithoutRefresh(t *testing.T) {
	configPath, _, _, _ := modelsListFixture(t)
	code, _, stderr := runForTest(t, "--config", configPath, "models", "list", "--force")
	if code != ExitUsage {
		t.Fatalf("exit %d, want %d (usage)", code, ExitUsage)
	}
	if !strings.Contains(stderr, "--refresh") {
		t.Errorf("the error does not say what --force needs:\n%s", stderr)
	}
}

// doctor reports an unclassified model without classifying it.
//
// It is the command a user runs to find out what is wrong, so it must not spend six seconds
// per file asking audio.cpp, and it must not fail because a cache could not be written -
// both of which it did before the cache had a single writer (D35).
func TestDoctorReportsUninspectedModelsWithoutTouchingTheCache(t *testing.T) {
	cacheDir := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cacheDir)
	_, modelsDir := freshConfig(t)
	if err := os.WriteFile(filepath.Join(modelsDir, "sample.gguf"), []byte("gguf"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Default()
	cfg.ModelsDir = modelsDir
	ctx := &Context{Config: cfg, Stdout: io.Discard, Stderr: io.Discard}

	var checks []Check
	modelChecks(ctx, func(check Check) { checks = append(checks, check) })
	if len(checks) == 0 {
		t.Fatal("doctor reported nothing about an unclassified model")
	}
	first := checks[0]
	if first.Status != "fail" || first.ID != "models" {
		t.Fatalf("first check = %+v, want a failing models finding", first)
	}
	if !strings.Contains(first.Detail, "have not been inspected") {
		t.Errorf("detail %q does not say what is missing", first.Detail)
	}
	if first.Fix != "voiceime models list --refresh" {
		t.Errorf("fix = %q, want the only command that classifies models", first.Fix)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "voiceime", "model_index.json")); !os.IsNotExist(err) {
		t.Error("doctor wrote the cache")
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}
