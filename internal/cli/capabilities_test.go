package cli

import (
	"testing"
)

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
