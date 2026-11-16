package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// checkpointFile is the file a named checkpoint has to be: it exists. Validate does not read
// what is inside it - only the commands that serve one do.
// usableConfig is a configuration that would pass. A test that is about something else still
// has to carry the pair that makes a setup work - an address and the id to ask it for - or it
// spends its assertions on a finding about that pair.
func usableConfig(t *testing.T) *Config {
	cfg := Default()
	cfg.ASRServerURL = "http://127.0.0.1:18080/v1/audio/transcriptions"
	cfg.Model = "sensevoice-small-q8-audiocpp-v1"
	return cfg
}

// The shipped defaults do not contradict each other, and a fresh installation is not described
// as broken: naming no address and no model asks for nothing. The two together are what makes a
// setup work - an address is only usable if something names what to ask it for - so half that
// pair is a blocking finding, and it names the key that completes it.
func TestDefaultsAreCoherent(t *testing.T) {
	issues := Errors(Default().Validate())
	if len(issues) != 0 {
		t.Fatalf("default config = %+v, want nothing blocking", issues)
	}

	cfg := Default()
	cfg.ASRServerURL = "http://127.0.0.1:18080/v1/audio/transcriptions"
	issues = Errors(cfg.Validate())
	if len(issues) == 0 {
		t.Error("an address with nothing to ask it for was accepted: nothing here asks an address what it serves")
	} else if issues[0].Key != "model" {
		t.Errorf("issue %+v does not name the key that completes the pair", issues[0])
	}

	cfg.Model = "sensevoice-small-q8-audiocpp-v1"
	if issues := Errors(cfg.Validate()); len(issues) != 0 {
		t.Errorf("an address and an id to name left issues: %+v", issues)
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

// The settings file can hold a bearer token, so it is not readable by everyone who can read
// this home directory. An older file that was world-readable is tightened by the next write:
// that is the moment its contents are known to have changed, and the tool never writes the
// file without meaning to.
