package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// Advice names commands, and the commands live in another file. Deleting one leaves every
// "run `voiceime …`" sentence behind pointing at nothing, which is exactly the un-followable
// advice this project keeps finding in other people's tools - so the tree says so.
//
// Only string literals are read: those are the sentences a caller acts on. Comments may talk
// about a command that used to exist, and do.
func TestAdviceNamesACommandThatExists(t *testing.T) {
	byName := map[string]*Command{"help": {}}
	for _, command := range commands() {
		byName[command.Name] = command
	}

	root := repositoryRoot(t)
	fset := token.NewFileSet()
	checked := 0
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			// bin/ holds build output, .git a whole history of advice that was true
			// when it was written, and docs/ is markdown, which docs-check reads.
			if name := info.Name(); name == "bin" || name == ".git" || name == "docs" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		file, parseErr := parser.ParseFile(fset, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			value, unErr := strconv.Unquote(literal.Value)
			if unErr != nil {
				return true
			}
			for _, advice := range adviceIn(value) {
				checked++
				position := fset.Position(literal.Pos())
				where := strings.TrimPrefix(position.String(), root+string(filepath.Separator))
				command, known := byName[advice.command]
				if !known {
					t.Errorf("%s: %q names no command (run `voiceime help`)", where, advice.text)
					continue
				}
				// The action is checked too: `config apply` was a real action of a real
				// command, and deleting the action is the drift that actually happens.
				if advice.action != "" && len(command.Actions) > 0 &&
					!slices.Contains(command.Actions, advice.action) {
					t.Errorf("%s: %q: %s takes no action %q (it takes %s)", where, advice.text,
						advice.command, advice.action, strings.Join(command.Actions, ", "))
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("reading the source: %v", err)
	}
	// A check that silently matched nothing is a check that passed by accident, so the
	// count is part of the assertion.
	if checked < 50 {
		t.Fatalf("only %d pieces of advice checked: the pattern stopped matching", checked)
	}
	t.Logf("checked %d pieces of advice across the module", checked)
}

// advice is one reference to a command inside a string literal, and how it is written decides
// whether it is advice at all. This codebase writes it two ways, and the check depends on
// that: a Fix or error message that *is* the command, and a sentence that names it in
// backticks. "the voiceime engine", "voiceime serves one model" and a systemd Description
// are prose about the program, and are not read as advice.
type advice struct {
	text    string
	command string
	action  string
}

func adviceIn(value string) []advice {
	var found []advice
	for _, m := range adviceAtHeadRE.FindAllStringSubmatch(value, -1) {
		found = append(found, advice{text: m[0], command: m[1], action: m[2]})
	}
	for _, m := range adviceInBackticksRE.FindAllStringSubmatch(value, -1) {
		found = append(found, advice{text: m[0], command: m[1], action: m[2]})
	}
	return found
}

var (
	// A Fix or error message that is only the command: "voiceime apply".
	adviceAtHeadRE = regexp.MustCompile(`^voiceime ([a-z][a-z0-9-]*)(?: ([a-z][a-z0-9-]*))?`)
	// A sentence naming it: "run `voiceime doctor` again". The backtick is what separates
	// advice from prose about the program - "the voiceime engine", "voiceime serves one
	// model" - which is not something a caller can run.
	adviceInBackticksRE = regexp.MustCompile("`voiceime ([a-z][a-z0-9-]*)(?: ([a-z][a-z0-9-]*))?")
)

// repositoryRoot walks up from the package directory to the checkout: the directory with a
// go.mod in it.
func repositoryRoot(t *testing.T) string {
	t.Helper()
	path, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			t.Fatal("no go.mod at or above the test's working directory")
		}
		path = parent
	}
}
