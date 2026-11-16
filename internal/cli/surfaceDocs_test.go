package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// The agent contract is a document. A document that describes a different program than the
// one built is worse than none: an agent follows it, reads "unknown setting", and stops
// trusting the rest of it. `docs-check` proves only that links resolve, and `advice_test.go`
// only that advice strings name real things - neither notices a surface change half-applied,
// which is exactly the mistake this project keeps making (a command deleted, its table row
// left; a setting renamed, its row left). So the three lists AGENT.md makes - the commands,
// the settings table, the findings table - are compared with what this build has, in both
// directions.
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

	// The finding ids are what a caller branches on: `data.checks[].id` is called stable in
	// the same document, so the document has to name every one, and name nothing that is not
	// reported. A finding was removed in the last change (a machine's model service stopped
	// being this program's business), and the row for it stayed until it was noticed by hand.
	reported := reportedFindingIDs(t)
	if strings.Join(doc.findings, "|") != strings.Join(reported, "|") {
		t.Errorf("AGENT.md's findings table and this build's findings are different lists:\n  documented: %v\n  reported:   %v",
			doc.findings, reported)
	}
}

// The deploy guide names finding ids too, in the command a person runs to accept an
// installation. That list is deliberately not every finding: the two whose warning is never
// the reason a deploy failed are left out of it, since the `doctor=0` on the line above
// already reports anything that actually fails. So it is checked one way - every id it tells
// someone to look for has to be an id this build reports. An id that stopped existing prints
// "<name> not ok" at the end of a deploy, which reads like a fault in the machine rather
// than a fault in the guide.
func TestDEPLOYNamesFindingsThisBuildReports(t *testing.T) {
	raw, err := os.ReadFile("../../docs/DEPLOY.md")
	if err != nil {
		t.Fatalf("read the deploy guide: %v", err)
	}
	var asked []string
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "for id in ") {
			continue
		}
		asked = append(asked, strings.Fields(strings.TrimSuffix(strings.TrimSpace(line), "; do"))[3:]...)
	}
	if len(asked) < 5 {
		t.Fatalf("the deploy guide's acceptance command was not found, or is not a list of findings any more: %v", asked)
	}
	reported := map[string]bool{}
	for _, id := range reportedFindingIDs(t) {
		reported[id] = true
	}
	for _, id := range asked {
		if !reported[id] {
			t.Errorf("DEPLOY.md tells a deploy to check %q, which this build never reports", id)
		}
	}
}

// reportedFindingIDs is every id this build can put in a doctor report, read from the source
// that builds the reports: a `Check` literal's ID field, whether it is written as a string or
// as the name of a const in the same file. Restating the list here is the drift this test
// exists to catch, so it is not restated.
func reportedFindingIDs(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var found []string
	seen := map[string]bool{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", name, parseErr)
		}
		consts := map[string]string{}
		ast.Inspect(file, func(node ast.Node) bool {
			switch at := node.(type) {
			case *ast.ValueSpec:
				if len(at.Names) == 1 && len(at.Values) == 1 {
					if literal, ok := at.Values[0].(*ast.BasicLit); ok && literal.Kind == token.STRING {
						value, unErr := strconv.Unquote(literal.Value)
						if unErr == nil {
							consts[at.Names[0].Name] = value
						}
					}
				}
			case *ast.CompositeLit:
				name, ok := at.Type.(*ast.Ident)
				if !ok || name.Name != "Check" {
					return true
				}
				for _, element := range at.Elts {
					field, ok := element.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					key, ok := field.Key.(*ast.Ident)
					if !ok || key.Name != "ID" {
						continue
					}
					id := ""
					switch value := field.Value.(type) {
					case *ast.BasicLit:
						if value.Kind == token.STRING {
							id, _ = strconv.Unquote(value.Value)
						}
					case *ast.Ident:
						id = consts[value.Name]
					}
					if id != "" && !seen[id] {
						seen[id] = true
						found = append(found, id)
					}
				}
			}
			return true
		})
	}
	sort.Strings(found)
	return found
}

// engineCommandName is the one name in the registry that is not typed by hand.
const engineCommandName = "ibus-engine"

// settingNames is the settings list as the program knows it: the toml keys of the structure
// that is read at start-up. The one field with no key of its own - the path those values
// came from, which no settings file holds - is not a setting and is not in the contract.
func settingNames() []string {
	var names []string
	typ := reflect.TypeOf(*config.Default())
	for i := 0; i < typ.NumField(); i++ {
		key := strings.Split(typ.Field(i).Tag.Get("toml"), ",")[0]
		if key != "" && key != "-" {
			names = append(names, key)
		}
	}
	sort.Strings(names)
	return names
}

// surfaceDocument is the lists AGENT.md makes. They are read as a person reads them: the
// command block under "## Commands", the key column of the table under "## Settings", the
// finding column of the table under "## Verifying".
type surfaceDocument struct {
	commands map[string][]string
	settings []string
	findings []string
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
			// A command with actions states them as its second word, separated by `|`.
			// Anything else that comes next is a flag, a placeholder or a comment, and
			// belongs to the person reading the block, not to the surface.
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
		case "Verifying":
			if !strings.HasPrefix(line, "| ") {
				continue
			}
			// The finding is the last cell of the row, and the cell can go on explaining
			// what the finding means; only the name in backticks at its start is the list.
			cells := strings.Split(strings.Trim(line, "|"), "|")
			last := strings.TrimSpace(cells[len(cells)-1])
			if !strings.HasPrefix(last, "`") {
				continue
			}
			id, _, found := strings.Cut(strings.TrimPrefix(last, "`"), "`")
			if found {
				out.findings = append(out.findings, id)
			}
		}
	}
	sort.Strings(out.settings)
	sort.Strings(out.findings)
	return out
}
