package cli

import (
	"bytes"
	"encoding/json"
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
