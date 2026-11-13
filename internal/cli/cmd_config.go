package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/ibus"
	"github.com/wallacegibbon/voiceime/internal/models"
	"github.com/wallacegibbon/voiceime/internal/server"
)

func cmdConfig() *Command {
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

// resolvedModels says which checkpoint an empty `model` resolves to, so `config show`
// can answer "which model is in use" for a value that is empty on purpose.
func resolvedModels(ctx *Context) map[string]string {
	resolved := map[string]string{}
	scanned, err := models.Cached(ctx.Config)
	if err != nil {
		return resolved
	}
	choice := models.Resolve(scanned, ctx.Config.Language, ctx.Config.Model)
	if strings.TrimSpace(ctx.Config.Model) != "" {
		if found := models.Lookup(scanned, ctx.Config.Model); found != nil {
			resolved["model"] = "in use: " + describeModel(scanned, found.Path)
		}
		return resolved
	}
	if choice.Model == "" {
		resolved["model"] = "empty means pick one, and no transcription model is installed"
	} else {
		resolved["model"] = "empty means pick one: " + describeModel(scanned, choice.Model)
	}
	return resolved
}

// describeModel names a checkpoint the way a user recognises it: the file, then its
// family, which is what the ranking is based on.
func describeModel(scanned []models.Model, path string) string {
	found := models.Lookup(scanned, path)
	if found == nil {
		return filepath.Base(path)
	}
	return filepath.Base(found.Path) + " (" + found.Family + ")"
}

// applyReport is what `config apply` did, one line per thing that reads the configuration.
type applyReport struct {
	Service   string   `json:"service"`
	Served    []string `json:"served,omitempty"`
	Component string   `json:"component"`
	// Desktop is the input-source list the desktop's own switcher reads: the entry the
	// engine needs there is a property of the *name*, so a changed ibus_engine_name has
	// to converge it as well.
	Desktop string `json:"desktop"`
	Engine  string `json:"engine"`
}

func applyConfig(ctx *Context) error {
	if issues := config.Errors(ctx.Config.Validate()); len(issues) > 0 {
		issue := issues[0]
		return PreconditionError(issue.Fix, "cannot apply the configuration: %s: %s", issue.Key, issue.Message)
	}

	var report applyReport
	var err error
	if report.Service, report.Served, err = applyService(ctx); err != nil {
		return err
	}
	if report.Component, err = applyComponent(ctx.Config); err != nil {
		return FailureError("%v", err)
	}
	// Not fatal on its own: an engine missing from this list still receives keystrokes,
	// it just cannot be shown by the desktop or switched to from its menu.
	report.Desktop, _ = addInputSource(ctx.Config.IBusEngineName)
	report.Engine = dropEngines()

	if !ctx.JSON {
		ctx.Print("service:  %v", report.Service)
		if len(report.Served) > 0 {
			ctx.Print("serving:  %s", strings.Join(report.Served, " and "))
		}
		ctx.Print("component: %v", report.Component)
		ctx.Print("desktop:  %v", report.Desktop)
		ctx.Print("engine:   %v", report.Engine)
	}
	return ctx.Emit("config apply", report)
}

func applyService(ctx *Context) (status string, served []string, err error) {
	if _, lookupErr := exec.LookPath("systemctl"); lookupErr != nil || !fileExists(serverUnitPath()) {
		return "not installed", nil, nil
	}
	scanned, scanErr := cachedModels(ctx)
	if scanErr != nil {
		return "", nil, scanErr
	}
	local, port := false, 0
	if strings.TrimSpace(ctx.Config.ASRServerURL) != "" {
		host, parsedPort, parseErr := server.ParseURL(ctx.Config.ASRServerURL)
		if parseErr != nil {
			return "", nil, FailureError("%v", parseErr)
		}
		local, port = server.IsLocalHost(host), parsedPort
	}
	spec, specErr := buildServerSpec(ctx.Config, scanned, port, "")
	if specErr != nil {
		return "", nil, specErr
	}
	if restartErr := runCommand("systemctl --user restart " + serverUnitName); restartErr != nil {
		return "", nil, FailureError("restart %s: %v", serverUnitName, restartErr)
	}
	if local {
		probeCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		waitErr := waitForModel(probeCtx, ctx.Config.ASRServerURL, spec)
		cancel()
		if waitErr != nil {
			return "", nil, FailureError("the service restarted but is not serving the configured models: %v", waitErr)
		}
		served = []string{filepath.Base(spec.Model)}
	}
	return "restarted", served, nil
}

// dropEngines names what happened to the engine processes, for the report.
func dropEngines() string {
	stopped, forced := stopEngines()
	switch {
	case stopped == 0:
		return "was not running"
	case forced:
		return fmt.Sprintf("%d dropped (one had to be killed); they restart on the next activation", stopped)
	default:
		return fmt.Sprintf("%d dropped; they restart on the next activation", stopped)
	}
}

func withConfiguredEngine(installed ibus.Component, cfg *config.Config) ibus.Component {
	updated := installed
	updated.EngineName = cfg.IBusEngineName
	return updated
}

// componentNeedsRewrite compares the file on disk with what this version would write.
func componentNeedsRewrite(raw []byte, updated ibus.Component) bool {
	return string(raw) != updated.XML()
}

func applyComponent(cfg *config.Config) (string, error) {
	if !ibus.Installed() {
		return "not installed", nil
	}
	raw, err := os.ReadFile(ibus.ComponentPath())
	if err != nil {
		return "", fmt.Errorf("cannot read the installed component: %w", err)
	}
	installed, err := ibus.ReadComponent(ibus.ComponentPath())
	if err != nil {
		return "", fmt.Errorf("cannot read the installed component: %w", err)
	}
	updated := withConfiguredEngine(installed, cfg)
	if !componentNeedsRewrite(raw, updated) {
		return "unchanged", nil
	}
	if _, err := ibus.Install(updated); err != nil {
		return "", fmt.Errorf("rewrite %s: %w", ibus.ComponentPath(), err)
	}
	// The daemon caches the component registry beside the directories it watched when the
	// cache was written, so a changed file needs that cache gone as well as a restart
	// (measured when the search path changed; see ibus.RegistryCachePath).
	if cache := ibus.RegistryCachePath(); cache != "" {
		_ = os.Remove(cache)
	}
	if err := runCommand("ibus restart"); err != nil {
		return "", fmt.Errorf("the component was rewritten but IBus did not restart, so it is still reading the old one: %w", err)
	}
	return "rewritten; IBus restarted", nil
}

func stopEngines() (stopped int, forced bool) {
	engines := processesRunning("ibus-engine")
	for _, engine := range engines {
		if process, err := os.FindProcess(engine.PID); err == nil {
			_ = process.Signal(syscall.SIGTERM)
		}
	}
	// Give the engine the time its shutdown path needs to commit the sentence in
	// progress; the drain budget is 5s (D25), so six is enough.
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if len(processesRunning("ibus-engine")) == 0 {
			return len(engines), false
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, engine := range processesRunning("ibus-engine") {
		if process, err := os.FindProcess(engine.PID); err == nil {
			_ = process.Kill()
			forced = true
		}
	}
	return len(engines), forced
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
			ctx.Print("config already exists at %s; not written (pass --force to overwrite)", ctx.ConfigPath)
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
		applies := "voiceime config apply"
		ctx.Print("%s = %v", key, value)
		ctx.Print("this takes effect when the processes that read it restart: run `%s`", applies)
		return ctx.Emit("config set", map[string]any{
			"key": key, "previous": previous, "value": value, "written": true, "valid": true,
			"apply_with": applies,
		})

	case "apply":
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
			// No fix on the error itself: the actionable command belongs to the
			// issue and was printed above. Naming this command as its own fix is
			// the kind of un-followable advice this project keeps finding.
			return PreconditionError("", "%d blocking issue(s) in %s",
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
// not inspect the files: that is `voiceime models list --refresh`, which records them.
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
	state["component_path"] = ibus.ComponentPath()
	state["component_installed"] = fileExists(ibus.ComponentPath())
	if listing, err := probe("ibus", "list-engine"); err == nil {
		state["engine_registered"] = listEngineContains(listing, ctx.Config.IBusEngineName)
	} else {
		state["engine_registered"] = false
	}
	return state
}
