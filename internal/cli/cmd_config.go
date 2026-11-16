package cli

import (
	"flag"
	"os"
	"strings"

	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/ibus"
)

// configActions is the whole of the configuration command: look at the file, read one
// value. Nothing here writes it - the settings are a file a person owns, and the project
// ships `config.example.toml` for them to copy - and nothing here applies it: that is
// `voiceime apply`, which converges everything that reads the file at once. Asking where
// the file is is not an action either: `show` starts with its path, and `capabilities`
// prints it without running any action at all.
var configActions = []string{"show", "get"}

func cmdConfig() *Command {
	return &Command{
		Name:    "config",
		Summary: "Read the configuration file",
		Actions: configActions,
		Usage:   usageLine("config", configActions, "[args]"),
		Run: func(ctx *Context, args []string) error {
			return runConfig(ctx, args)
		},
	}
}

func runConfig(ctx *Context, args []string) error {
	if len(args) == 0 {
		return UsageError("config needs an action: %s", strings.Join(configActions, ", "))
	}
	action, rest := args[0], args[1:]

	switch action {
	case "show":
		fields := ctx.Config.Fields()
		ctx.Print("%s", ctx.ConfigPath)
		if !fileExists(ctx.ConfigPath) {
			// The shape of a fresh install. Saying so is what keeps "these are the
			// defaults" and "these are your settings" distinguishable - and the file is
			// not written for them, because the file belongs to the person who reads it.
			ctx.Print("(no file yet: these are the defaults. copy the example the project ships,")
			ctx.Print("config.example.toml, to that path and edit it; `voiceime doctor` then names")
			ctx.Print("what is still missing)")
		}
		for _, field := range fields {
			ctx.Print("%-22s %v", field.Key, field.Value)
		}
		return ctx.Emit("config show", map[string]any{
			"path":   ctx.ConfigPath,
			"exists": fileExists(ctx.ConfigPath),
			"fields": fields,
		})

	case "get":
		if len(rest) != 1 {
			return UsageError("config get needs exactly one key")
		}
		value, err := ctx.Config.Get(rest[0])
		if err != nil {
			return UsageError("%v", err)
		}
		ctx.Print("%v", value)
		return ctx.Emit("config get", map[string]any{"key": rest[0], "value": value})

	}

	return UsageError("unknown config action %q; config takes %s", action, strings.Join(configActions, ", "))
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

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
		ctx.Print("commands: %s", commandNames())
		ctx.Print("set `--json` for the full surface")
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
