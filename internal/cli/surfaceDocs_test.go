package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
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

// A sentence anywhere in the tree can still name a command that is gone. The command block
// in AGENT.md is compared with this build in both directions, and the advice strings the
// program prints are checked against the registry, but neither looks at the rest of the
// text: a comment went on naming a deleted command until it was noticed by hand, and nothing
// in the suite had an opinion about it. So every place this project writes a command the way
// it writes one - in a code span, or as a line inside a fenced block - is asked whether this
// build has that command. Text that only puts the program name in front of a sentence
// (voiceime cannot name one, voiceime installs no script) is not naming a command, and is
// not matched.
func TestEveryCommandTheTextNamesIsACommand(t *testing.T) {
	root := repositoryRoot(t)
	known := map[string]bool{"help": true} // cli.go answers this one before the registry is reached
	for _, command := range commands() {
		known[command.Name] = true
	}
	checked := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			// bin/ is build output and .git is a record of every sentence that used to
			// be true.
			if name := info.Name(); name == "bin" || name == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		markdown := strings.HasSuffix(path, ".md")
		if !markdown && !strings.HasSuffix(path, ".go") {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		// A fenced block is a command to run; the lines around it are prose about the
		// program, which is the difference these two patterns are drawn on.
		inside := false
		for number, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "```") {
				inside = !inside
				continue
			}
			found := commandMentionRE.FindAllStringSubmatch(line, -1)
			if inside && markdown {
				if match := fenceCommandRE.FindStringSubmatch(line); match != nil {
					found = append(found, match)
				}
			}
			for _, match := range found {
				checked++
				if !known[match[1]] {
					t.Errorf("%s:%d names the command %q, which this build does not have", path, number+1, match[1])
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("reading the text: %v", err)
	}
	// The count exists to catch the patterns stopping, not to hold a number.
	if checked < 1 {
		t.Fatalf("no command mentions checked: the patterns stopped matching")
	}
	t.Logf("checked %d command mentions against this build", checked)
}

// commandMentionRE matches a command named in a code span. The backtick is required: it is
// what separates a command someone is told to run from a sentence that happens to put a word
// after the program name.
var commandMentionRE = regexp.MustCompile("`voiceime ([a-z][a-z-]+)")

// fenceCommandRE matches the first word of a line inside a fenced block, which is where the
// docs put a whole command.
var fenceCommandRE = regexp.MustCompile(`^voiceime ([a-z][a-z-]+)`)

// The engine's log is the only account of a dictation, and the contract has to say what each
// line means: the recovery table in DEPLOY.md starts from "no text, log shows …". Both
// directions are checked. A line the engine can write and the table does not name is a line a
// reader has to guess the meaning of - and a table row for a line this build cannot write is a
// description of a program that is not running, which is how a renamed log line used to survive
// its own rename.
func TestAGENTExplainsEveryLineTheEngineCanWrite(t *testing.T) {
	root := repositoryRoot(t)
	logged, err := engineLogLines(root)
	if err != nil {
		t.Fatalf("reading the engine's log calls: %v", err)
	}
	// The count is here to catch the walk stopping, not to hold a number.
	if len(logged) < 10 {
		t.Fatalf("only %d log lines found: the walk stopped matching", len(logged))
	}
	rows := documentedLogLines(t, root)
	if len(rows) < 10 {
		t.Fatalf("only %d log rows found in AGENT.md: the table stopped matching", len(rows))
	}

	for _, line := range logged {
		if !anyPatternMatches(line, rows) {
			t.Errorf("the engine can write %q and AGENT.md does not say what it means", shortLine(line))
		}
	}
	for _, pattern := range rows {
		// A row naming a value the sample line cannot carry (`server <address>`) is a shape,
		// not a sentence; the forward direction has nothing to compare it with.
		if strings.ContainsAny(pattern, "<") {
			continue
		}
		found := false
		for _, line := range logged {
			if logPatternRE(pattern).MatchString(line) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("AGENT.md explains %q, which this build cannot write", pattern)
		}
	}
	t.Logf("checked %d log lines the engine can write against %d documented rows", len(logged), len(rows))
}

// engineLogLines is every line the engine can put in its log: the first argument of each
// logf call, with each conversion replaced by a fixed token so a line can be compared with a
// documented pattern. A call whose format is nothing but conversions (the server description)
// carries no words of its own, and is left out.
func engineLogLines(root string) ([]string, error) {
	var lines []string
	fset := token.NewFileSet()
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			if name := info.Name(); name == "bin" || name == ".git" || name == "docs" || name == "tools" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				name = fun.Name
			case *ast.SelectorExpr:
				name = fun.Sel.Name
			}
			if name != "logf" || len(call.Args) == 0 {
				return true
			}
			literal, ok := call.Args[0].(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			text, quoteErr := strconv.Unquote(literal.Value)
			if quoteErr != nil {
				return true
			}
			if strings.TrimSpace(logConversionRE.ReplaceAllString(text, "")) == "" {
				return true
			}
			// The session writer prefixes every line it is given.
			lines = append(lines, "session: "+logConversionRE.ReplaceAllString(text, "x"))
			return true
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(lines)
	return lines, nil
}

// documentedLogLines is the first column of the log table in AGENT.md - the table that follows
// the sentence naming the log file.
func documentedLogLines(t *testing.T, root string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, "docs", "AGENT.md"))
	if err != nil {
		t.Fatalf("reading the contract: %v", err)
	}
	var rows []string
	after := false
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, "The engine's own log") {
			after = true
			continue
		}
		if !after {
			continue
		}
		if !strings.HasPrefix(line, "|") {
			if len(rows) > 0 {
				break
			}
			continue
		}
		cell, found := strings.CutPrefix(line, "| `")
		if !found {
			continue
		}
		pattern, _, found := strings.Cut(cell, "`")
		if !found {
			continue
		}
		rows = append(rows, pattern)
	}
	return rows
}

