package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These call the real entry point with a temporary config file. The command

// runForTest runs the CLI as a caller would and returns what it wrote.
func runForTest(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, err bytes.Buffer
	code = Run(args, &out, &err)
	return code, out.String(), err.String()
}

// freshConfig writes a config that is usable - an endpoint, and the model to ask it for - so
// that a test fails for the reason it is about rather than for an unrelated missing setting.
func freshConfig(t *testing.T) string {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.toml")
	body := fmt.Sprintf("asr_server_url = %q\nmodel = %q\n",
		"http://127.0.0.1:18080", "sensevoice-small-q8-audiocpp-v1")
	if err := os.WriteFile(configPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return configPath
}

// bareConfig writes a config with nothing in it: the shape of a fresh install, before anyone
// has said where recognition comes from.
func bareConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func decodeOne(t *testing.T, text string) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(text))
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		t.Fatalf("output is not one JSON object: %v\n%s", err, text)
	}
	if decoder.More() {
		t.Errorf("output holds more than one JSON document:\n%s", text)
	}
	return object
}

// A successful --json invocation is exactly one object on stdout and nothing on
// stderr, so a caller can parse it without looking anywhere else.
func TestJsonSuccessIsOneObjectOnStdout(t *testing.T) {
	configPath := freshConfig(t)
	code, stdout, stderr := runForTest(t, "capabilities", "--json", "--config", configPath)
	if code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, stderr)
	}
	payload := decodeOne(t, stdout)
	if payload["ok"] != true {
		t.Errorf("ok = %v, want true", payload["ok"])
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty on success", stderr)
	}
}

// A failure is one object on stderr carrying an exit code and a fix, which is what
// lets an agent repair a machine without parsing prose.
func TestJsonFailureCarriesExitCodeAndFix(t *testing.T) {
	configPath := bareConfig(t)
	appendToFile(t, configPath, "text_script = \"traditional\"\n")
	code, stdout, stderr := runForTest(t, "apply", "--json", "--config", configPath)
	if code == ExitOK {
		t.Fatal("a configuration no engine can use was reported as working")
	}
	payload := decodeOne(t, stderr)
	if payload["ok"] != false {
		t.Errorf("ok = %v, want false", payload["ok"])
	}
	if int(payload["exit_code"].(float64)) != code {
		t.Errorf("exit_code = %v, want %d", payload["exit_code"], code)
	}
	problem := payload["error"].(map[string]any)
	if problem["fix"] == "" {
		t.Error("a precondition failure carried no fix command")
	}
	if stdout != "" {
		t.Errorf("a failed run also wrote data to stdout:\n%s", stdout)
	}
}

func TestUnknownCommandIsAUsageError(t *testing.T) {
	code, _, stderr := runForTest(t, "definitely-not-a-command")
	if code != ExitUsage {
		t.Fatalf("exit %d, want %d (usage)", code, ExitUsage)
	}
	if !strings.Contains(stderr, "unknown command") {
		t.Errorf("stderr = %q, want it to name the problem", stderr)
	}
}

// `config set` writes the value and then fails if the file it wrote cannot be used.
func TestConfigSetFailsWhenTheValueBreaksTheConfig(t *testing.T) {
	configPath := freshConfig(t)

	code, stdout, stderr := runForTest(t, "config", "set", "max_utterance_seconds", "0", "--json", "--config", configPath)
	if code != ExitPrecondition {
		t.Fatalf("exit %d, want %d (precondition) for a value that makes the config unusable", code, ExitPrecondition)
	}

	// The report is still one object on stdout, saying the value was written.
	payload := decodeOne(t, stdout)["data"].(map[string]any)
	if payload["written"] != true {
		t.Errorf("written = %v, want true: the value is staged on purpose", payload["written"])
	}
	if payload["valid"] != false {
		t.Errorf("valid = %v, want false", payload["valid"])
	}

	// And the failure carries the fix, like every other precondition failure.
	failure := decodeOne(t, stderr)["error"].(map[string]any)
	fix, _ := failure["fix"].(string)
	if !strings.Contains(fix, "config set max_utterance_seconds") {
		t.Errorf("fix = %q, want the command that repairs the value", fix)
	}

	// The value really is in the file, so the report and the file agree.
	loaded, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(loaded), "max_utterance_seconds = 0") {
		t.Error("the value the report says was written is not in the file")
	}

	// A usable value is a success, with the command that applies it named.
	code, stdout, stderr = runForTest(t, "config", "set", "max_utterance_seconds", "45", "--json", "--config", configPath)
	if code != ExitOK {
		t.Fatalf("setting a valid value: exit %d, stderr %s", code, stderr)
	}
	if payload := decodeOne(t, stdout)["data"].(map[string]any); payload["valid"] != true {
		t.Errorf("valid = %v for a value that is fine", payload["valid"])
	}
}
