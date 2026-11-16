package config

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// examplePath is the file the project ships for a person to copy. There is no command
// that writes the settings, so this file is how a settings file starts, and it is the
// only place all nine keys are written down next to a reason. A sample that does not
// parse, that carries a key this program stopped having, or that quietly disagrees with
// Default() is worse than no sample: it is the file a person copies and then cannot
// explain. So it is checked the way a source file is.
const examplePath = "../../config.example.toml"

func TestTheShippedExampleIsAFileThisProgramReads(t *testing.T) {
	if _, err := os.Stat(examplePath); err != nil {
		t.Fatalf("the example settings file is not there to copy: %v", err)
	}
	// Load is the same call the engine makes: a key this program does not have, or a
	// value in a shape it cannot read, fails here.
	fromExample, err := Load(examplePath)
	if err != nil {
		t.Fatalf("the example does not parse: %v", err)
	}
	// Copied and changed nothing is a valid file: the address and the id are both
	// absent, which is a finding and a fix, not a broken configuration.
	if blocking := Errors(fromExample.Validate()); len(blocking) > 0 {
		t.Errorf("the example as shipped is not usable: %v", blocking)
	}
	// And it changes nothing by itself: a sample whose values drift from the defaults is
	// a second place those values are decided.
	got, want := fromExample.Fields(), Default().Fields()
	for i := range want {
		if got[i].Key != want[i].Key || fmt.Sprint(got[i].Value) != fmt.Sprint(want[i].Value) {
			t.Errorf("the example sets %s to %v; the default is %v", got[i].Key, got[i].Value, want[i].Value)
		}
	}
}

// The example carries every key, commented or not: a key added to the program has to
// appear in the file a person copies, with its reason beside it, or the sample is a
// partial list and reads as complete.
func TestTheShippedExampleCarriesEverySetting(t *testing.T) {
	raw, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatalf("read the example: %v", err)
	}
	lines := strings.Split(string(raw), "\n")
	for _, field := range Default().Fields() {
		if !carriesKey(lines, field.Key) {
			t.Errorf("the example says nothing about %q, which is a setting this program reads", field.Key)
		}
	}
}

func carriesKey(lines []string, key string) bool {
	for _, line := range lines {
		trimmed := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "#"))
		name, _, found := strings.Cut(trimmed, "=")
		if found && strings.TrimSpace(name) == key {
			return true
		}
	}
	return false
}
