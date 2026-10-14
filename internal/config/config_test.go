package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultsAreCoherent(t *testing.T) {
	cfg := Default()
	if issues := Errors(cfg.Validate()); len(issues) != 0 {
		t.Fatalf("default config has blocking issues: %+v", issues)
	}
	if cfg.CaptureRate != TargetRate {
		t.Errorf("CaptureRate = %d, want %d: the models' VAD rejects other rates", cfg.CaptureRate, TargetRate)
	}
}

func TestValidateRejectsANonSixteenKilohertzCaptureRate(t *testing.T) {
	cfg := Default()
	cfg.CaptureRate = 48000
	issues := Errors(cfg.Validate())
	if len(issues) == 0 {
		t.Fatal("a 48 kHz capture rate was accepted")
	}
	if issues[0].Key != "capture_rate" || issues[0].Fix == "" {
		t.Errorf("issue = %+v, want a capture_rate issue carrying a fix command", issues[0])
	}
}

func TestValidateWarnsAboutShortWindows(t *testing.T) {
	cfg := Default()
	cfg.ChunkSeconds = 1
	found := false
	for _, issue := range cfg.Validate() {
		if issue.Key == "chunk_seconds" && issue.Severity == "warning" {
			found = true
		}
	}
	if !found {
		t.Error("a 1s window produced no warning, but it measurably corrupts CJK output")
	}
}

func TestSetConvertsTypes(t *testing.T) {
	cfg := Default()
	cases := map[string]string{
		"threads":               "8",
		"chunk_seconds":         "2.5",
		"refine_on_segment_end": "false",
		"language":              "en",
	}
	for key, value := range cases {
		if err := cfg.Set(key, value); err != nil {
			t.Fatalf("Set(%q, %q): %v", key, value, err)
		}
	}
	if cfg.Threads != 8 || cfg.ChunkSeconds != 2.5 || cfg.RefineOnSegmentEnd || cfg.Language != "en" {
		t.Errorf("Set did not apply: %+v", cfg)
	}
	if err := cfg.Set("threads", "many"); err == nil {
		t.Error("Set accepted a non-integer for an int field")
	}
	if err := cfg.Set("nope", "1"); err == nil {
		t.Error("Set accepted an unknown key")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := Default()
	original.Threads = 7
	original.RequestOptions = map[string]string{"audio_chunk_mode": "none"}
	if err := Save(original, path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Threads != 7 || loaded.RequestOptions["audio_chunk_mode"] != "none" {
		t.Errorf("round trip lost settings: %+v", loaded)
	}
	if loaded.ModelsDir != original.ModelsDir || loaded.Hotkey != original.Hotkey {
		t.Errorf("round trip changed defaults: %+v", loaded)
	}
}

func TestLoadRejectsTypos(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := WriteFileForTest(path, "threds = 8\n"); err != nil {
		t.Fatal(err)
	}
	// A silently ignored typo is worse than a failure: the user would believe a
	// setting took effect when it did not.
	if _, err := Load(path); err == nil {
		t.Fatal("Load accepted an unknown key")
	}
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "absent.toml"))
	if err != nil {
		t.Fatalf("Load of a missing file: %v", err)
	}
	if cfg.Threads != Default().Threads {
		t.Errorf("missing file did not yield defaults: %+v", cfg)
	}
}

func TestExpandHome(t *testing.T) {
	home, err := UserHomeDirForTest()
	if err != nil {
		t.Skip("no home directory")
	}
	if got := Expand("~/models"); !strings.HasPrefix(got, home) {
		t.Errorf("Expand(~/models) = %q, want it under %q", got, home)
	}
	if got := Expand("/absolute/path"); got != "/absolute/path" {
		t.Errorf("Expand of an absolute path = %q", got)
	}
}
