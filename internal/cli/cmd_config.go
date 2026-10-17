package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/models"
	"github.com/wallacegibbon/voiceime/internal/server"
)

// cmdConfig is the only supported way to edit settings from a script. It exists
// so an agent never has to generate TOML by hand: `config set` validates the
// type, writes atomically, and reports what the new config validates to.
func cmdConfig() *Command {
	// --force and --yes are declared here rather than read out of the positional
	// arguments, which is what the rest of this file used to do: the flag parser
	// rejects an undeclared flag before the command ever runs, so
	// `config init --force` - the documented way to overwrite a config - failed
	// with "flag provided but not defined" and could never work.
	var force, yes bool
	return &Command{
		Name:    "config",
		Summary: "Inspect and edit the configuration file",
		Usage:   "voiceime config <path|show|init|get|set|apply|validate> [args]",
		Flags: func(set *flag.FlagSet) {
			set.BoolVar(&force, "force", false, "config init: overwrite an existing file")
			set.BoolVar(&yes, "yes", false, "accepted and ignored; keeps scripts uniform")
		},
		Run: func(ctx *Context, args []string) error {
			return runConfig(ctx, args, force)
		},
	}
}

// resolvedModels says what the two model settings resolve to, for the ones the
// configuration leaves empty. It is what makes `config show` answer "which model is
// streaming", a question an empty value cannot answer on its own.
func resolvedModels(ctx *Context) map[string]string {
	resolved := map[string]string{}
	scanned, err := models.Scan(ctx.Config, false, nil)
	if err != nil {
		return resolved
	}
	streaming := ""
	if strings.TrimSpace(ctx.Config.StreamingModel) == "" {
		if picked := models.PickStreaming(scanned, ctx.Config.Language); picked != nil {
			streaming = picked.Path
			resolved["streaming_model"] = "empty means pick one: " + filepath.Base(picked.Path) + " (" + picked.Family + ")"
		} else {
			resolved["streaming_model"] = "empty means pick one, and no streaming model is installed"
		}
	}
	repair := ctx.Config.RefineModel
	if strings.TrimSpace(repair) == "" {
		if streaming != "" {
			resolved["refine_model"] = "empty means reuse the streaming model: " + filepath.Base(streaming)
		} else {
			resolved["refine_model"] = "empty means reuse the streaming model, which is not installed"
		}
	} else if found := models.Lookup(scanned, repair); found != nil {
		resolved["refine_model"] = "in use: " + filepath.Base(found.Path) + " (" + found.Family + ")"
	}
	return resolved
}

// applyConfig restarts the three processes that load the configuration, so a hand-edited
// file takes effect, and reports what actually happened rather than what was attempted.
func applyConfig(ctx *Context) error {
	result := map[string]any{}
	systemctl, _ := exec.LookPath("systemctl")

	// 1. The service: it renders the server config from this file at its own start, so a
	// model change lands here and nowhere else.
	if systemctl != "" && fileExists(serverUnitPath()) {
		if err := runCommand("systemctl --user restart " + serverUnitName); err != nil {
			return FailureError("restart %s: %v", serverUnitName, err)
		}
		scanned, scanErr := models.Scan(ctx.Config, false, nil)
		host, port, parseErr := server.ParseURL(ctx.Config.ASRServerURL)
		if scanErr == nil && parseErr == nil && server.IsLocalHost(host) {
			if spec, specErr := buildServerSpec(ctx.Config, scanned, port, "", ""); specErr == nil {
				probeCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
				err := waitForEntries(probeCtx, ctx.Config.ASRServerURL, spec)
				cancel()
				if err != nil {
					return FailureError("the service restarted but is not serving the configured models: %v", err)
				}
				result["served"] = []string{filepath.Base(spec.StreamModel), filepath.Base(spec.RepairModel)}
			}
		}
		result["service"] = "restarted"
	} else {
		result["service"] = "not installed"
	}

	// 2. The listener: the shortcut itself is read once, when it starts.
	if systemctl != "" && fileExists(servicePath()) {
		if err := runCommand("systemctl --user restart voiceime-hotkey"); err == nil {
			result["listener"] = "restarted"
		} else {
			result["listener"] = "restart failed"
		}
	} else {
		result["listener"] = "not installed"
	}

	// 3. The engine: it is spawned by ibus-daemon and stays, holding the configuration it
	// started with. Ending it is enough - IBus starts a fresh one on the next activation.
	result["engine"] = "was not running"
	if out, err := exec.Command("pgrep", "-f", "voiceime ibus-engine").Output(); err == nil && len(out) > 0 {
		if err := exec.Command("pkill", "-f", "voiceime ibus-engine").Run(); err == nil {
			result["engine"] = "dropped; it restarts on the next activation"
		} else {
			result["engine"] = "could not be stopped"
		}
	}

	if !ctx.JSON {
		ctx.Print("service:  %v", result["service"])
		if served, ok := result["served"].([]string); ok {
			ctx.Print("serving:  %s", strings.Join(served, " and "))
		}
		ctx.Print("listener: %v", result["listener"])
		ctx.Print("engine:   %v", result["engine"])
	}
	return ctx.Emit("config apply", result)
}

