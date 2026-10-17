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

func TestDownloadEndpointAcceptsAnyHuggingFaceProtocolHost(t *testing.T) {
	cases := map[string]string{
		"https://hf-mirror.com":            "https://hf-mirror.com",
		"https://hf-mirror.com/":           "https://hf-mirror.com",
		"http://192.168.1.10:8080":         "http://192.168.1.10:8080",
		"https://huggingface.co":           "https://huggingface.co",
		"  https://mirror.example.com/hf/": "https://mirror.example.com/hf",
	}
	for configured, want := range cases {
		cfg := Default()
		cfg.DownloadMirror = configured
		got, err := cfg.DownloadEndpoint()
		if err != nil {
			t.Errorf("DownloadEndpoint(%q): %v", configured, err)
			continue
		}
		if got != want {
			t.Errorf("DownloadEndpoint(%q) = %q, want %q", configured, got, want)
		}
	}
}

// The former preset names are gone rather than aliased: a value that is not an
// endpoint URL must fail loudly, because silently reading it as "some host we
// know" is the ambiguity this setting was simplified to remove.
func TestDownloadEndpointRejectsTheFormerPresetNames(t *testing.T) {
	for _, configured := range []string{"hf-mirror", "huggingface", "modelscope", ""} {
		cfg := Default()
		cfg.DownloadMirror = configured
		if _, err := cfg.DownloadEndpoint(); err == nil {
			t.Errorf("DownloadEndpoint(%q) was accepted", configured)
		}
		issues := Errors(cfg.Validate())
		if len(issues) != 1 || issues[0].Key != "download_mirror" || issues[0].Fix == "" {
			t.Errorf("Validate with download_mirror=%q = %+v, want one issue carrying a fix", configured, issues)
		}
	}
}

func TestDownloadEndpointRejectsMalformedEndpoints(t *testing.T) {
	for _, configured := range []string{
		"hf-mirror.com",             // no scheme
		"ftp://hf-mirror.com",       // wrong scheme
		"https://",                  // no host
		"https://host/path?token=x", // query string
		"https://host/path#frag",    // fragment
	} {
		cfg := Default()
		cfg.DownloadMirror = configured
		if _, err := cfg.DownloadEndpoint(); err == nil {
			t.Errorf("DownloadEndpoint(%q) was accepted", configured)
		}
	}
}

// An existing config file that still holds a preset name must stay loadable, so
// that the one command that repairs it is runnable. Failing at Load time would
// leave hand-editing as the only way out, which is the state a key rename would
// produce.
func TestLoadToleratesALegacyEndpointValueSoTheFixStaysRunnable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := WriteFileForTest(path, "download_mirror = \"hf-mirror\"\n"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load refused a file with a legacy endpoint, leaving no CLI path to fix it: %v", err)
	}
	if len(Errors(cfg.Validate())) == 0 {
		t.Fatal("the legacy value validated clean, but it is not an endpoint URL")
	}
	if err := cfg.Set("download_mirror", DefaultDownloadEndpoint); err != nil {
		t.Fatalf("the repair command is not runnable: %v", err)
	}
	if len(Errors(cfg.Validate())) != 0 {
		t.Error("the documented fix did not resolve the issue")
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
