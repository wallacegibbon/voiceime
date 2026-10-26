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
	scanned, err := models.Cached(ctx.Config)
	if err != nil {
		return resolved
	}
	// The same resolution the service is configured from (D32), so "which model is in
	// use" has one answer rather than one per command.
	choice := models.Resolve(scanned, ctx.Config.Language, ctx.Config.StreamingModel, ctx.Config.RefineModel)
	if strings.TrimSpace(ctx.Config.StreamingModel) == "" {
		if choice.Stream == "" {
			resolved["streaming_model"] = "empty means pick one, and no streaming model is installed"
		} else {
			resolved["streaming_model"] = "empty means pick one: " + describeModel(scanned, choice.Stream)
		}
	}
	if strings.TrimSpace(ctx.Config.RefineModel) == "" {
		if choice.Repair == "" {
			resolved["refine_model"] = "empty means reuse the streaming model, which is not installed"
		} else {
			resolved["refine_model"] = "empty means reuse the streaming model: " + describeModel(scanned, choice.Repair)
		}
	} else if found := models.Lookup(scanned, ctx.Config.RefineModel); found != nil {
		resolved["refine_model"] = "in use: " + describeModel(scanned, found.Path)
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
//
// A struct rather than a map: every step fills one field, the JSON keeps the same keys, and
// nothing has to assert the type of a value it just put in.
type applyReport struct {
	Service   string   `json:"service"`
	Served    []string `json:"served,omitempty"`
	Listener  string   `json:"listener"`
	Component string   `json:"component"`
	Engine    string   `json:"engine"`
}

// applyConfig makes the running installation match the file, and reports what actually
// happened rather than what was attempted: the service and the listener are restarted, the
// installed IBus component is rewritten when the settings it owns have changed, and the
// engine is dropped so the next activation runs the new binary.
func applyConfig(ctx *Context) error {
	// Refuse an unusable file before touching any of the processes. This is the failure that
	// used to be reported as success: a setting naming a model that is not there is valid
	// TOML and passes every syntactic check, so it only showed up as a crash-looping service
	// while `config apply` printed "service: restarted" and exited 0.
	if issues := config.Errors(ctx.Config.Validate()); len(issues) > 0 {
		issue := issues[0]
		return PreconditionError(issue.Fix, "cannot apply the configuration: %s: %s", issue.Key, issue.Message)
	}

	var report applyReport
	var err error
	if report.Service, report.Served, err = applyService(ctx); err != nil {
		return err
	}
	report.Listener = applyListener(ctx)
	if report.Component, err = applyComponent(ctx.Config); err != nil {
		return FailureError("%v", err)
	}
	report.Engine = dropEngines()

	if !ctx.JSON {
		ctx.Print("service:  %v", report.Service)
		if len(report.Served) > 0 {
			ctx.Print("serving:  %s", strings.Join(report.Served, " and "))
		}
		ctx.Print("listener: %v", report.Listener)
		ctx.Print("component: %v", report.Component)
		ctx.Print("engine:   %v", report.Engine)
	}
	return ctx.Emit("config apply", report)
}

// applyService restarts the recognition service, and waits until it serves what the
// configuration now names.
//
// It builds the spec it expects *before* restarting: a configuration that cannot be served
// is refused instead of applied and then left unverified.
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
	spec, specErr := buildServerSpec(ctx.Config, scanned, port, "", "")
	if specErr != nil {
		return "", nil, specErr
	}
	if restartErr := runCommand("systemctl --user restart " + serverUnitName); restartErr != nil {
		return "", nil, FailureError("restart %s: %v", serverUnitName, restartErr)
	}
	if local {
		probeCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		waitErr := waitForEntries(probeCtx, ctx.Config.ASRServerURL, spec)
		cancel()
		if waitErr != nil {
			return "", nil, FailureError("the service restarted but is not serving the configured models: %v", waitErr)
		}
		served = []string{filepath.Base(spec.StreamModel), filepath.Base(spec.RepairModel)}
	}
	return "restarted", served, nil
}

