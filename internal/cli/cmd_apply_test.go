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

// A wrong setting has to be named, explained, and given a fix that says what to write and
// where: doctor is the only place findings are reported, so that is where this is checked.
func TestDoctorNamesAWrongSettingWithAFixThatNamesTheFile(t *testing.T) {
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
	if !strings.Contains(detail, "not a model id") {
		t.Errorf("the finding does not say what the key holds instead: %q", detail)
	}
	if !strings.Contains(fix, "write model = ") {
		t.Fatalf("the fix does not say which line to write: %q", fix)
	}
	if !strings.Contains(fix, configPath) {
		t.Fatalf("the fix does not say which file to write it in: %q", fix)
	}
	// Follow the fix and confirm it works, by following it: advice that names the wrong file,
	// or a line that is not the one the finding is about, is the failure mode this project
	// keeps hitting. Empty is the correct value for a local model, which is then whatever the
	// service on this machine serves. Nothing here runs a command to change a setting: the fix
	// named a file and a line, so following it is an edit.
	current, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	corrected := strings.Replace(string(current),
		`model = "/home/u/models/sensevoice.gguf"`, `model = ""`, 1)
	if corrected == string(current) {
		t.Fatal("the example file no longer holds the line the finding is about")
	}
	if err := os.WriteFile(configPath, []byte(corrected), 0o600); err != nil {
		t.Fatal(err)
	}
	_, clearOut, clearErr := runForTest(t, "--config", configPath, "--json", "doctor")
	stillWrong := configFinding(t, clearOut)
	// The other findings may still fail - this file has no address, and that is a
	// different finding with its own fix. What this case is about is that the advice
	// about the model line, followed, leaves no complaint about the model line.
	if stillWrong != "" {
		t.Fatalf("following the documented fix left the settings still wrong: %s (%s)", stillWrong, clearErr)
	}
}

// A credential in a file other people can read is not a broken configuration - everything
// still works - so it is a warning: it costs a finding and stops nothing. Nothing in this
// program writes that file, so nothing here can quietly have made the mode correct.
func TestDoctorWarnsAboutASettingFileOtherPeopleCanRead(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	body := "asr_server_url = \"http://127.0.0.1:18080/v1/audio/transcriptions\"\n" +
		"model = \"sensevoice-small-q8-audiocpp-v1\"\n" +
		"api_key = \"sk-a-token-nobody-else-needs\"\n"
	if err := os.WriteFile(configPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	_, readable, _ := runForTest(t, "--config", configPath, "--json", "doctor")
	status, _, fix := configCheck(t, readable)
	if status != "warn" {
		t.Fatalf("the settings finding is %q, want warn: a credential is readable by others", status)
	}
	if !strings.Contains(fix, "chmod 600 "+configPath) {
		t.Errorf("fix = %q, want the command that tightens this file", fix)
	}

	// Follow it, and the finding goes away - and the count of prerequisites missing never
	// moved, because a warning is not one of those.
	before := reportedFailures(t, readable)
	if err := os.Chmod(configPath, 0o600); err != nil {
		t.Fatal(err)
	}
	_, tightened, _ := runForTest(t, "--config", configPath, "--json", "doctor")
	if status, _, _ := configCheck(t, tightened); status != "ok" {
		t.Errorf("after chmod 600 the settings finding is %q, want ok", status)
	}
	if after := reportedFailures(t, tightened); after != before {
		t.Errorf("prerequisites missing moved from %v to %v: a warning is not a missing prerequisite", before, after)
	}
}

// reportedFailures is the count doctor reports alongside its findings, which is the number
// that decides its exit code.
func reportedFailures(t *testing.T, report string) float64 {
	data, _ := decodeOne(t, report)["data"].(map[string]any)
	count, _ := data["failures"].(float64)
	return count
}

// configFinding is the detail of the settings finding, or "" when the settings are accepted.
func configFinding(t *testing.T, report string) string {
	_, detail, _ := configCheck(t, report)
	return detail
}

// configCheck reads the settings finding out of a doctor report. Every finding carries an id
// so that a caller can ask for one by name; reading the report as prose would test the
// wording instead of the finding.
func configCheck(t *testing.T, report string) (status, detail, fix string) {
	data, _ := decodeOne(t, report)["data"].(map[string]any)
	checks, _ := data["checks"].([]any)
	for _, raw := range checks {
		check, _ := raw.(map[string]any)
		if check["id"] != "config" {
			continue
		}
		status, _ = check["status"].(string)
		if status == "ok" {
			return status, "", ""
		}
		detail, _ = check["detail"].(string)
		fix, _ = check["fix"].(string)
		return status, detail, fix
	}
	return "", "the report holds no settings finding", ""
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
