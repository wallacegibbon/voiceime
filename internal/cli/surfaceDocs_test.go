package cli

import (
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// The agent contract is a document. A document that describes a different program than the
// one built is worse than none: an agent follows it, reads "unknown setting", and stops
// trusting the rest of it. `docs-check` proves only that links resolve, and `advice_test.go`
// only that advice strings name real things - neither notices a surface change half-applied,
// which is exactly the mistake this project keeps making (a command deleted, its table row
// left; a setting renamed, its row left). So the two lists AGENT.md makes - the commands and
// the settings table - are compared with what this build has, in both directions.
func TestAGENTDocumentsTheSurfaceThisBuildHas(t *testing.T) {
	raw, err := os.ReadFile("../../docs/AGENT.md")
	if err != nil {
		t.Fatalf("read the contract: %v", err)
	}
	doc := documentedSurface(string(raw))

	built := map[string][]string{}
	for _, command := range commands() {
		if command.Name == engineCommandName {
			// Not a command anyone types: ibus-daemon starts it. The contract says so in
			// prose beside the block, which is where a name that is not typed belongs.
			continue
		}
		built[command.Name] = command.Actions
	}
	for name, actions := range built {
		documented, ok := doc.commands[name]
		if !ok {
			t.Errorf("%q is a command in this build and is not in AGENT.md's command block", name)
			continue
		}
		if strings.Join(documented, "|") != strings.Join(actions, "|") {
			t.Errorf("%q: AGENT.md documents the actions %v, this build accepts %v",
				name, documented, actions)
		}
	}
	for name := range doc.commands {
		if _, ok := built[name]; !ok {
			t.Errorf("AGENT.md documents %q, which this build does not have", name)
		}
	}

	settings := settingNames()
	if strings.Join(doc.settings, "|") != strings.Join(settings, "|") {
		t.Errorf("AGENT.md's settings table and this build's settings are different lists:\n  documented: %v\n  built:      %v",
			doc.settings, settings)
	}
}

// engineCommandName is the one name in the registry that is not typed by hand.
const engineCommandName = "ibus-engine"

// settingNames is the settings list as the program knows it: the toml keys of the structure
// that is read at start-up, in the order the file's own writer uses.
func settingNames() []string {
	var names []string
	typ := reflect.TypeOf(*config.Default())
	for i := 0; i < typ.NumField(); i++ {
		key := strings.Split(typ.Field(i).Tag.Get("toml"), ",")[0]
		if key != "" {
			names = append(names, key)
		}
	}
	sort.Strings(names)
	return names
}

// surfaceDocument is the two lists AGENT.md makes. They are read as a person reads them: the
// command block under "## Commands", the key column of the table under "## Settings".
type surfaceDocument struct {
	commands map[string][]string
	settings []string
}

func documentedSurface(doc string) surfaceDocument {
	out := surfaceDocument{commands: map[string][]string{}}
	section := ""
	for _, line := range strings.Split(doc, "\n") {
		if strings.HasPrefix(line, "## ") {
			section = strings.TrimSpace(strings.TrimPrefix(line, "## "))
			continue
		}
		switch section {
		case "Commands":
			fields := strings.Fields(line)
			if len(fields) < 2 || fields[0] != "voiceime" {
				continue
			}
			name := fields[1]
			actions := []string{}
			// Only `voiceime config show|get|set` states actions, and it states them as the
			// second word. Anything else that comes next is a flag, a placeholder or a
			// comment, and belongs to the person reading the block, not to the surface.
			if len(fields) > 2 && !strings.ContainsAny(fields[2], "#-[<") {
				for _, action := range strings.Split(fields[2], "|") {
					if action != "" {
						actions = append(actions, action)
					}
				}
			}
			out.commands[name] = actions
		case "Settings":
			if !strings.HasPrefix(line, "| `") {
				continue
			}
			key, _, found := strings.Cut(strings.TrimPrefix(line, "| `"), "`")
			if found {
				out.settings = append(out.settings, key)
			}
		}
	}
	sort.Strings(out.settings)
	return out
}
