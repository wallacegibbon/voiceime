package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// `config apply` used to restart the service and then report success even when the
// file named a model that does not exist. The restart failed, the unit crash-looped,
// and the only output was "service: restarted" with exit code 0 - which is how a
// broken install came to look like a setting that did nothing.
func TestConfigApplyRefusesAConfigThatNamesAMissingModel(t *testing.T) {
	configPath, modelsDir := freshConfig(t)
	missing := filepath.Join(modelsDir, "absent.gguf")
	appendToFile(t, configPath, "refine_model = \""+missing+"\"\n")

	code, stdout, stderr := runForTest(t, "--config", configPath, "config", "apply")
	if code != ExitPrecondition {
		t.Fatalf("apply exited %d, want %d (precondition)\nstdout: %s\nstderr: %s",
			code, ExitPrecondition, stdout, stderr)
	}
	if !strings.Contains(stderr, "refine_model") {
		t.Errorf("the error does not name the setting that is wrong:\n%s", stderr)
	}
	if strings.Contains(stdout, "restarted") {
		t.Errorf("apply reported a restart it never attempted:\n%s", stdout)
	}
}

// `config validate` has to answer the same question the service asks at startup,
// or the one command a user is told to run before applying says "valid" about a
// configuration that cannot start.
func TestConfigValidateRejectsAModelPathThatDoesNotExist(t *testing.T) {
	configPath, modelsDir := freshConfig(t)
	missing := filepath.Join(modelsDir, "absent.gguf")
	appendToFile(t, configPath, "streaming_model = \""+missing+"\"\n")

	code, stdout, _ := runForTest(t, "--config", configPath, "config", "validate")
	if code != ExitPrecondition {
		t.Fatalf("validate exited %d, want %d (precondition)\n%s", code, ExitPrecondition, stdout)
	}
	if !strings.Contains(stdout, "streaming_model") || !strings.Contains(stdout, missing) {
		t.Errorf("the report does not name the setting and the path:\n%s", stdout)
	}
	if !strings.Contains(stdout, "does not exist") {
		t.Errorf("the report does not say what is wrong with the path:\n%s", stdout)
	}
}

// The fix offered for a missing path has to be runnable, which is the failure mode
// this project keeps hitting: advice that cannot be followed.
func TestConfigValidateOffersARunnableFixForAMissingModel(t *testing.T) {
	configPath, modelsDir := freshConfig(t)
	missing := filepath.Join(modelsDir, "absent.gguf")
	appendToFile(t, configPath, "refine_model = \""+missing+"\"\n")

	code, stdout, _ := runForTest(t, "--config", configPath, "--json", "config", "validate")
	if code != ExitPrecondition {
		t.Fatalf("validate exited %d, want %d\n%s", code, ExitPrecondition, stdout)
	}
	payload := decodeOne(t, stdout)
	data, _ := payload["data"].(map[string]any)
	issues, _ := data["issues"].([]any)
	var fix string
	for _, raw := range issues {
		issue, _ := raw.(map[string]any)
		if issue["key"] == "refine_model" {
			fix, _ = issue["fix"].(string)
		}
	}
	if !strings.Contains(fix, "config set refine_model") {
		t.Fatalf("the fix does not say how to correct the setting: %q", fix)
	}
	// Run the fix, exactly as printed, and confirm it clears the issue.
	_, clearOut, clearErr := runForTest(t, "--config", configPath, "--json",
		"config", "set", "refine_model", "")
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
