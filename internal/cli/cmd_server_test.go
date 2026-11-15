package cli

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// ggufFixture writes a checkpoint whose metadata says it belongs to a given audio.cpp family,
// and returns its path. The family is what the generated server config needs, so any test
// that reaches the service has to name a file that carries one.
func ggufFixture(t *testing.T, family string) string {
	t.Helper()
	return writeGGUF(t, filepath.Join(t.TempDir(), family+".gguf"), family, []string{"asr"})
}

// Test that the family comes out of the file rather than out of anything the tool remembers
// about a directory: the settings name one path, and the answer follows it. The backend and
// thread count are arguments because they are what deployment resolved, not settings.
func TestServerSpecServesTheConfiguredModelWithTheFamilyItCarries(t *testing.T) {
	chosen := writeGGUF(t, filepath.Join(t.TempDir(), "chosen.gguf"), "qwen3_asr", []string{"asr"})

	cfg := config.Default()
	cfg.Model = chosen
	spec, err := buildServerSpec(cfg, 18080, "cpu", 8)
	if err != nil {
		t.Fatalf("buildServerSpec: %v", err)
	}
	if spec.Model != chosen {
		t.Errorf("model = %q, want the configured one", spec.Model)
	}
	if spec.Family != "qwen3_asr" {
		t.Errorf("family = %q, want the family the file names", spec.Family)
	}
	if spec.Backend != "cpu" || spec.Threads != 8 {
		t.Errorf("spec = %s/%d, want what deployment resolved", spec.Backend, spec.Threads)
	}
}

// Nothing named: the answer is what to fetch and the command that names it afterwards. voiceime
// fetches nothing and scans nothing, so "install a model" is not a thing to say here.
func TestServerSpecWithoutANamedCheckpointGivesTheAdvice(t *testing.T) {
	_, err := buildServerSpec(config.Default(), 18080, "cpu", 4)
	if err == nil {
		t.Fatal("buildServerSpec succeeded with nothing named")
	}
	// An answer with no name in it cannot be acted on: the choices differ by 20x on disk.
	if !strings.Contains(err.Error(), "sensevoice_small_q8") || !strings.Contains(err.Error(), "MiB") {
		t.Errorf("error %q does not say what to fetch", err)
	}
	if strings.Contains(err.Error(), "models list") || strings.Contains(err.Error(), "models install") {
		t.Errorf("error %q points at commands that no longer exist", err)
	}
}

// A named file that is not an audio.cpp checkpoint is a different failure, and it says so:
// the reader has a file in hand and needs to know which of the two problems they have.
func TestServerSpecRejectsAFileWithoutAModelSpec(t *testing.T) {
	notACheckpoint := filepath.Join(t.TempDir(), "notes.gguf")
	if err := os.WriteFile(notACheckpoint, []byte("not a gguf"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.Model = notACheckpoint
	_, err := buildServerSpec(cfg, 18080, "cpu", 4)
	if err == nil {
		t.Fatal("a text file was accepted as a checkpoint")
	}
	if !strings.Contains(err.Error(), "not a GGUF") {
		t.Errorf("error %q does not say the file is not a GGUF", err)
	}
}

// `voiceime model` is how you check what you named: it reads the file, not the settings.
func TestModelCommandDescribesTheNamedCheckpoint(t *testing.T) {
	configPath := freshConfig(t)
	checkpoint := ggufFixture(t, "sense_asr")
	if _, _, stderr := runForTest(t, "--config", configPath, "config", "set", "model", checkpoint); stderr != "" {
		t.Fatalf("config set model: %s", stderr)
	}

	code, stdout, stderr := runForTest(t, "--config", configPath, "model")
	if code != ExitOK {
		t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", code, ExitOK, stdout, stderr)
	}
	for _, want := range []string{checkpoint, "sense_asr", "asr"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("output %q does not mention %s", stdout, want)
		}
	}

	// With nothing named it is a missing prerequisite, with the advice.
	emptyPath := bareConfig(t)
	if code, _, stderr := runForTest(t, "--config", emptyPath, "model"); code != ExitPrecondition {
		t.Errorf("exit %d with nothing named, want %d: %s", code, ExitPrecondition, stderr)
	}
}

// A failed apply leaves no unit behind. The settings are written before the files that
// describe them, so a unit can never name a configuration that never named the service.
func TestApplyLeavesNoUnitWhenTheSettingsCannotBeSaved(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	// audio.cpp on PATH, and a build that says what it was compiled for: everything
	// machine-shaped has to answer before the settings are reached.
	t.Setenv("HOME", t.TempDir())
	binDir := t.TempDir()
	built := filepath.Join(binDir, "audiocpp_server")
	script := "#!/bin/sh\nprintf 'audio.cpp dev\\nbackends: cpu\\n'\n"
	if err := os.WriteFile(built, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	cfg := config.Default()
	cfg.Model = ggufFixture(t, "sense_asr")
	ctx := &Context{
		Config:     cfg,
		ConfigPath: os.DevNull, // readable, never writable
		Stdout:     io.Discard,
		Stderr:     io.Discard,
	}

	report, err := applyService(ctx)
	if err == nil {
		t.Fatal("applying with an unwritable configuration reported success")
	}
	if report.Service != "" {
		t.Errorf("a failed apply reported %q", report.Service)
	}
	if report.AudioCPP != built {
		t.Errorf("the report does not say which audio.cpp it had resolved: %q", report.AudioCPP)
	}
	if !strings.Contains(err.Error(), os.DevNull) {
		t.Errorf("error %q does not name the configuration it could not write", err)
	}
	unit := serverUnitPath()
	if _, statErr := os.Stat(unit); !os.IsNotExist(statErr) {
		t.Errorf("a unit was written at %s even though apply failed", unit)
	}
	// The generated audio.cpp config is written with the settings, not after the unit: a
	// unit that names a file this attempt never wrote would be worse than no unit at all.
	if _, statErr := os.Stat(serverConfigFile()); !os.IsNotExist(statErr) {
		t.Errorf("an audio.cpp config was written at %s for settings that were never saved", serverConfigFile())
	}
}
