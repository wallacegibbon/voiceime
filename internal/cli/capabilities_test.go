package cli

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// capabilities is the surface: what can be run, what can be set, where the files live, what
// the exit codes mean. Machine state is `doctor`'s. It used to be printed here too, which
// gave an agent two answers to one question - a probe of the audio.cpp CLI in each, a
// description of the checkpoint in each, a listing of IBus in each - and nothing to say which
// one to believe. The keys are asserted in both directions so the duplication cannot come
// back as one more plausible field.
func TestCapabilitiesIsTheSurfaceAndNotTheMachine(t *testing.T) {
	configPath := bareConfig(t)
	code, stdout, stderr := runForTest(t, "--config", configPath, "--json", "capabilities")
	if code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, stderr)
	}
	data := decodeOne(t, stdout)["data"].(map[string]any)

	for _, key := range []string{"commands", "settings", "paths", "exit_codes", "docs"} {
		if _, present := data[key]; !present {
			t.Errorf("the surface document has no %q", key)
		}
	}
	for _, key := range []string{"environment", "ibus", "model", "asr_bin", "capture_tool"} {
		if value, present := data[key]; present {
			t.Errorf("capabilities reports %q (%v): machine state has one home, `voiceime doctor`", key, value)
		}
	}
}

// The surface document is read by code that loops. A command with no actions has an empty
// list, not a null: "none" and "unknown" are the same answer here, and a caller that has to
// tell them apart will get it wrong in one of the two directions. `config` was the command
// that had actions, and it is gone - so this is about the shape, which has to hold for the
// next command that does have them, and about `flags`, which several commands exercise now.
func TestCapabilitiesListsActionsAndFlagsAsListsForEveryCommand(t *testing.T) {
	configPath := bareConfig(t)
	code, stdout, stderr := runForTest(t, "--config", configPath, "--json", "capabilities")
	if code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, stderr)
	}
	commands := decodeOne(t, stdout)["data"].(map[string]any)["commands"].([]any)
	if len(commands) == 0 {
		t.Fatal("no commands in the surface document")
	}
	withFlags := 0
	for _, entry := range commands {
		command := entry.(map[string]any)
		name, _ := command["name"].(string)
		for _, field := range []string{"actions", "flags"} {
			list, isList := command[field].([]any)
			if !isList {
				t.Errorf("%s: %s is %T, want a list", name, field, command[field])
				continue
			}
			if field == "flags" && len(list) > 0 {
				withFlags++
			}
		}
	}
	if withFlags == 0 {
		t.Error("no command declares flags, so the list shape is not being exercised here")
	}
}

// The settings are part of the surface, with the values on record: this is the only place
// they are reported, so it has to answer the whole question - the file, whether it exists,
// every key, the value that will be used - or there would be a reason for a second command
// over the same answers.
func TestCapabilitiesReportsTheSettingsItWillUse(t *testing.T) {
	configPath := freshConfig(t)
	onRecord, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}

	code, text, stderr := runForTest(t, "--config", configPath, "capabilities")
	if code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, stderr)
	}
	if !strings.Contains(text, configPath) {
		t.Errorf("the text does not name the file it read:\n%s", text)
	}
	onText := printedSettings(text)
	for _, field := range onRecord.Fields() {
		value, ok := onText[field.Key]
		if !ok {
			t.Errorf("the text does not print %s at all:\n%s", field.Key, text)
			continue
		}
		if value != fmt.Sprint(field.Value) {
			t.Errorf("the text prints %s as %q, want the value on record (%q)", field.Key, value, field.Value)
		}
	}

	_, stdout, stderr := runForTest(t, "--config", configPath, "--json", "capabilities")
	if stderr != "" {
		t.Fatalf("stderr: %s", stderr)
	}
	data := decodeOne(t, stdout)["data"].(map[string]any)
	if data["config_path"] != configPath || data["config_exists"] != true {
		t.Errorf("config_path/config_exists = %v/%v, want %v/true", data["config_path"], data["config_exists"], configPath)
	}
	printed := map[string]any{}
	for _, raw := range data["settings"].([]any) {
		setting := raw.(map[string]any)
		printed[setting["key"].(string)] = setting["value"]
	}
	for _, field := range onRecord.Fields() {
		if _, ok := printed[field.Key]; !ok {
			t.Errorf("the surface document has no setting %q", field.Key)
		}
	}
	if len(printed) != len(onRecord.Fields()) {
		t.Errorf("the surface document lists %d settings, this build has %d", len(printed), len(onRecord.Fields()))
	}
}

// printedSettings reads the settings out of the text report: the two-indented block of
// "key<spaces>value" lines. Reading it back instead of matching a literal keeps this a test
// about the values and not about padding.
func printedSettings(text string) map[string]string {
	printed := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		if !strings.HasPrefix(line, "  ") || strings.Contains(line, "(") {
			continue
		}
		key, value, found := strings.Cut(strings.TrimLeft(line, " "), "  ")
		if !found {
			continue
		}
		printed[strings.TrimSpace(key)] = strings.TrimSpace(value)
	}
	return printed
}

// With no settings file there is still one answer, and it says so: the defaults are printed,
// the file is named, and the copy that starts one is pointed at. A fresh install should not
// have to guess whether what it sees is a configuration or an absence of one.
func TestCapabilitiesSaysWhenThereIsNoSettingsFile(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "absent", "config.toml")
	code, text, stderr := runForTest(t, "--config", configPath, "capabilities")
	if code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, stderr)
	}
	for _, want := range []string{configPath, "no file", "config.example.toml"} {
		if !strings.Contains(text, want) {
			t.Errorf("the report of a fresh install does not say %q:\n%s", want, text)
		}
	}
}