// logConversionRE is a Go conversion in a format string: `%.2fs` is the conversion `%.2f` and
// a literal `s`, so only the conversion is replaced.
var logConversionRE = regexp.MustCompile(`%[ +#\-0-9*]*(?:\.[0-9*]+)?[a-zA-Z]`)

// logPatternRE turns a documented line into a matcher: `…` stands for whatever the conversion
// carried, and the pattern matches anywhere in the line, since every line starts with the
// writer's own prefix.
func logPatternRE(pattern string) *regexp.Regexp {
	quoted := regexp.QuoteMeta(pattern)
	return regexp.MustCompile(strings.ReplaceAll(quoted, "…", ".*"))
}

func anyPatternMatches(line string, patterns []string) bool {
	for _, pattern := range patterns {
		if logPatternRE(pattern).MatchString(line) {
			return true
		}
	}
	return false
}

// shortLine keeps a failing message to the part of the line a reader would recognise.
func shortLine(line string) string {
	runes := []rune(line)
	if len(runes) <= 72 {
		return line
	}
	return string(runes[:72]) + "…"
}

// deadcode answers "can main reach this function", and staticcheck's U1000 skips exported
// names - and every name under internal/ is exported to the rest of this module, which is the
// blind spot. Two things sat in it: a second copy of the capture sample rate in the settings
// package, and a sentinel error whose message is assembled somewhere else. Both were named by
// nothing, and this is the check that finds that.
func TestNothingInThisModuleNamesSomethingItNeverUses(t *testing.T) {
	root := repositoryRoot(t)
	fset := token.NewFileSet()

	// Declarations come from non-test files; references are counted in every Go file,
	// because a helper only a test calls is still a thing that exists and is not what this
	// check is for.
	type declaration struct {
		name  string
		where string
	}
	var declarations []declaration
	var sources = map[string]string{}

	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			if name := info.Name(); name == "bin" || name == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		sources[path] = string(raw)
		if strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		for _, decl := range file.Decls {
			var names []string
			switch d := decl.(type) {
			case *ast.FuncDecl:
				names = []string{d.Name.Name}
			case *ast.GenDecl:
				for _, spec := range d.Specs {
					switch s := spec.(type) {
					case *ast.TypeSpec:
						names = append(names, s.Name.Name)
					case *ast.ValueSpec:
						for _, name := range s.Names {
							names = append(names, name.Name)
						}
					}
				}
			}
			for _, name := range names {
				if token.IsExported(name) {
					declarations = append(declarations, declaration{name: name, where: path})
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("reading the module: %v", err)
	}
	// The count is here to catch the walk stopping, not to hold a number.
	if len(declarations) < 50 {
		t.Fatalf("only %d exported names found: the walk stopped matching", len(declarations))
	}

	for _, found := range declarations {
		if !mentionedOutsideItsDeclaration(sources, found.name, found.where) {
			t.Errorf("%s declares %s, which nothing in this module references", found.where, found.name)
		}
	}
	t.Logf("checked %d exported names declared under this module", len(declarations))
}

// mentionedOutsideItsDeclaration reports whether name appears as a whole word anywhere other
// than on the line that declares it.
func mentionedOutsideItsDeclaration(sources map[string]string, name, declaredIn string) bool {
	mention := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`)
	declares := regexp.MustCompile(`^(?:func|type|var|const)?[[:space:]]*` + regexp.QuoteMeta(name) + `[[:space:]]*[(=:]`)
	for path, text := range sources {
		for _, line := range strings.Split(text, "\n") {
			if !mention.MatchString(line) {
				continue
			}
			if path == declaredIn && declares.MatchString(strings.TrimSpace(line)) {
				continue
			}
			return true
		}
	}
	return false
}
