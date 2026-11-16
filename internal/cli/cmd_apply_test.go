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

func TestApplyRefusesAConfigThatCannotWork(t *testing.T) {
	configPath := bareConfig(t)
	appendToFile(t, configPath, "text_script = \"traditional\"\n")

	code, stdout, stderr := runForTest(t, "--config", configPath, "apply")
	if code != ExitPrecondition {
		t.Fatalf("apply exited %d, want %d (precondition)\nstdout: %s\nstderr: %s",
			code, ExitPrecondition, stdout, stderr)
	}
	if !strings.Contains(stderr, "text_script") {
		t.Errorf("the error does not name the setting that is wrong:\n%s", stderr)
	}
	if strings.Contains(stdout, "component:") {
		t.Errorf("apply installed half of a configuration it had refused:\n%s", stdout)
	}
}

// A wrong setting has to be named, explained, and given a fix that runs: doctor is the only
// place findings are reported now, so that is where this is checked.
func TestDoctorNamesAWrongSettingWithAFixThatRuns(t *testing.T) {
	configPath := bareConfig(t)
	appendToFile(t, configPath, "model = \"/home/u/models/sensevoice.gguf\"\n")

	code, stdout, _ := runForTest(t, "--config", configPath, "--json", "doctor")
	if code == ExitOK {
		t.Fatalf("doctor reported nothing wrong with a model id that is a path:\n%s", stdout)
	}
	payload := decodeOne(t, stdout)
	data, _ := payload["data"].(map[string]any)
	checks, _ := data["checks"].([]any)
	var detail, fix string
	for _, raw := range checks {
		check, _ := raw.(map[string]any)
		if check["id"] == "config" {
			detail, _ = check["detail"].(string)
			fix, _ = check["fix"].(string)
		}
	}
	if !strings.Contains(detail, "model") {
		t.Errorf("the finding does not name the setting: %q", detail)
	}
	if !strings.Contains(detail, "asr-service.sh") {
		t.Errorf("the finding does not say where a model file belongs instead: %q", detail)
	}
	if !strings.Contains(fix, "config set model") {
		t.Fatalf("the fix does not say how to correct the setting: %q", fix)
	}
	// Run the fix, and confirm the configuration becomes usable: advice that names the
	// wrong command is the failure mode this project keeps hitting. Empty is the correct
	// value for a local model, which is then whatever the service on this machine serves.
	_, clearOut, clearErr := runForTest(t, "--config", configPath, "--json",
		"config", "set", "model", "")
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

// The component file is owned by the install, not by whichever binary happens to run: the
// only part of it that changes with a build is the engine identity.
func TestWithCurrentEngineChangesOnlyTheEngineIdentity(t *testing.T) {
	cfg := config.Default()
	installed := ibus.DefaultComponent(cfg, "/home/u/.local/bin/voiceime ibus-engine --ibus")

	if componentNeedsRewrite([]byte(installed.XML()), withCurrentEngine(installed)) {
		t.Error("a file this version wrote was reported as needing a rewrite")
	}

	// The engine name is not a setting: what is on disk naming a different engine than
	// this build is a file from another build, and that has to be healed.
	stale := installed
	stale.EngineName = "voiceime-next"
	cfg.Language = "en"
	updated := withCurrentEngine(stale)
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

// The unit is what a start at login reads, so it has to name what is on disk. It is written
// by scripts/asr-service.sh and read back here, because a moved checkout or a deleted
// generated config breaks it silently: the service keeps starting and keeps failing. Whether
// this machine wants a unit at all is the endpoint's decision, so every case states one.
func TestUnitCheckReadsTheRecordBack(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	// A built audio.cpp in a checkout: this is what the machine considers its own.
	built := filepath.Join(home, "playground", "audio.cpp", "build", "linux-cpu-release", "bin", modelServiceBinary)
	if err := os.MkdirAll(filepath.Dir(built), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(built, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())

	unitText := func(binary, configFile string) string {
		return "[Unit]\nDescription=x\n\n[Service]\nExecStart=" + binary +
			" --config " + configFile + " --log\n"
	}
	unitFor := func(t *testing.T, binary, configFile string) {
		t.Helper()
		path := unitPath()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(unitText(binary, configFile)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	withEndpoint := func(raw string) *config.Config {
		cfg := config.Default()
		cfg.ASRServerURL = raw
		return cfg
	}
	findings := func(t *testing.T, cfg *config.Config) []Check {
		t.Helper()
		var checks []Check
		unitCheck(func(c Check) { checks = append(checks, c) }, cfg)
		return checks
	}
	oneFinding := func(t *testing.T, cfg *config.Config) Check {
		t.Helper()
		checks := findings(t, cfg)
		if len(checks) != 1 {
			t.Fatalf("%d findings, want one", len(checks))
		}
		return checks[0]
	}

	t.Run("the endpoint is this machine and nothing starts a model", func(t *testing.T) {
		check := oneFinding(t, withEndpoint("http://127.0.0.1:18080"))
		if check.Status != "fail" || !strings.Contains(check.Detail, "this machine") {
			t.Errorf("finding = %+v, want a failure naming the endpoint that has nothing behind it", check)
		}
		if !strings.Contains(check.Fix, "asr-service.sh") {
			t.Errorf("fix %q does not name what writes a unit", check.Fix)
		}
	})

	t.Run("the endpoint is elsewhere and there is no unit", func(t *testing.T) {
		if got := findings(t, withEndpoint("https://api.example.com/v1")); len(got) != 0 {
			t.Errorf("findings = %+v, want none: this setup has no model here, by design", got)
		}
	})

	t.Run("the config it names is gone", func(t *testing.T) {
		unitFor(t, built, filepath.Join(home, "asr-server.json"))
		check := oneFinding(t, withEndpoint("http://127.0.0.1:18080"))
		if check.Status != "fail" {
			t.Fatalf("status = %q: the service would fail to start forever, and silently", check.Status)
		}
		if !strings.Contains(check.Detail, "asr-server.json") {
			t.Errorf("detail %q does not name the file that is missing", check.Detail)
		}
	})

	t.Run("what the script wrote", func(t *testing.T) {
		generated := filepath.Join(config.DataDir(), generatedConfigName)
		if err := os.MkdirAll(filepath.Dir(generated), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(generated, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		unitFor(t, built, generated)
		check := oneFinding(t, withEndpoint("http://127.0.0.1:18080"))
		if check.Status != "ok" {
			t.Fatalf("status = %q, detail %q", check.Status, check.Detail)
		}
		if !strings.Contains(check.Detail, built) || !strings.Contains(check.Detail, generated) {
			t.Errorf("detail %q does not say which audio.cpp runs and what it was told to serve", check.Detail)
		}
	})

	t.Run("a model is loaded that nothing posts to", func(t *testing.T) {
		// The unit is valid; the settings moved to a provider. That is a model sitting in
		// memory for nothing, which is worth saying once, with the command that stops it.
		unitFor(t, built, filepath.Join(config.DataDir(), generatedConfigName))
		check := oneFinding(t, withEndpoint("https://api.example.com"))
		if check.Status != "warn" {
			t.Fatalf("status = %q, want a warning about a model nothing uses", check.Status)
		}
		if !strings.Contains(check.Detail, "api.example.com") || !strings.Contains(check.Fix, "disable --now") {
			t.Errorf("finding = %+v, want the endpoint named and the command that stops the unit", check)
		}
	})
}

// An installed file this version would write differently is rewritten once.
func TestComponentNeedsRewriteSeesAnOlderWriter(t *testing.T) {
	cfg := config.Default()
	installed := ibus.DefaultComponent(cfg, "/home/u/.local/bin/voiceime ibus-engine --ibus")
	updated := withCurrentEngine(installed)

	escaped := strings.Replace(installed.XML(), "<symbol>🎤</symbol>",
		"<symbol>&amp;#x1F3A4;</symbol>", 1)
	if !strings.Contains(escaped, "&amp;#x1F3A4;") {
		t.Fatal("the fixture did not produce an escaped symbol")
	}
	if !componentNeedsRewrite([]byte(escaped), updated) {
		t.Error("a file with the escaped symbol was reported as unchanged, so apply cannot heal it")
	}
}
