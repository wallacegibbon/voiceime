// Package cli implements the command surface.
//
// Two rules shape everything here, and both exist so that an automated agent can
// operate voiceime without a human:
//
//  1. Every command must be non-interactive and idempotent. There are no
//     prompts; anything a dialog would ask for is a flag or a config key.
//  2. Every command must be machine-readable. `--json` produces exactly one
//     JSON object on stdout, and failures are reported both as that object and
//     as a distinct exit code, so a caller never has to parse prose.
//
// docs/AGENT.md documents the surface for agents; `voiceime capabilities` prints
// the same information at runtime, derived from the code so it cannot go stale.
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// Exit codes. Distinct non-zero values let an agent branch without parsing text:
// a missing prerequisite is a different situation from a bug.
const (
	ExitOK           = 0
	ExitFailure      = 1 // the command ran and failed
	ExitUsage        = 2 // the command line was wrong
	ExitPrecondition = 3 // something the environment must supply is missing
)

// Errors that carry an exit code and, when known, a command that fixes the
// situation. An agent that hits a precondition error can run Fix and retry.
type codedError struct {
	kind    string
	message string
	fix     string
	code    int
}

func (e *codedError) Error() string { return e.message }

// UsageError marks a bad command line.
func UsageError(format string, args ...any) error {
	return &codedError{kind: "usage", message: fmt.Sprintf(format, args...), code: ExitUsage}
}

// PreconditionError marks a missing external dependency or unconfigured state,
// with the command that resolves it.
func PreconditionError(fix, format string, args ...any) error {
	return &codedError{kind: "precondition", message: fmt.Sprintf(format, args...), fix: fix, code: ExitPrecondition}
}

// FailureError marks a command that ran but did not succeed.
func FailureError(format string, args ...any) error {
	return &codedError{kind: "failure", message: fmt.Sprintf(format, args...), code: ExitFailure}
}

// Context is what every command receives: the resolved configuration, the
// output writers, and the two output-mode flags.
type Context struct {
	Config     *config.Config
	ConfigPath string
	JSON       bool
	Pretty     bool
	Stdout     io.Writer
	Stderr     io.Writer
}

// Print writes a human-readable line. Suppressed in JSON mode so that stdout
// stays a single parseable document.
func (c *Context) Print(format string, args ...any) {
	if c.JSON {
		return
	}
	fmt.Fprintf(c.Stdout, format+"\n", args...)
}

// Emit writes the result of a command: the JSON envelope in JSON mode, or
// nothing (the command has already printed its human-readable form).
func (c *Context) Emit(command string, data any) error {
	if !c.JSON {
		return nil
	}
	envelope := map[string]any{
		"command": command,
		"ok":      true,
	}
	if data != nil {
		envelope["data"] = data
	}
	return c.writeEnvelope(envelope)
}

func (c *Context) writeEnvelope(envelope map[string]any) error {
	encoder := json.NewEncoder(c.Stdout)
	if c.Pretty {
		encoder.SetIndent("", "  ")
	}
	return encoder.Encode(envelope)
}

// Command is one subcommand. FlagSet is built per invocation so repeated calls
// (tests) never share flag state.
type Command struct {
	Name    string
	Summary string
	Usage   string
	Flags   func(*flag.FlagSet)
	Run     func(ctx *Context, args []string) error
}

// commands is the registry. It is a function rather than a package variable so
// the list cannot be mutated accidentally, and so tests can call it twice.
func commands() []*Command {
	return []*Command{
		cmdCapabilities(),
		cmdConfig(),
		cmdDevices(),
		cmdDoctor(),
	}
}

// Run parses the command line, dispatches and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) int {
	globals, rest, err := splitGlobals(args)
	if err != nil {
		writeError(stderr, "", globals, err)
		return ExitUsage
	}

	if len(rest) == 0 || rest[0] == "help" {
		if len(rest) > 1 {
			return runHelp(rest[1], stdout, stderr, globals)
		}
		writeHelp(stdout)
		if len(rest) == 0 {
			// Bare invocation prints help but is still a usage error, so
			// scripts that forget a subcommand do not silently succeed.
			return ExitUsage
		}
		return ExitOK
	}

	name := rest[0]
	command := find(name)
	if command == nil {
		err := UsageError("unknown command %q; run `voiceime help`", name)
		writeError(stderr, name, globals, err)
		return ExitUsage
	}

	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard) // we render usage ourselves
	if command.Flags != nil {
		command.Flags(flags)
	}
	if err := flags.Parse(rest[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			printCommandHelp(stdout, command)
			return ExitOK
		}
		writeError(stderr, name, globals, UsageError("%v", err))
		return ExitUsage
	}

	cfg, err := config.Load(globals.configPath)
	if err != nil {
		writeError(stderr, name, globals, FailureError("%v", err))
		return ExitFailure
	}

	ctx := &Context{
		Config:     cfg,
		ConfigPath: globals.configPath,
		JSON:       globals.json,
		Pretty:     globals.pretty,
		Stdout:     stdout,
		Stderr:     stderr,
	}
	if err := command.Run(ctx, flags.Args()); err != nil {
		writeError(stderr, name, globals, err)
		return exitCode(err)
	}
	return ExitOK
}