// serviceSetting reports whether a setting is one the recognition service reads when it
// starts, rather than one the session reads per request.
func serviceSetting(key string) bool {
	switch key {
	case "streaming_model", "refine_model", "chunk_seconds", "threads", "backend",
		"language", "session_options", "request_options",
		"asr_server_stream_model", "asr_server_model":
		return true
	}
	return false
}

func runConfig(ctx *Context, args []string, force bool) error {
	if len(args) == 0 {
		return UsageError("config needs an action: path, show, init, get, set, apply or validate")
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
		// An empty model setting is an instruction to choose, not an absence, and the
		// file cannot show what it chose. Printing the resolution here is where a user
		// looks first for "which model is running".
		resolved := resolvedModels(ctx)
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
			return ctx.Emit("config init", map[string]any{
				"path":    ctx.ConfigPath,
				"written": false,
				"existed": true,
				"note":    "already exists; pass --force to overwrite",
			})
		}
		// The default audio_cpp_dir is the reference machine's layout. Writing a
		// discovered checkout into the file is reported, not silent, and only ever
		// replaces the *default* - never a value someone chose.
		located := config.LocateAudioCPP(ctx.Config)
		discovered := ""
		if located != "" && config.Expand(ctx.Config.AudioCPPDir) != located {
			ctx.Config.AudioCPPDir = located
			discovered = located
		}
		if err := config.Save(ctx.Config, ctx.ConfigPath); err != nil {
			return FailureError("cannot write %s: %v", ctx.ConfigPath, err)
		}
		ctx.Print("wrote %s", ctx.ConfigPath)
		if discovered != "" {
			ctx.Print("discovered an audio.cpp checkout at %s", discovered)
		}
		// Every default in this file is a measurement or a path from the machine
		// voiceime was developed on. The ones that are not portable are listed
		// rather than left for an agent to notice.
		facts := detectHardware()
		reviews := config.ReviewDefaults(ctx.Config, config.ReviewFacts{
			AudioCPPCheckout: located,
			Cores:            facts.Cores(),
			GPUs:             facts.GPUDrivers,
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
			"discovered": map[string]any{"audio_cpp_dir": discovered},
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
		// A few settings are compiled into the recognition service when it starts rather
		// than read per request. Saying which, and what applies them, is the difference
		// between a setting that works and one that looks like it did nothing.
		applies := ""
		if serviceSetting(key) {
			applies = "voiceime config apply"
			ctx.Print("this reaches the recognition service at startup: run `%s`", applies)
		}
		return ctx.Emit("config set", map[string]any{
			"key": key, "previous": previous, "value": value, "written": true, "valid": true,
			"apply_with": applies,
		})

	case "apply":
		// Editing the file by hand is legitimate - it is the documented single source of
		// truth - but three processes read it at their own start: the recognition service
		// (models, window, threads), the engine (endpoints, language, server address) and
		// the shortcut listener (the key). One verb for "make the running system match the
		// file" is simpler than expecting a user to know which of the three to restart.
		return applyConfig(ctx)

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
		state["engine_registered"] = listEngineContains(listing, ctx.Config.IBusEngineName)
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
