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
// surface is the contract an agent programs against - one JSON object per
// invocation, exit codes that branch without parsing prose - and it had no tests
// at all until one documented flag turned out never to have worked.

// runForTest runs the CLI as a caller would and returns what it wrote.
func runForTest(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, err bytes.Buffer
	code = Run(args, &out, &err)
	return code, out.String(), err.String()
}

// freshConfig writes a config whose models directory is empty, so commands that
// scan for models fail for a reason the test controls.
func freshConfig(t *testing.T) (configPath, modelsDir string) {
	t.Helper()
	dir := t.TempDir()
	configPath = filepath.Join(dir, "config.toml")
	modelsDir = filepath.Join(dir, "models")
	if err := os.MkdirAll(modelsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "models_dir = \"" + modelsDir + "\"\naudio_cpp_dir = \"" + dir + "\"\n"
	if err := os.WriteFile(configPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return configPath, modelsDir
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

// `config init --force` is the documented way to overwrite a config, and it used
// to fail with "flag provided but not defined": the command looked for --force
// among its positional arguments, which the flag parser rejects first.
func TestConfigInitRefusesThenOverwritesWithForce(t *testing.T) {
	configPath, _ := freshConfig(t)
	original, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runForTest(t, "config", "init", "--json", "--config", configPath)
	if code != ExitOK {
		t.Fatalf("init on an existing file: exit %d, stderr %s", code, stderr)
	}
	payload := decodeOne(t, stdout)["data"].(map[string]any)
	if payload["written"] != false {
		t.Errorf("init overwrote an existing file without --force: %v", payload)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(original) {
		t.Error("init changed a file it said it had not written")
	}

	code, stdout, stderr = runForTest(t, "config", "init", "--force", "--json", "--config", configPath)
	if code != ExitOK {
		t.Fatalf("init --force: exit %d, stderr %s", code, stderr)
	}
	payload = decodeOne(t, stdout)["data"].(map[string]any)
	if payload["written"] != true {
		t.Errorf("init --force did not write: %v", payload)
	}
}

// `models list --all` is the documented way to see the files in a shared models
// directory that are not speech models. It failed the same way --force did.
func TestModelsListAcceptsTheAllFlag(t *testing.T) {
	configPath, _ := freshConfig(t)
	code, _, stderr := runForTest(t, "models", "list", "--all", "--config", configPath)
	if code == ExitUsage {
		t.Fatalf("models list --all was rejected as a bad command line: %s", stderr)
	}
	if code != ExitPrecondition {
		t.Errorf("exit %d, want %d (an empty models directory is a missing prerequisite)", code, ExitPrecondition)
	}
}

// A successful --json invocation is exactly one object on stdout and nothing on
// stderr, so a caller can parse it without looking anywhere else.
func TestJsonSuccessIsOneObjectOnStdout(t *testing.T) {
	configPath, _ := freshConfig(t)
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
	configPath, _ := freshConfig(t)
	code, stdout, stderr := runForTest(t, "models", "list", "--json", "--config", configPath)
	if code == ExitOK {
		t.Fatal("listing models in an empty directory succeeded")
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
	// The report itself still goes to stdout: a caller that only wants the data
	// does not have to read the error channel.
	decodeOne(t, stdout)
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
