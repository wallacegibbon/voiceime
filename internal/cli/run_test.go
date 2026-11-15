package cli

import (
	"bytes"
	"encoding/binary"
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

// freshConfig writes a config that is usable - one named checkpoint, present on disk - so a
// test fails for the reason it is about. A model is a file a test names; there is no
// directory for the tool to scan.
func freshConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeGGUF(t, filepath.Join(dir, "test.gguf"), "sense_asr", []string{"asr"})
	configPath := filepath.Join(dir, "config.toml")
	body := fmt.Sprintf("model = %q\nbackend = \"cpu\"\n", filepath.Join(dir, "test.gguf"))
	if err := os.WriteFile(configPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return configPath
}

// bareConfig writes a config with nothing in it: the shape of a fresh install, before anyone
// has named a checkpoint.
func bareConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("backend = \"cpu\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// writeGGUF writes a GGUF file: the header, a key-value section with the keys voiceime reads,
// and bytes after it that are never read. It is here rather than in the model package's tests
// because several CLI tests need a checkpoint the server config can name.
func writeGGUF(t *testing.T, path, family string, tasks []string) string {
	t.Helper()
	var raw []byte
	str := func(s string) {
		var length [8]byte
		binary.LittleEndian.PutUint64(length[:], uint64(len(s)))
		raw = append(raw, length[:]...)
		raw = append(raw, s...)
	}
	u32 := func(v uint32) {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], v)
		raw = append(raw, b[:]...)
	}
	u64 := func(v uint64) {
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], v)
		raw = append(raw, b[:]...)
	}

	raw = append(raw, []byte("GGUF")...)
	keyCount := uint64(2)
	if len(tasks) > 0 {
		keyCount++ // the spec key below
	}
	u32(3) // format version
	u64(0) // tensor count
	u64(keyCount)
	str("general.architecture")
	u32(8)
	str("testmodel")
	str("audiocpp.model_spec.family")
	u32(8)
	str(family)
	if len(tasks) > 0 {
		// The spec is JSON, and voiceime reads only its tasks out of it.
		spec := `{"family":"` + family + `","tasks":[`
		for i, task := range tasks {
			if i > 0 {
				spec += ","
			}
			spec += `"` + task + `"`
		}
		spec += "]}"
		str("audiocpp.model_spec.json")
		u32(8)
		str(spec)
	}
	raw = append(raw, make([]byte, 64)...)
	if err := os.WriteFile(path, raw, 0o644); err != nil {
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

func TestConfigInitRefusesThenOverwritesWithForce(t *testing.T) {
	configPath := freshConfig(t)
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

	// The refusal has to be visible in human mode too: the step is documented
	// without --json, and exit 0 with no output reads as a success.
	code, stdout, stderr = runForTest(t, "config", "init", "--config", configPath)
	if code != ExitOK {
		t.Fatalf("init on an existing file (human mode): exit %d, stderr %s", code, stderr)
	}
	if !strings.Contains(stdout, "already exists") {
		t.Errorf("init said nothing in human mode, where a refusal and a success look alike:\n%s", stdout)
	}
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("config file missing after a no-op init: %v", err)
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
	code, stdout, stderr := runForTest(t, "model", "--json", "--config", configPath)
	if code == ExitOK {
		t.Fatal("describing a checkpoint that was never named succeeded")
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

	code, stdout, stderr := runForTest(t, "config", "set", "threads", "0", "--json", "--config", configPath)
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
	if !strings.Contains(fix, "config set threads") {
		t.Errorf("fix = %q, want the command that repairs the value", fix)
	}

	// The value really is in the file, so the report and the file agree.
	loaded, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(loaded), "threads = 0") {
		t.Error("the value the report says was written is not in the file")
	}

	// A usable value is a success, with the command that applies it named.
	code, stdout, stderr = runForTest(t, "config", "set", "threads", "4", "--json", "--config", configPath)
	if code != ExitOK {
		t.Fatalf("setting a valid value: exit %d, stderr %s", code, stderr)
	}
	if payload := decodeOne(t, stdout)["data"].(map[string]any); payload["valid"] != true {
		t.Errorf("valid = %v for a value that is fine", payload["valid"])
	}
}
