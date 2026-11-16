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
// usableConfig is a configuration that would pass. Nothing has to be added to the defaults
// for that: the defaults name no endpoint and no model, and both are things a person says
// about their machine rather than things that can be wrong on their own.
func usableConfig(t *testing.T) *Config {
	return Default()
}

// The shipped defaults do not contradict each other, and a fresh installation is not
// described as broken: naming no endpoint and no model is a configuration that asks for
// nothing, because those two keys are what the person adds about their machine. `voiceime
// doctor`'s asr_endpoint finding is what reports them, because it is the check that knows
// whether the answer is a local model or a provider.
func TestDefaultsAreCoherent(t *testing.T) {
	issues := Errors(Default().Validate())
	if len(issues) != 0 {
		t.Fatalf("default config = %+v, want nothing blocking", issues)
	}
	cfg := Default()
	cfg.ASRServerURL = "http://127.0.0.1:18080"
	if issues := Errors(cfg.Validate()); len(issues) != 0 {
		t.Errorf("naming an endpoint left issues: %+v", issues)
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
		"asr_server_timeout_ms": "8000",
		"max_utterance_seconds": "30",
		"language":              "en",
	}
	for key, value := range cases {
		if err := cfg.Set(key, value); err != nil {
			t.Fatalf("Set(%q, %q): %v", key, value, err)
		}
	}
	if cfg.ASRServerTimeoutMS != 8000 || cfg.MaxUtteranceSeconds != 30 || cfg.Language != "en" {
		t.Errorf("Set did not apply: %+v", cfg)
	}
	if err := cfg.Set("asr_server_timeout_ms", "many"); err == nil {
		t.Error("Set accepted a non-integer for an int field")
	}
	if err := cfg.Set("nope", "1"); err == nil {
		t.Error("Set accepted an unknown key")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	original := Default()
	original.Model = "/home/u/models/m.gguf"
	original.MaxUtteranceSeconds = 17
	original.ASRServerTimeoutMS = 9000
	if err := Save(original, path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.MaxUtteranceSeconds != 17 || loaded.ASRServerTimeoutMS != 9000 || loaded.Model != original.Model {
		t.Errorf("round trip lost settings: %+v", loaded)
	}
	if loaded.PushToTalk != original.PushToTalk {
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
	if cfg.ASRServerTimeoutMS != Default().ASRServerTimeoutMS {
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
func TestValidateRejectsAModelThatIsAPath(t *testing.T) {
	cfg := Default()
	cfg.Model = filepath.Join(t.TempDir(), "sensevoice.gguf")
	issues := Errors(cfg.Validate())
	if len(issues) != 1 || issues[0].Key != "model" || issues[0].Fix == "" {
		t.Fatalf("Validate with a path as a model id = %+v, want one blocking issue carrying a fix", issues)
	}
	if !strings.Contains(issues[0].Message, "asr-service.sh") {
		t.Errorf("message %q does not say where a model file belongs", issues[0].Message)
	}
}

// An id is not a path, and a remote endpoint is not a local one: both are accepted without
// being contacted, because nothing here can tell whether a provider's list mentions a model.
func TestValidateAcceptsAModelIdAndARemoteEndpoint(t *testing.T) {
	for _, model := range []string{"gpt-4o-mini-transcribe", "sensevoice-small-q8-audiocpp-v1"} {
		cfg := Default()
		cfg.Model = model
		cfg.ASRServerURL = "https://api.example.com"
		if issues := Errors(cfg.Validate()); len(issues) != 0 {
			t.Errorf("%q over https were rejected: %+v", model, issues)
		}
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
		go func(timeoutMS int) {
			defer wait.Done()
			cfg := Default()
			cfg.ASRServerTimeoutMS = timeoutMS
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
	if loaded.ASRServerTimeoutMS < 1 || loaded.ASRServerTimeoutMS > writers {
		t.Errorf("asr_server_timeout_ms = %d after concurrent writes, want one of the values written",
			loaded.ASRServerTimeoutMS)
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

// The streaming client speaks HTTP over a raw socket, so it cannot do TLS.
// The endpoint is an address, not a process, so any HTTP origin is accepted - including
// https, which is how every provider is reached. What is rejected is a value the client
// cannot use at all.
func TestValidateAcceptsAnyHTTPEndpoint(t *testing.T) {
	cfg := usableConfig(t)

	// The typo someone will actually make: a bare host and port.
	cfg.ASRServerURL = "127.0.0.1:18080"
	issues := Errors(cfg.Validate())
	if len(issues) != 1 || issues[0].Key != "asr_server_url" ||
		!strings.Contains(issues[0].Message, "scheme and a host") {
		t.Fatalf("a bare host:port = %+v, want the shape it should have", issues)
	}
	if issues[0].Fix == "" {
		t.Error("the finding carries no fix")
	}

	for _, address := range []string{
		"http://127.0.0.1:18080", "http://127.0.0.1:18080/", "http://localhost:18080/api",
		"https://api.example.com", "https://example.invalid:18080/v1",
	} {
		cfg.ASRServerURL = address
		if issues := Errors(cfg.Validate()); len(issues) != 0 {
			t.Errorf("%s was rejected: %+v", address, issues)
		}
	}

	for _, address := range []string{"grpc://127.0.0.1:18080", "ws://127.0.0.1:18080"} {
		cfg.ASRServerURL = address
		if issues := Errors(cfg.Validate()); len(issues) != 1 {
			t.Errorf("%s = %+v, want one blocking issue", address, issues)
		}
	}

	// An unset address is doctor's finding, not a validation error: a fresh machine has
	// no endpoint yet, and reporting it twice is how one failure becomes two.
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
