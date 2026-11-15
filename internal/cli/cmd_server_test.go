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
// about a directory: the settings name one path, and the answer follows it.
func TestServerSpecServesTheConfiguredModelWithTheFamilyItCarries(t *testing.T) {
	chosen := writeGGUF(t, filepath.Join(t.TempDir(), "chosen.gguf"), "qwen3_asr", []string{"asr"})

	cfg := config.Default()
	cfg.Model = chosen
	spec, err := buildServerSpec(cfg, 18080, "")
	if err != nil {
		t.Fatalf("buildServerSpec: %v", err)
	}
	if spec.Model != chosen {
		t.Errorf("model = %q, want the configured one", spec.Model)
	}
	if spec.Family != "qwen3_asr" {
		t.Errorf("family = %q, want the family the file names", spec.Family)
	}
}

// A flag is a one-off override, and it has to beat the setting it duplicates.
func TestServerSpecPrefersTheFlag(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Model = writeGGUF(t, filepath.Join(dir, "chosen.gguf"), "qwen3_asr", []string{"asr"})

	spec, err := buildServerSpec(cfg, 18080, writeGGUF(t, filepath.Join(dir, "other.gguf"), "sense_asr", []string{"asr"}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(spec.Model, "other.gguf") || spec.Family != "sense_asr" {
		t.Errorf("spec = %+v, want the file named on the command line", spec)
	}
}

// Nothing named: the answer is what to fetch and the command that names it afterwards. voiceime
// fetches nothing and scans nothing, so "install a model" is not a thing to say here.
func TestServerSpecWithoutANamedCheckpointGivesTheAdvice(t *testing.T) {
	_, err := buildServerSpec(config.Default(), 18080, "")
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
	_, err := buildServerSpec(cfg, 18080, "")
	if err == nil {
		t.Fatal("a text file was accepted as a checkpoint")
	}
	if !strings.Contains(err.Error(), "not a GGUF") {
		t.Errorf("error %q does not say the file is not a GGUF", err)
	}
}

// With nothing named, `server run` refuses before it starts anything, and the fix it names is
// the setting, not the command that just failed.
func TestServerRunNamesTheModelSettingWhenNothingIsNamed(t *testing.T) {
	configPath := bareConfig(t)

	code, _, stderr := runForTest(t, "--config", configPath, "server", "run")
	if code != ExitPrecondition {
		t.Fatalf("exit %d, want %d\nstderr: %s", code, ExitPrecondition, stderr)
	}
	if !strings.Contains(stderr, "config set model ") {
		t.Errorf("the fix is not the command that names a checkpoint:\n%s", stderr)
	}
	if strings.Contains(stderr, "voiceime models ") {
		t.Errorf("the fix runs a command that no longer exists:\n%s", stderr)
	}
	// "no model" alone cannot be acted on: the choices differ by 20x on disk.
	if !strings.Contains(stderr, "sensevoice_small_q8") {
		t.Errorf("the failure does not name a checkpoint to fetch:\n%s", stderr)
	}
}

// `voiceime model` is how you check what you named: it reads the file, not the settings.
func TestModelCommandDescribesTheNamedCheckpoint(t *testing.T) {
	configPath, _ := freshConfig(t)
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

// A failed install leaves no unit behind.
func TestServerInstallWritesNoUnitWhenTheSettingsCannotBeSaved(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	binDir := t.TempDir()
	cli := filepath.Join(binDir, "audiocpp_cli")
	for _, name := range []string{cli, filepath.Join(binDir, "audiocpp_server")} {
		if err := os.WriteFile(name, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cfg := config.Default()
	cfg.Model = ggufFixture(t, "sense_asr")
	cfg.ASRBin = cli
	ctx := &Context{
		Config:     cfg,
		ConfigPath: os.DevNull, // readable, never writable
		Stdout:     io.Discard,
		Stderr:     io.Discard,
	}

	err := runServerInstall(ctx, 18080, "")
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
