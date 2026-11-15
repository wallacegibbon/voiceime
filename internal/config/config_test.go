package config

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// checkpointFile is the file a named checkpoint has to be: it exists. Validate does not read
// what is inside it - only the commands that serve one do.
func checkpointFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "model.gguf")
	if err := os.WriteFile(path, []byte("GGUF"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// usableConfig is a configuration that would pass: every default, plus the one thing no
// default can know - where the user put the checkpoint.
func usableConfig(t *testing.T) *Config {
	cfg := Default()
	cfg.Model = checkpointFile(t)
	return cfg
}

// The shipped defaults do not contradict each other. What a fresh one is missing is a
// checkpoint, and that is said once, with the command that fixes it.
func TestDefaultsAreCoherent(t *testing.T) {
	cfg := Default()
	issues := Errors(cfg.Validate())
	if len(issues) != 1 || issues[0].Key != "model" {
		t.Fatalf("default config = %+v, want only the missing checkpoint", issues)
	}
	if issues[0].Fix == "" {
		t.Error("the missing checkpoint carries no fix, so an agent cannot act on it")
	}
	cfg.Model = checkpointFile(t)
	if issues := Errors(cfg.Validate()); len(issues) != 0 {
		t.Errorf("naming a checkpoint left issues: %+v", issues)
	}
}

func TestValidateReportsAnUnusablePushToTalkTrigger(t *testing.T) {
	cfg := usableConfig(t)
	cfg.PushToTalk = ""
	issues := Errors(cfg.Validate())
	if len(issues) != 1 || issues[0].Key != "push_to_talk" {
		t.Fatalf("Validate with no trigger = %+v, want one push_to_talk issue", issues)
	}
	if issues[0].Fix == "" {
		t.Error("the issue carries no fix, so an agent cannot act on it")
	}
}

func TestSetConvertsTypes(t *testing.T) {
	cfg := Default()
	cases := map[string]string{
		"threads":               "8",
		"max_utterance_seconds": "30",
		"language":              "en",
	}
	for key, value := range cases {
		if err := cfg.Set(key, value); err != nil {
			t.Fatalf("Set(%q, %q): %v", key, value, err)
		}
	}
	if cfg.Threads != 8 || cfg.MaxUtteranceSeconds != 30 || cfg.Language != "en" {
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
	if loaded.Backend != original.Backend || loaded.PushToTalk != original.PushToTalk {
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

// A configured model path that is not there is the failure this check exists for:
func TestValidateReportsAModelPathThatDoesNotExist(t *testing.T) {
	cfg := Default()
	missing := filepath.Join(t.TempDir(), "absent.gguf")
	cfg.Model = missing
	issues := Errors(cfg.Validate())
	if len(issues) != 1 || issues[0].Key != "model" || issues[0].Fix == "" {
		t.Fatalf("Validate with a missing model = %+v, want one blocking issue carrying a fix", issues)
	}
	if !strings.Contains(issues[0].Message, missing) {
		t.Errorf("message %q does not name the path that is wrong", issues[0].Message)
	}
}

// The same check must not fire for a path that exists, or a working install would
// be reported as broken and `voiceime apply` would refuse to run.
func TestValidateAcceptsAnExistingModelPath(t *testing.T) {
	existing := filepath.Join(t.TempDir(), "model.gguf")
	if err := os.WriteFile(existing, []byte("gguf"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Default()
	cfg.Model = existing
	if issues := Errors(cfg.Validate()); len(issues) != 0 {
		t.Fatalf("existing model paths were rejected: %+v", issues)
	}
}

// Writing the configuration must survive concurrent writers.
func TestSaveIsSafeForConcurrentWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	const writers = 8

	var wait sync.WaitGroup
	failures := make(chan error, writers)
	for index := 0; index < writers; index++ {
		wait.Add(1)
		go func(threads int) {
			defer wait.Done()
			cfg := Default()
			cfg.Threads = threads
			if err := Save(cfg, path); err != nil {
				failures <- err
			}
		}(index + 1)
	}
	wait.Wait()
	close(failures)
	for err := range failures {
		t.Errorf("concurrent Save: %v", err)
	}

	// Whatever the interleaving, the file is one whole document: one of the writers
	// won, and nothing left a half-written file behind.
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load after concurrent writes: %v", err)
	}
	if loaded.Threads < 1 || loaded.Threads > writers {
		t.Errorf("threads = %d after concurrent writes, want one of the values written", loaded.Threads)
	}
	// And no temporary files are left in the directory.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Errorf("left %s behind", entry.Name())
		}
	}
}

func TestSetRequestOptionsAsKeyValue(t *testing.T) {
	cfg := Default()

	if err := cfg.Set("request_options", "audio_chunk_mode=none"); err != nil {
		t.Fatalf("Set(request_options, key=value): %v", err)
	}
	if got := cfg.RequestOptions["audio_chunk_mode"]; got != "none" {
		t.Fatalf("request_options[audio_chunk_mode] = %q, want none", got)
	}

	// Overwriting keeps one entry, not two.
	if err := cfg.Set("request_options", "audio_chunk_mode=fixed"); err != nil {
		t.Fatal(err)
	}
	if len(cfg.RequestOptions) != 1 || cfg.RequestOptions["audio_chunk_mode"] != "fixed" {
		t.Fatalf("request_options = %v, want the single overwritten entry", cfg.RequestOptions)
	}

	// An empty value removes the key: without that, a value set once could never be
	// taken out again by the command-line surface.
	if err := cfg.Set("request_options", "audio_chunk_mode="); err != nil {
		t.Fatalf("Set with an empty value: %v", err)
	}
	if _, found := cfg.RequestOptions["audio_chunk_mode"]; found {
		t.Errorf("request_options = %v, want the key removed", cfg.RequestOptions)
	}

	// A malformed value is a usage error that says what the form is.
	if err := cfg.Set("request_options", "no-equals-sign"); err == nil {
		t.Error("Set accepted a value that is not key=value")
	} else if !strings.Contains(err.Error(), "key=value") {
		t.Errorf("error %q does not say what the form is", err)
	}
	if err := cfg.Set("request_options", "=value"); err == nil {
		t.Error("Set accepted an empty key")
	}

	// The round trip through the file keeps it, because it reaches the server config.
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := cfg.Set("request_options", "audio_chunk_mode=none"); err != nil {
		t.Fatal(err)
	}
	if err := Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.RequestOptions["audio_chunk_mode"]; got != "none" {
		t.Errorf("after a save and load: request_options = %v", loaded.RequestOptions)
	}
}

// The streaming client speaks HTTP over a raw socket, so it cannot do TLS.
func TestValidateRejectsAServerAddressTheStreamCannotUse(t *testing.T) {
	cfg := usableConfig(t)
	cfg.ASRServerURL = "https://example.invalid:18080"
	issues := Errors(cfg.Validate())
	if len(issues) != 1 || issues[0].Key != "asr_server_url" {
		t.Fatalf("Validate with an https server = %+v, want one blocking asr_server_url issue", issues)
	}
	if !strings.Contains(issues[0].Message, "https") || !strings.Contains(issues[0].Message, "TLS") {
		t.Errorf("message %q does not explain what is wrong with https", issues[0].Message)
	}
	if issues[0].Fix == "" {
		t.Error("the finding carries no fix")
	}

	// The typo someone will actually make: a bare host and port.
	cfg.ASRServerURL = "127.0.0.1:18080"
	if issues := Errors(cfg.Validate()); len(issues) != 1 ||
		!strings.Contains(issues[0].Message, "scheme and a host") {
		t.Errorf("a bare host:port = %+v, want the shape it should have", issues)
	}

	// A plain local address is fine, with or without a path prefix.
	for _, address := range []string{"http://127.0.0.1:18080", "http://127.0.0.1:18080/", "http://localhost:18080/api"} {
		cfg.ASRServerURL = address
		if issues := Errors(cfg.Validate()); len(issues) != 0 {
			t.Errorf("%s was rejected: %+v", address, issues)
		}
	}

	// An unset address is doctor's finding, not a validation error: a fresh machine has
	// no service yet, and reporting it twice is how one failure becomes two.
	cfg.ASRServerURL = ""
	if issues := Errors(cfg.Validate()); len(issues) != 0 {
		t.Errorf("an unset server address was rejected: %+v", issues)
	}
}

func TestValidateRejectsUnknownTextCleanupValues(t *testing.T) {
	cfg := usableConfig(t)
	cfg.TextScript = "traditional"
	issues := Errors(cfg.Validate())
	if len(issues) != 1 {
		t.Fatalf("Validate = %+v, want one error", issues)
	}
	keys := map[string]bool{}
	for _, issue := range issues {
		keys[issue.Key] = true
		if issue.Fix == "" {
			t.Errorf("%s has no fix command", issue.Key)
		}
	}
	if !keys["text_script"] {
		t.Error("text_script was not reported")
	}
}
