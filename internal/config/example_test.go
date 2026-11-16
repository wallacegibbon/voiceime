package config

import (
	"fmt"
	"os"
	"reflect"
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
	got, want := settingsOf(t, fromExample), settingsOf(t, Default())
	for key, wantValue := range want {
		value, found := got[key]
		if !found {
			t.Errorf("the example says nothing about %s, which is a setting this program reads", key)
			continue
		}
		if value != wantValue {
			t.Errorf("the example sets %s to %s; the default is %s", key, value, wantValue)
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
	for key := range settingsOf(t, Default()) {
		if !carriesKey(lines, key) {
			t.Errorf("the example says nothing about %q, which is a setting this program reads", key)
		}
	}
}

// settingsOf is the settings as this package reads them: every key the decoder accepts,
// and the value on record for it. The list comes from the structure `Load` fills, so the
// tests below cannot drift from it, and the field that holds the path the values came
// from - which no settings file carries - is not a setting and is left out.
func settingsOf(t *testing.T, cfg *Config) map[string]string {
	t.Helper()
	out := map[string]string{}
	value := reflect.ValueOf(cfg).Elem()
	typ := value.Type()
	for i := 0; i < typ.NumField(); i++ {
		key := strings.Split(typ.Field(i).Tag.Get("toml"), ",")[0]
		if key == "" || key == "-" {
			continue
		}
		out[key] = fmt.Sprint(value.Field(i).Interface())
	}
	return out
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
