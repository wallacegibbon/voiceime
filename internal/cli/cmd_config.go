package cli

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// cmdConfig is the only supported way to edit settings from a script. It exists
// so an agent never has to generate TOML by hand: `config set` validates the
// type, writes atomically, and reports what the new config validates to.
func cmdConfig() *Command {
	return &Command{
		Name:    "config",
		Summary: "Inspect and edit the configuration file",
		Usage:   "voiceime config <path|show|init|get|set|validate> [args]",
		Run:     runConfig,
	}
}

func runConfig(ctx *Context, args []string) error {
	if len(args) == 0 {
		return UsageError("config needs an action: path, show, init, get, set or validate")
	}
	action, rest := args[0], args[1:]

	switch action {
	case "path":
		ctx.Print("%s", ctx.ConfigPath)
		return ctx.Emit("config path", map[string]any{
			"path":   ctx.ConfigPath,
			"exists": fileExists(ctx.ConfigPath),
		})

	case "show":
		fields := ctx.Config.Fields()
		ctx.Print("%s", ctx.ConfigPath)
		for _, field := range fields {
			ctx.Print("%-22s %v", field.Key, field.Value)
		}
		return ctx.Emit("config show", map[string]any{
			"path":   ctx.ConfigPath,
			"exists": fileExists(ctx.ConfigPath),
			"fields": fields,
		})

	case "init":
		// Writing over an existing file needs an explicit flag: the command has
		// to be safe to run blind from a setup script.
		force := false
		for _, arg := range rest {
			switch arg {
			case "--force":
				force = true
			case "--yes", "-y":
				// Accepted for symmetry with the rest of the surface; init has
				// nothing to confirm besides --force.
			default:
				return UsageError("config init: unexpected argument %q", arg)
			}
		}
		existed := fileExists(ctx.ConfigPath)
		if existed && !force {
			return ctx.Emit("config init", map[string]any{
				"path":    ctx.ConfigPath,
				"written": false,
				"existed": true,
				"note":    "already exists; pass --force to overwrite",
			})
		}
		if err := config.Save(ctx.Config, ctx.ConfigPath); err != nil {
			return FailureError("cannot write %s: %v", ctx.ConfigPath, err)
		}
		ctx.Print("wrote %s", ctx.ConfigPath)
		return ctx.Emit("config init", map[string]any{
			"path":    ctx.ConfigPath,
			"written": true,
			"existed": existed,
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

	case "set":
		if len(rest) != 2 {
			return UsageError("config set needs a key and a value")
		}
		key, value := rest[0], rest[1]
		previous, err := ctx.Config.Get(key)
		if err != nil {
			return UsageError("%v", err)
		}
		if err := ctx.Config.Set(key, value); err != nil {
			return UsageError("%v", err)
		}
		if err := config.Save(ctx.Config, ctx.ConfigPath); err != nil {
			return FailureError("cannot write %s: %v", ctx.ConfigPath, err)
		}
		issues := ctx.Config.Validate()
		if blocking := config.Errors(issues); len(blocking) > 0 {
			// The value is written, but the config is now unusable. Saying so
			// loudly here is better than letting the daemon fail later.
			for _, issue := range blocking {
				fmt.Fprintf(ctx.Stderr, "voiceime: %s %s: %s\n", issue.Severity, issue.Key, issue.Message)
			}
			return ctx.Emit("config set", map[string]any{
				"key": key, "previous": previous, "value": value,
				"written": true, "valid": false, "issues": blocking,
			})
		}
		ctx.Print("%s = %v", key, value)
		return ctx.Emit("config set", map[string]any{
			"key": key, "previous": previous, "value": value, "written": true, "valid": true,
		})

	case "validate":
		issues := ctx.Config.Validate()
		blocking := config.Errors(issues)
		for _, issue := range issues {
			ctx.Print("%-7s %-22s %s", issue.Severity, issue.Key, issue.Message)
			if issue.Fix != "" {
				ctx.Print("        fix: %s", issue.Fix)
			}
		}
		if len(issues) == 0 {
			ctx.Print("config is valid")
		}
		if err := ctx.Emit("config validate", map[string]any{
			"valid":  len(blocking) == 0,
			"issues": issues,
		}); err != nil {
			return err
		}
		if len(blocking) > 0 {
			return PreconditionError("voiceime config validate", "%d blocking issue(s) in %s",
				len(blocking), ctx.ConfigPath)
		}
		return nil
	}

	return UsageError("unknown config action %q", action)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// cmdCapabilities exists so an agent can ask the binary what it can do instead
// of parsing this repository. The command list and the config key list are both
// derived from live code, so the answer cannot be stale.
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
			"config_dir":  config.ConfigDir(),
			"cache_dir":   config.CacheDir(),
			"data_dir":    config.DataDir(),
			"runtime_dir": config.RuntimeDir(),
			"socket":      config.SocketPath(),
			"model_index": config.ModelIndexPath(),
			"models_dir":  ctx.Config.ResolvedModelsDir(),
		},
		"exit_codes": map[string]int{
			"ok":           ExitOK,
			"failure":      ExitFailure,
			"usage":        ExitUsage,
			"precondition": ExitPrecondition,
		},
		"environment": environment(ctx),
		"docs": []string{
			"docs/AGENT.md",
			"docs/decisions.md",
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
}

// environment is the discovery result an agent needs before it can configure
// anything: which programs are present, and whether the session can carry text.
func environment(ctx *Context) map[string]any {
	environment := map[string]any{}

	if binary, err := ctx.Config.FindASRBinary(); err == nil {
		environment["asr_bin"] = binary
		if version, err := probe(binary, "--version"); err == nil {
			environment["asr_version"] = firstLine(version)
			environment["asr_backends_line"] = lastLineMatching(version, "backends:")
		}
	} else {
		environment["asr_bin"] = nil
	}

	if manager, err := ctx.Config.FindModelManager(); err == nil {
		environment["model_manager"] = manager
	} else {
		environment["model_manager"] = nil
	}

	modelsDir := ctx.Config.ResolvedModelsDir()
	environment["models_dir"] = modelsDir
	environment["models_dir_exists"] = dirExists(modelsDir)
	environment["models_dir_gguf_count"] = countGGUF(modelsDir)

	environment["capture_tool"] = config.FindExecutable("parec", "pw-record")
	environment["play_tool"] = config.FindExecutable("pw-play", "paplay", "canberra-gtk-play")
	environment["notify_tool"] = config.FindExecutable("notify-send")
	environment["default_source"] = defaultSource()
	environment["wayland_display"] = os.Getenv("WAYLAND_DISPLAY")
	environment["display"] = os.Getenv("DISPLAY")

	environment["ibus"] = ibusState(ctx)
	return environment
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// countGGUF is a cheap inventory of the models directory. It deliberately does
// not inspect the files: that is `voiceime models refresh`, which caches.
func countGGUF(dir string) int {
	matches, err := filepath.Glob(filepath.Join(dir, "*.gguf"))
	if err != nil {
		return 0
	}
	nested, err := filepath.Glob(filepath.Join(dir, "*", "*.gguf"))
	if err != nil {
		return len(matches)
	}
	return len(matches) + len(nested)
}

func lastLineMatching(text, prefix string) string {
	last := ""
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), prefix) {
			last = strings.TrimSpace(line)
		}
	}
	return last
}

// ibusState reports whether IBus is running and whether our engine is installed.
// This is the single most common reason text delivery fails, so it is part of
// the environment report rather than only of doctor.
func ibusState(ctx *Context) map[string]any {
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
	state["component_path"] = ibusComponentPath()
	state["component_installed"] = fileExists(ibusComponentPath())
	if listing, err := probe("ibus", "list-engine"); err == nil {
		state["engine_registered"] = containsLine(listing, ctx.Config.IBusEngineName)
	} else {
		state["engine_registered"] = false
	}
	return state
}

// ibusComponentPath is where the engine definition is installed. IBus discovers
// components by scanning this directory, so installing the engine is a file
// write plus a daemon restart - no build step, no root.
func ibusComponentPath() string {
	return filepath.Join(config.DataDir(), "component", "voiceime.xml")
}
