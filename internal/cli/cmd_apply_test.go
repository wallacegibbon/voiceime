package cli

import (
	"fmt"
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

func TestApplyRefusesAConfigThatNamesAMissingModel(t *testing.T) {
	configPath := bareConfig(t)
	missing := filepath.Join(filepath.Dir(configPath), "absent.gguf")
	appendToFile(t, configPath, "model = \""+missing+"\"\n")

	code, stdout, stderr := runForTest(t, "--config", configPath, "apply")
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

// A wrong setting has to be named, explained, and given a fix that runs: doctor is the only
// place findings are reported now, so that is where this is checked.
func TestDoctorNamesAWrongSettingWithAFixThatRuns(t *testing.T) {
	configPath := bareConfig(t)
	missing := filepath.Join(filepath.Dir(configPath), "absent.gguf")
	appendToFile(t, configPath, "model = \""+missing+"\"\n")

	code, stdout, _ := runForTest(t, "--config", configPath, "--json", "doctor")
	if code == ExitOK {
		t.Fatalf("doctor reported nothing wrong with a model path that does not exist:\n%s", stdout)
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
	if !strings.Contains(detail, "model") || !strings.Contains(detail, missing) {
		t.Errorf("the finding does not name the setting and the path: %q", detail)
	}
	if !strings.Contains(detail, "does not exist") {
		t.Errorf("the finding does not say what is wrong with the path: %q", detail)
	}
	if !strings.Contains(fix, "config set model") {
		t.Fatalf("the fix does not say how to correct the setting: %q", fix)
	}
	// Run the fix, and confirm the configuration becomes usable: advice that names the
	// wrong command is the failure mode this project keeps hitting.
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

// A machine's own facts are settled once, here, and written where they are used. What a build
// can do comes from the build: an audio.cpp compiled without vulkan cannot do vulkan, so its
// banner is asked rather than assumed. cpu is preferred where a build offers it because cpu is
// the only backend measured for this project (docs/measurements.md); the others stay
// unavailable rather than becoming an unmeasured default.
func TestResolveEngineAsksTheBuildAndTheCPU(t *testing.T) {
	stub := func(t *testing.T, banner string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), config.ASRServerName)
		script := "#!/bin/sh\n"
		if banner != "" {
			script += "printf '" + strings.ReplaceAll(banner, "'", `'\''`) + "'\n"
		}
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		return path
	}

	// The banner is several lines and the backends are the last of them, which is what
	// `supportedBackends` reads.
	for _, tc := range []struct{ name, banner, want string }{
		{"a cpu build", "audio.cpp dev\\nbackends: cpu\\n", "cpu"},
		{"cpu among others", "audio.cpp dev\\nbackends: vulkan, cpu, cuda\\n", "cpu"},
		// A build that cannot do cpu is run on what it can do, rather than refused: the
		// machine was built for that backend.
		{"no cpu in the build", "audio.cpp dev\\nbackends: cuda\\n", "cuda"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend, threads, err := resolveEngine(stub(t, tc.banner))
			if err != nil {
				t.Fatal(err)
			}
			if backend != tc.want {
				t.Errorf("backend = %q, want %q", backend, tc.want)
			}
			// The worker count belongs to the CPU, and one worker per physical core: the
			// streaming decoder and the repair pass share the same cores.
			if want := physicalCores(); threads != want || threads <= 0 {
				t.Errorf("threads = %d, want the machine's %d", threads, want)
			}
		})
	}

	t.Run("a build that says nothing about itself", func(t *testing.T) {
		// No backend named is no answer: running it with a guessed one is how an install
		// ends up with a service that will not start.
		_, _, err := resolveEngine(stub(t, "audio.cpp dev"))
		if err == nil {
			t.Fatal("resolveEngine guessed a backend the build never named")
		}
		if !strings.Contains(fmt.Sprint(err), "backend") {
			t.Errorf("error %q does not say what is missing", err)
		}
	})
}

// The unit is what a start at login reads, so it has to name what is on disk. Two ways that
// stops being true have different findings: the file the unit points at is gone, or the unit
// points at an audio.cpp this machine no longer considers its own.
func TestServerUnitCheckReadsTheRecordBack(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", home)
	// A built audio.cpp in a checkout: this is what the machine considers its own.
	built := filepath.Join(home, "playground", "audio.cpp", "build", "linux-cpu-release", "bin", config.ASRServerName)
	if err := os.MkdirAll(filepath.Dir(built), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(built, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())

	unitFor := func(t *testing.T, binary, configFile string) {
		t.Helper()
		path := serverUnitPath()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(serverUnit(binary, configFile)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	oneFinding := func(t *testing.T) Check {
		t.Helper()
		var checks []Check
		serverUnitCheck(func(c Check) { checks = append(checks, c) })
		if len(checks) != 1 {
			t.Fatalf("%d findings, want one", len(checks))
		}
		return checks[0]
	}

	t.Run("nothing installed", func(t *testing.T) {
		check := oneFinding(t)
		if check.Status != "fail" || !strings.Contains(check.Detail, "no recognition server unit") {
			t.Errorf("finding = %+v, want a failure naming the missing unit", check)
		}
	})

	t.Run("the config it names is gone", func(t *testing.T) {
		unitFor(t, built, filepath.Join(home, "asr-server.json"))
		check := oneFinding(t)
		if check.Status != "fail" {
			t.Fatalf("status = %q: the service would fail to start forever, and silently", check.Status)
		}
		if !strings.Contains(check.Detail, "asr-server.json") {
			t.Errorf("detail %q does not name the file that is missing", check.Detail)
		}
	})

	t.Run("another audio.cpp is running", func(t *testing.T) {
		previous := filepath.Join(t.TempDir(), config.ASRServerName)
		if err := os.WriteFile(previous, []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		generated := filepath.Join(home, "asr-server.json")
		if err := os.WriteFile(generated, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		unitFor(t, previous, generated)
		check := oneFinding(t)
		if check.Status != "warn" {
			t.Fatalf("status = %q, want a warning: the old binary works, so nothing else notices", check.Status)
		}
		if !strings.Contains(check.Detail, previous) || !strings.Contains(check.Detail, built) {
			t.Errorf("detail %q does not name both the running audio.cpp and the one on the machine", check.Detail)
		}
		if !strings.Contains(check.Fix, "voiceime apply") {
			t.Errorf("fix %q does not name the command that rewrites the unit", check.Fix)
		}
	})

	t.Run("what apply wrote", func(t *testing.T) {
		generated := filepath.Join(config.DataDir(), serverConfigName)
		if err := os.MkdirAll(filepath.Dir(generated), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(generated, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		unitFor(t, built, generated)
		check := oneFinding(t)
		if check.Status != "ok" {
			t.Fatalf("status = %q, detail %q", check.Status, check.Detail)
		}
		if !strings.Contains(check.Detail, built) || !strings.Contains(check.Detail, generated) {
			t.Errorf("detail %q does not say which audio.cpp runs and what it was told to serve", check.Detail)
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
