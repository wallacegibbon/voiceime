package cli

import (
	"flag"
	"os"
	"strings"

	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/ibus"
)

// fileExists is asked of the filesystem, never of a memory of it: the component file, the
// settings file and a model service's config can each have been moved by hand.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// cmdCapabilities is the whole of what this program will do, and what it will do it with: the
// commands, the settings with the values currently on record, the paths, the exit codes, the
// documents. It is one command because it is one question - "what would this program do, and
// with what values" - and a second command over the same answers is how a tool ends up
// describing itself differently in two places. The settings have no reader but this program,
// and their file has no writer but the person editing it.
func cmdCapabilities() *Command {
	return &Command{
		Name:    "capabilities",
		Summary: "Print the machine-readable command and configuration surface",
		Usage:   "voiceime capabilities",
		Run:     runCapabilities,
	}
}

func runCapabilities(ctx *Context, args []string) error {
	if len(args) > 0 {
		return UsageError("capabilities takes no arguments")
	}

	entries := []map[string]any{}
	for _, command := range commands() {
		flags := []map[string]string{}
		if command.Flags != nil {
			set := flag.NewFlagSet(command.Name, flag.ContinueOnError)
			command.Flags(set)
			set.VisitAll(func(f *flag.Flag) {
				flags = append(flags, map[string]string{
					"name":        f.Name,
					"usage":       f.Usage,
					"default":     f.DefValue,
					"placeholder": f.Value.String(),
				})
			})
		}
		entries = append(entries, map[string]any{
			"name":    command.Name,
			"summary": command.Summary,
			"usage":   command.Usage,
			// An empty list, not null: a caller iterating the actions of every
			// command should not have to know which commands have any. `flags`
			// below is already built as an empty slice for the same reason.
			"actions": orEmpty(command.Actions),
			"flags":   flags,
		})
	}

	document := map[string]any{
		"version":       Version,
		"config_path":   ctx.ConfigPath,
		"config_exists": fileExists(ctx.ConfigPath),
		"commands":      entries,
		"settings":      ctx.Config.Fields(),
		"paths": map[string]string{
			"config_dir":       config.ConfigDir(),
			"cache_dir":        config.CacheDir(),
			"data_dir":         config.DataDir(),
			"runtime_dir":      config.RuntimeDir(),
			"environment_dir":  config.EnvironmentDir(),
			"environment_file": ibus.EnvironmentFile(),
		},
		"exit_codes": map[string]int{
			"ok":           ExitOK,
			"failure":      ExitFailure,
			"usage":        ExitUsage,
			"precondition": ExitPrecondition,
		},
		"docs": []string{
			"docs/AGENT.md",
			"docs/DEPLOY.md",
			"docs/asr.md",
			"docs/ibus-protocol.md",
			"docs/measurements.md",
		},
	}

	if !ctx.JSON {
		ctx.Print("voiceime %s", Version)
		ctx.Print("config: %s", ctx.ConfigPath)
		if !fileExists(ctx.ConfigPath) {
			// The shape of a fresh install. Saying so is what keeps "these are the
			// defaults" and "these are your settings" distinguishable - and nothing is
			// written for the person: the file is theirs to copy and keep.
			ctx.Print("  (no file: those are the defaults. copy config.example.toml from the project to")
			ctx.Print("   that path and edit it; `voiceime doctor` names what is still missing)")
		}
		for _, field := range ctx.Config.Fields() {
			ctx.Print("  %-22s %v", field.Key, field.Value)
		}
		ctx.Print("commands: %s", commandNames())
		ctx.Print("set `--json` for the same thing with what each setting is for, the paths, and the exit codes")
	}
	return ctx.Emit("capabilities", document)
}

func commandNames() string {
	var names []string
	for _, command := range commands() {
		names = append(names, command.Name)
	}
	return strings.Join(names, ", ")
} // ibusState reports whether IBus is running and whether our engine is installed.
func ibusState() map[string]any {
	state := map[string]any{}
	if config.FindExecutable("ibus") == "" {
		state["installed"] = false
		state["note"] = "the ibus CLI is not on PATH"
		return state
	}
	version, err := probe("ibus", "version")
	if err != nil {
		state["installed"] = true
		state["daemon_running"] = false
		return state
	}
	state["installed"] = true
	state["daemon_running"] = true
	state["version"] = firstLine(version)

	if current, err := probe("ibus", "engine"); err == nil {
		state["current_engine"] = firstLine(current)
	}
	state["component_path"] = ibus.ComponentPath()
	state["component_installed"] = fileExists(ibus.ComponentPath())
	if listing, err := probe("ibus", "list-engine"); err == nil {
		state["engine_registered"] = listEngineContains(listing, ibus.EngineName)
	} else {
		state["engine_registered"] = false
	}
	return state
}
