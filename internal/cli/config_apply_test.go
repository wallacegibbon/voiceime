package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/ibus"
)

// Stopping the engine processes leaves the session with no input method at all, so the
// engine is selected again afterwards - but only when it was ours, or when nothing is
// selected. Another engine is the user's choice.
func TestReselectsEngineOnlyWhenItWasOursOrNone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		current string
		known   bool
		want    bool
	}{
		{"ours is selected", ibus.EngineName, true, true},
		{"nothing is selected", "", true, true},
		{"another engine is selected", "pinyin", true, false},
		{"the daemon could not be asked", "", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := reselectsEngine(tc.current, tc.known); got != tc.want {
				t.Errorf("reselectsEngine(%q, %v) = %v, want %v", tc.current, tc.known, got, tc.want)
			}
		})
	}
}

func TestConfigApplyRefusesAConfigThatNamesAMissingModel(t *testing.T) {
	configPath := bareConfig(t)
	missing := filepath.Join(filepath.Dir(configPath), "absent.gguf")
	appendToFile(t, configPath, "model = \""+missing+"\"\n")

	code, stdout, stderr := runForTest(t, "--config", configPath, "config", "apply")
	if code != ExitPrecondition {
		t.Fatalf("apply exited %d, want %d (precondition)\nstdout: %s\nstderr: %s",
			code, ExitPrecondition, stdout, stderr)
	}
	if !strings.Contains(stderr, "model") {
		t.Errorf("the error does not name the setting that is wrong:\n%s", stderr)
	}
	if strings.Contains(stdout, "restarted") {
		t.Errorf("apply reported a restart it never attempted:\n%s", stdout)
	}
}

func TestConfigValidateRejectsAModelPathThatDoesNotExist(t *testing.T) {
	configPath := bareConfig(t)
	missing := filepath.Join(filepath.Dir(configPath), "absent.gguf")
	appendToFile(t, configPath, "model = \""+missing+"\"\n")

	code, stdout, _ := runForTest(t, "--config", configPath, "config", "validate")
	if code != ExitPrecondition {
		t.Fatalf("validate exited %d, want %d (precondition)\n%s", code, ExitPrecondition, stdout)
	}
	if !strings.Contains(stdout, "model") || !strings.Contains(stdout, missing) {
		t.Errorf("the report does not name the setting and the path:\n%s", stdout)
	}
	if !strings.Contains(stdout, "does not exist") {
		t.Errorf("the report does not say what is wrong with the path:\n%s", stdout)
	}
}

// The fix offered for a missing path has to be runnable, which is the failure mode
// this project keeps hitting: advice that cannot be followed.
func TestConfigValidateOffersARunnableFixForAMissingModel(t *testing.T) {
	configPath := bareConfig(t)
	missing := filepath.Join(filepath.Dir(configPath), "absent.gguf")
	appendToFile(t, configPath, "model = \""+missing+"\"\n")

	code, stdout, _ := runForTest(t, "--config", configPath, "--json", "config", "validate")
	if code != ExitPrecondition {
		t.Fatalf("validate exited %d, want %d\n%s", code, ExitPrecondition, stdout)
	}
	payload := decodeOne(t, stdout)
	data, _ := payload["data"].(map[string]any)
	issues, _ := data["issues"].([]any)
	var fix string
	for _, raw := range issues {
		issue, _ := raw.(map[string]any)
		if issue["key"] == "model" {
			fix, _ = issue["fix"].(string)
		}
	}
	if !strings.Contains(fix, "config set model") {
		t.Fatalf("the fix does not say how to correct the setting: %q", fix)
	}
	// Run the fix with a real checkpoint in place of its placeholder, and confirm the
	// configuration becomes usable: advice that names the wrong command is the failure
	// mode this project keeps hitting.
	_, clearOut, clearErr := runForTest(t, "--config", configPath, "--json",
		"config", "set", "model", ggufFixture(t, "sense_asr"))
	if clearErr != "" {
		t.Fatalf("the documented fix failed: %s", clearErr)
	}
	result := decodeOne(t, clearOut)
	data, _ = result["data"].(map[string]any)
	if data["valid"] != true {
		t.Fatalf("the documented fix left the config invalid: %s", clearOut)
	}
}

func appendToFile(t *testing.T, path, text string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

// `config apply` owns the engine language in the installed component.
func TestWithConfiguredEngineAppliesOnlyWhatTheConfigOwns(t *testing.T) {
	cfg := config.Default()
	installed := ibus.DefaultComponent(cfg, "/home/u/.local/bin/voiceime ibus-engine --ibus")

	if componentNeedsRewrite([]byte(installed.XML()), withConfiguredEngine(installed, cfg)) {
		t.Error("a file this version wrote was reported as needing a rewrite")
	}

	// The engine name is not a setting: what is on disk naming a different engine than
	// this build is a file from another build, and that has to be healed.
	stale := installed
	stale.EngineName = "voiceime-next"
	cfg.Language = "en"
	updated := withConfiguredEngine(stale, cfg)
	if !componentNeedsRewrite([]byte(stale.XML()), updated) {
		t.Fatal("a component naming another engine was reported as no change")
	}
	if updated.EngineName != ibus.EngineName || updated.Language != "en" {
		t.Errorf("engine = %q, language = %q, want %q and the configured language",
			updated.EngineName, updated.Language, ibus.EngineName)
	}
	// The exec line is not a setting: apply must not repoint IBus at whatever binary it
	// ran from.
	if updated.Exec != installed.Exec {
		t.Errorf("exec = %q, want it left alone (%q)", updated.Exec, installed.Exec)
	}
	// And the rewrite has to produce a component that parses back to the same thing, or
	// the next apply would see a permanent difference.
	path := filepath.Join(t.TempDir(), "voiceime.xml")
	if err := os.WriteFile(path, []byte(updated.XML()), 0o644); err != nil {
		t.Fatal(err)
	}
	read, err := ibus.ReadComponent(path)
	if err != nil {
		t.Fatalf("the rewritten component does not parse: %v", err)
	}
	if read != updated {
		t.Errorf("round trip changed the component:\n got %+v\nwant %+v", read, updated)
	}
}

// An installed file this version would write differently is rewritten once.
func TestComponentNeedsRewriteSeesAnOlderWriter(t *testing.T) {
	cfg := config.Default()
	installed := ibus.DefaultComponent(cfg, "/home/u/.local/bin/voiceime ibus-engine --ibus")
	updated := withConfiguredEngine(installed, cfg)

	escaped := strings.Replace(installed.XML(), "<symbol>🎤</symbol>",
		"<symbol>&amp;#x1F3A4;</symbol>", 1)
	if !strings.Contains(escaped, "&amp;#x1F3A4;") {
		t.Fatal("the fixture did not produce an escaped symbol")
	}
	if !componentNeedsRewrite([]byte(escaped), updated) {
		t.Error("a file with the escaped symbol was reported as unchanged, so apply cannot heal it")
	}
}
