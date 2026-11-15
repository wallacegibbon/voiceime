package cli

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/ibus"
	"github.com/wallacegibbon/voiceime/internal/model"
)

// configActions is the whole of the configuration command: read the file, write one value.
// Applying a setting is not one of them - that is `voiceime apply`, which converges
// everything that reads the file at once.
var configActions = []string{"path", "show", "init", "get", "set"}

func cmdConfig() *Command {
	var force bool
	return &Command{
		Name:    "config",
		Summary: "Inspect and edit the configuration file",
		Actions: configActions,
		Usage:   usageLine("config", configActions, "[args]"),
		Flags: func(set *flag.FlagSet) {
			set.BoolVar(&force, "force", false, "config init: overwrite an existing file")
		},
		Run: func(ctx *Context, args []string) error {
			return runConfig(ctx, args, force)
		},
	}
}

// configuredModel answers "which model is in use" where a user looks first. The answer comes
// from the file itself, so it is the same answer everywhere else in the tool.
func configuredModel(ctx *Context) map[string]string {
	out := map[string]string{}
	path := strings.TrimSpace(ctx.Config.Model)
	if path == "" {
		out["model"] = noModelAdvice()
		return out
	}
	info, err := model.Open(config.Expand(path))
	if err != nil {
		out["model"] = err.Error()
		return out
	}
	out["model"] = describeCheckpoint(info)
	return out
}

func runConfig(ctx *Context, args []string, force bool) error {
	if len(args) == 0 {
		return UsageError("config needs an action: %s", strings.Join(configActions, ", "))
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
		// The model setting is a path, and a path on its own does not say what will be
		// served. What the file says about itself is printed next to it.
		resolved := configuredModel(ctx)
		ctx.Print("%s", ctx.ConfigPath)
		for _, field := range fields {
			line := fmt.Sprintf("%-22s %v", field.Key, field.Value)
			if note, ok := resolved[field.Key]; ok && note != "" {
				line += "  -> " + note
			}
			ctx.Print("%s", line)
		}
		return ctx.Emit("config show", map[string]any{
			"path":     ctx.ConfigPath,
			"exists":   fileExists(ctx.ConfigPath),
			"fields":   fields,
			"resolved": resolved,
		})

	case "init":
		// Writing over an existing file needs an explicit flag (--force, declared
		// on the command): init has to be safe to run blind from a setup script.
		if len(rest) > 0 {
			return UsageError("config init: unexpected argument %q", rest[0])
		}
		existed := fileExists(ctx.ConfigPath)
		if existed && !force {
			ctx.Print("config already exists at %s; not written (pass --force to overwrite)", ctx.ConfigPath)
			return ctx.Emit("config init", map[string]any{
				"path":    ctx.ConfigPath,
				"written": false,
				"existed": true,
				"note":    "already exists; pass --force to overwrite",
			})
		}
		// Where audio.cpp was built is not something to guess at every run, so it is
		// guessed once here, written into the settings, and reported. A value that
		// already names a runnable file is kept: init never overwrites a choice.
		found := ""
		if _, err := ctx.Config.FindASRBinary(); err != nil {
			if candidates := config.DiscoverASRBinaries(); len(candidates) > 0 {
				found = candidates[0]
				ctx.Config.ASRBin = found
			}
		}
		if err := config.Save(ctx.Config, ctx.ConfigPath); err != nil {
			return FailureError("cannot write %s: %v", ctx.ConfigPath, err)
		}
		ctx.Print("wrote %s", ctx.ConfigPath)
		if found != "" {
			ctx.Print("pointed asr_bin at the audio.cpp CLI %s", found)
		}
		// Every default in this file is a measurement or a path from the machine
		// voiceime was developed on. The ones that are not portable are listed
		// rather than left for an agent to notice.
		facts := detectHardware()
		reviews := config.ReviewDefaults(ctx.Config, config.ReviewFacts{
			ASRBinary: found,
			Cores:     facts.Cores(),
			GPUs:      facts.GPUDrivers,
		})
		for _, review := range reviews {
			ctx.Print("review %-22s %v  (%s)", review.Key, review.Value, review.Why)
			if review.Fix != "" {
				ctx.Print("       fix: %s", review.Fix)
			}
		}
		return ctx.Emit("config init", map[string]any{
			"path":       ctx.ConfigPath,
			"written":    true,
			"existed":    existed,
			"discovered": map[string]any{"asr_bin": found},
			"review":     reviews,
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
			if !ctx.JSON {
				for _, issue := range blocking {
					ctx.Print("%-7s %-22s %s", issue.Severity, issue.Key, issue.Message)
				}
			}
			if err := ctx.Emit("config set", map[string]any{
				"key": key, "previous": previous, "value": value,
				"written": true, "valid": false, "issues": blocking,
			}); err != nil {
				return err
			}
			issue := blocking[0]
			return PreconditionError(issue.Fix, "%s = %v is written, but the configuration is unusable: %s",
				key, value, issue.Message)
		}
		applies := "voiceime apply"
		ctx.Print("%s = %v", key, value)
		ctx.Print("this takes effect when the processes that read it restart: run `%s`", applies)
		return ctx.Emit("config set", map[string]any{
			"key": key, "previous": previous, "value": value, "written": true, "valid": true,
			"apply_with": applies,
		})

	}

	return UsageError("unknown config action %q", action)
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
		"environment": environment(ctx),
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

	// The named checkpoint, as the file itself describes it. "How many GGUFs are around"
	// is not a question this tool answers: one file is named and the rest are somebody
	// else's.
	if path := strings.TrimSpace(ctx.Config.Model); path != "" {
		expanded := config.Expand(path)
		environment["model"] = expanded
		if info, err := model.Open(expanded); err == nil {
			environment["model_family"] = info.Family
			environment["model_size_bytes"] = info.SizeBytes
		} else {
			environment["model_error"] = err.Error()
		}
	}

	environment["capture_tool"] = config.FindExecutable("parec", "pw-record")
	environment["play_tool"] = config.FindExecutable("pw-play", "paplay", "canberra-gtk-play")
	environment["notify_tool"] = config.FindExecutable("notify-send")
	environment["default_source"] = defaultSource()
	environment["wayland_display"] = os.Getenv("WAYLAND_DISPLAY")
	environment["display"] = os.Getenv("DISPLAY")

	environment["ibus"] = ibusState()
	return environment
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