func exitCode(err error) int {
	var coded *codedError
	if errors.As(err, &coded) && coded.code != 0 {
		return coded.code
	}
	return ExitFailure
}

func find(name string) *Command {
	for _, command := range commands() {
		if command.Name == name {
			return command
		}
	}
	return nil
}

// --------------------------------------------------------------------------- //
// Global flags
// --------------------------------------------------------------------------- //

type globalOptions struct {
	json       bool
	pretty     bool
	configPath string
}

// splitGlobals pulls the global flags out of the argument list wherever they
// appear. Accepting them before and after the subcommand means an agent can
// append `--json` to any command line without knowing its grammar.
func splitGlobals(args []string) (globalOptions, []string, error) {
	options := globalOptions{configPath: config.DefaultPath()}
	var rest []string
	for i := 0; i < len(args); i++ {
		switch arg := args[i]; {
		case arg == "--json":
			options.json = true
		case arg == "--pretty":
			options.pretty = true
		case arg == "--config":
			if i+1 >= len(args) {
				return options, nil, errors.New("--config needs a path")
			}
			i++
			options.configPath = config.Expand(args[i])
		case strings.HasPrefix(arg, "--config="):
			options.configPath = config.Expand(strings.TrimPrefix(arg, "--config="))
		default:
			rest = append(rest, arg)
		}
	}
	return options, rest, nil
}

// --------------------------------------------------------------------------- //
// Output
// --------------------------------------------------------------------------- //

func writeError(stderr io.Writer, command string, globals globalOptions, err error) {
	code := exitCode(err)
	kind := "failure"
	fix := ""
	var coded *codedError
	if errors.As(err, &coded) {
		kind = coded.kind
		if kind == "" {
			kind = "failure"
		}
		fix = coded.fix
	}
	if globals.json {
		payload := map[string]any{
			"command": command,
			"ok":      false,
			"error": map[string]any{
				"kind":    kind,
				"message": err.Error(),
			},
			"exit_code": code,
		}
		if fix != "" {
			payload["error"].(map[string]any)["fix"] = fix
		}
		encoder := json.NewEncoder(stderr)
		if globals.pretty {
			encoder.SetIndent("", "  ")
		}
		_ = encoder.Encode(payload)
		return
	}
	fmt.Fprintf(stderr, "voiceime: %s\n", err.Error())
	if fix != "" {
		fmt.Fprintf(stderr, "fix: %s\n", fix)
	}
}

func writeHelp(w io.Writer) {
	fmt.Fprint(w, "voiceime - local streaming speech input for GNOME, no GUI by design.\n\n")
	fmt.Fprint(w, "Usage:\n  voiceime <command> [flags] [args]\n\n")
	fmt.Fprint(w, "Commands:\n")
	list := commands()
	sort.Slice(list, func(i, j int) bool { return list[i].Name < list[j].Name })
	for _, command := range list {
		fmt.Fprintf(w, "  %-14s %s\n", command.Name, command.Summary)
	}
	fmt.Fprint(w, "\nGlobal flags (accepted before or after the command):\n")
	fmt.Fprint(w, "  --json           print one JSON object on stdout / errors on stderr\n")
	fmt.Fprint(w, "  --pretty         indent that JSON\n")
	fmt.Fprint(w, "  --config <path>  configuration file (default "+config.DefaultPath()+")\n")
	fmt.Fprint(w, "\nEvery command is non-interactive and idempotent; docs/AGENT.md has recipes.\n")
}

func runHelp(name string, stdout, stderr io.Writer, globals globalOptions) int {
	command := find(name)
	if command == nil {
		writeError(stderr, name, globals, UsageError("unknown command %q", name))
		return ExitUsage
	}
	printCommandHelp(stdout, command)
	return ExitOK
}

func printCommandHelp(w io.Writer, command *Command) {
	fmt.Fprintf(w, "voiceime %s - %s\n\nUsage:\n  %s\n", command.Name, command.Summary, command.Usage)
	if command.Flags != nil {
		flags := flag.NewFlagSet(command.Name, flag.ContinueOnError)
		command.Flags(flags)
		var lines []string
		flags.VisitAll(func(f *flag.Flag) {
			lines = append(lines, fmt.Sprintf("  --%-18s %s", f.Name, f.Usage))
		})
		if len(lines) > 0 {
			sort.Strings(lines)
			fmt.Fprintf(w, "\nFlags:\n%s\n", strings.Join(lines, "\n"))
		}
	}
}

// Main is the entry point used by cmd/voiceime.
func Main() {
	os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr))
}