// applyListener restarts the shortcut listener, which reads the key once at its start.
func applyListener(ctx *Context) string {
	if _, err := exec.LookPath("systemctl"); err != nil || !fileExists(servicePath()) {
		return "not installed"
	}
	if err := runCommand("systemctl --user restart voiceime-hotkey"); err != nil {
		return "restart failed"
	}
	return "restarted"
}

// dropEngines names what happened to the engine processes, for the report.
//
// By pid, never by a pkill pattern: `pkill -f "voiceime ibus-engine"` matches any process
// whose command line merely mentions that string - a shell running a command that contains
// it, an editor with the docs open - and measured here, it killed the shell that ran this
// command twice. SIGTERM first, because the engine's shutdown path commits the sentence in
// progress (D25).
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

// withConfiguredEngine returns the installed component with the fields the configuration
// owns changed, and whether anything changed at all.
//
// Everything else - the exec line above all - is left as it is: `config apply` may be run
// from a checkout while the component names an installed binary, and repointing it would
// replace a working delivery path with a path that disappears when the checkout moves
// (install_check reports that case as its own finding).
func withConfiguredEngine(installed ibus.Component, cfg *config.Config) ibus.Component {
	updated := installed
	updated.EngineName = cfg.IBusEngineName
	updated.Language = cfg.Language
	return updated
}

// componentNeedsRewrite compares the file on disk with what this version would write.
//
// Comparing the parsed component with the updated one is not enough, and that is how the
// escaped symbol survived a rewrite: the reader decodes the reference, so both sides render
// the same character and the file - which still holds the escaped text - was reported as
// unchanged. Comparing bytes also picks up a file written by an older version, which is
// exactly the upgrade case this has to heal.
func componentNeedsRewrite(raw []byte, updated ibus.Component) bool {
	return string(raw) != updated.XML()
}

// applyComponent rewrites the installed IBus component when the configuration disagrees
// with it, and restarts IBus so the daemon reads the new file.
//
// Before this, `ibus_engine_name` was the one setting `config apply` silently ignored: the
// engine name is baked into the component at `engine install` time, so changing it made
// the selector point at an engine IBus had never heard of while every other setting
// applied - and `doctor` reported it as "not registered" without saying why.
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

// stopEngines asks every running engine process to exit, and reports how many there were
// and whether any had to be killed.
//
// A process matches on its executable, not on a substring of its command line: only a
// process started from this binary can be an engine (see processesRunning).
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
			// A refusal that prints nothing is indistinguishable from success in
			// human mode (exit 0, empty stdout), and the step is documented without
			// --json: it has to say it did nothing. JSON mode carries the same fact
			// in the note, and Print stays out of the single JSON document.
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
			// The value is written - staging a setting before the thing it names
			// exists (a model path, a server that is not running yet) is how a
			// deployment is scripted - but the file is now unusable, and saying so
			// only on stderr was not enough: an agent that branches on exit codes saw
			// success, and the next command failed somewhere else. The report is
			// emitted first so a JSON caller still gets `written: true`, and then the
			// command fails with the fix for the value that broke the file.
			//
			// In JSON mode the findings travel in that object, so nothing else is
			// written to stderr: a caller parses it as one document.
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
		// A setting that is written is not yet a setting that is in force: every
		// resident process (the recognition service, the engine, the shortcut
		// listener) reads the configuration when it starts. Naming the one command
		// that restarts them is what turns "applied" into something an agent can
		// verify, and answering it the same way for every key is what keeps this from
		// becoming a second schema: the previous list of "settings the service reads"
		// had already gone stale, naming two settings that no longer existed and
		// calling `language` a service setting when it is sent per request.
		applies := "voiceime config apply"
		ctx.Print("%s = %v", key, value)
		ctx.Print("this takes effect when the processes that read it restart: run `%s`", applies)
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
	state["component_path"] = ibus.ComponentPath()
	state["component_installed"] = fileExists(ibus.ComponentPath())
	if listing, err := probe("ibus", "list-engine"); err == nil {
		state["engine_registered"] = listEngineContains(listing, ctx.Config.IBusEngineName)
	} else {
		state["engine_registered"] = false
	}
	return state
}
