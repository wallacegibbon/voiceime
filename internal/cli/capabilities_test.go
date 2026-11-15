package cli

import (
	"testing"
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
// tell them apart will get it wrong in one of the two directions.
func TestCapabilitiesListsActionsAsAListForEveryCommand(t *testing.T) {
	configPath := bareConfig(t)
	code, stdout, stderr := runForTest(t, "--config", configPath, "--json", "capabilities")
	if code != ExitOK {
		t.Fatalf("exit %d, stderr %s", code, stderr)
	}
	commands := decodeOne(t, stdout)["data"].(map[string]any)["commands"].([]any)
	if len(commands) == 0 {
		t.Fatal("no commands in the surface document")
	}
	withActions := 0
	for _, entry := range commands {
		command := entry.(map[string]any)
		name, _ := command["name"].(string)
		actions, isList := command["actions"].([]any)
		if !isList {
			t.Errorf("%s: actions is %T, want a list", name, command["actions"])
			continue
		}
		if len(actions) > 0 {
			withActions++
		}
	}
	if withActions == 0 {
		t.Error("no command declares actions, so this test is not testing the case it is for")
	}
}
