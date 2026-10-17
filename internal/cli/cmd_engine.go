package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/daemon"
	"github.com/wallacegibbon/voiceime/internal/ibus"
)

// cmdEngine installs and inspects the IBus component, and hosts the engine
// process the daemon spawns.
func cmdEngine() *Command {
	var (
		system bool
		apply  bool
		quiet  bool
	)
	return &Command{
		Name:    "engine",
		Summary: "Install, inspect or run the IBus engine that delivers text",
		Usage:   "voiceime engine <install|uninstall|status> [--system|--apply]",
		Flags: func(set *flag.FlagSet) {
			set.BoolVar(&system, "system", false, "install into /usr/share/ibus/component (needs root)")
			set.BoolVar(&apply, "apply", false, "also set the activation environment and restart IBus")
			set.BoolVar(&quiet, "quiet", false, "print only what changed")
		},
		Run: func(ctx *Context, args []string) error {
			action := "status"
			if len(args) > 0 {
				action = args[0]
			}
			switch action {
			case "install":
				return runEngineInstall(ctx, system, apply, quiet)
			case "uninstall":
				return runEngineUninstall(ctx)
			case "status":
				return runEngineStatus(ctx)
			default:
				return UsageError("unknown engine action %q", action)
			}
		},
	}
}

func runEngineInstall(ctx *Context, system, apply, quiet bool) error {
	binary, err := os.Executable()
	if err != nil {
		return FailureError("cannot locate this binary: %v", err)
	}
	component := ibus.DefaultComponent(ctx.Config, fmt.Sprintf("%s ibus-engine --ibus", binary))

	if system {
		// A system install is one file and no environment surgery, but it needs
		// root. Printing the exact command keeps the decision with the user (or
		// the agent) rather than escalating privileges on its own.
		path, err := ibus.Install(component)
		if err != nil {
			return FailureError("%v", err)
		}
		target := "/usr/share/ibus/component/voiceime.xml"
		command := fmt.Sprintf("sudo install -m 644 %s %s && ibus restart", path, target)
		if !quiet {
			ctx.Print("component written to %s", path)
			ctx.Print("install it system-wide with:")
			ctx.Print("  %s", command)
		}
		return ctx.Emit("engine install", map[string]any{
			"mode":      "system",
			"component": path,
			"target":    target,
			"run":       command,
			"installed": false,
		})
	}

	path, err := ibus.Install(component)
	if err != nil {
		return FailureError("%v", err)
	}
	// IBUS_COMPONENT_PATH *replaces* the default search path, so the value must
	// name the system directory too or every other engine disappears. Verified in
	// a sandbox daemon; see docs/ibus-protocol.md.
	paths := ibus.InstallPaths("")
	setEnv := fmt.Sprintf("dbus-update-activation-environment --systemd IBUS_COMPONENT_PATH=%s", paths)

	if !quiet {
		ctx.Print("component written to %s", path)
		ctx.Print("note: IBus does not read that directory by default, and IBUS_COMPONENT_PATH")
		ctx.Print("      replaces the default path, which is why it must also list")
		ctx.Print("      /usr/share/ibus/component")
		if apply {
			ctx.Print("enabling it now:")
		} else {
			ctx.Print("enable it with:")
		}
		ctx.Print("  %s", setEnv)
		ctx.Print("  ibus restart")
	}

	if apply {
		if _, err := exec.LookPath("dbus-update-activation-environment"); err != nil {
			return PreconditionError("sudo apt install dbus", "dbus-update-activation-environment is missing: %v", err)
		}
		if err := runCommand(setEnv); err != nil {
			return FailureError("%v", err)
		}
		// `ibus restart` alone is not enough. The daemon caches the component
		// registry together with the directories it watched when the cache was
		// written, so a newly added search path stays invisible until that cache
		// is gone. This was measured: with IBUS_COMPONENT_PATH already in the
		// daemon's environment, the engine did not appear until the cache was
		// removed and the daemon restarted.
		if cache := ibus.RegistryCachePath(); cache != "" {
			if err := os.Remove(cache); err != nil && !os.IsNotExist(err) {
				ctx.Print("warning: could not remove %s: %v", cache, err)
			} else if !quiet {
				ctx.Print("removed the component registry cache (%s)", cache)
			}
		}
		if err := runCommand("ibus restart"); err != nil {
			return FailureError("%v", err)
		}
		if !quiet {
			ctx.Print("activation environment updated and IBus restarted")
		}
	}

	registered, detail := engineRegistered(ctx)
	return ctx.Emit("engine install", map[string]any{
		"mode":              "user",
		"component":         path,
		"component_paths":   paths,
		"run":               []string{setEnv, "ibus restart"},
		"applied":           apply,
		"engine_registered": registered,
		"registration_note": detail,
	})
}

func runEngineUninstall(ctx *Context) error {
	if err := ibus.Uninstall(); err != nil {
		return FailureError("cannot remove the component: %v", err)
	}
	ctx.Print("component removed from %s", ibus.ComponentDir())
	ctx.Print("IBUS_COMPONENT_PATH may still name that directory; unset it and run `ibus restart` to finish.")
	return ctx.Emit("engine uninstall", map[string]any{
		"component_dir": ibus.ComponentDir(),
		"installed":     false,
	})
}

func runEngineStatus(ctx *Context) error {
	if config.FindExecutable("ibus") == "" {
		return PreconditionError("sudo apt install ibus", "the ibus CLI is not installed")
	}
	registered, detail := engineRegistered(ctx)
	state := map[string]any{
		"component_path":      ibus.ComponentPath(),
		"component_installed": ibus.Installed(),
		"component_dir":       ibus.ComponentDir(),
		"expected_paths":      ibus.InstallPaths(""),
		"engine_name":         ctx.Config.IBusEngineName,
		"engine_registered":   registered,
		"detail":              detail,
	}
	if listing, err := probe("ibus", "list-engine"); err == nil {
		state["engine_visible"] = listEngineContains(listing, ctx.Config.IBusEngineName)
	}
	if current, err := probe("ibus", "engine"); err == nil {
		state["current_engine"] = firstLine(current)
	}
	ctx.Print("component: %s (installed: %v)", ibus.ComponentPath(), ibus.Installed())
	ctx.Print("engine %s registered: %v", ctx.Config.IBusEngineName, registered)
	if detail != "" {
		ctx.Print("detail: %s", detail)
	}
	return ctx.Emit("engine status", state)
}

// engineRegistered asks the daemon whether it knows our engine.
func engineRegistered(ctx *Context) (bool, string) {
	listing, err := probe("ibus", "list-engine")
	if err != nil {
		return false, "ibus list-engine failed: " + err.Error()
	}
	if listEngineContains(listing, ctx.Config.IBusEngineName) {
		return true, ""
	}
	return false, "not listed by ibus: run `voiceime engine install` and restart IBus"
}

func runCommand(line string) error {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return nil
	}
	output, err := exec.Command(parts[0], parts[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v: %s", line, err, strings.TrimSpace(string(output)))
	}
	return nil
}

// cmdIBusEngine is the process ibus-daemon spawns. It is deliberately its own
// subcommand: the daemon runs the component's <exec> verbatim, so the engine must
// be reachable as a plain command line.
func cmdIBusEngine() *Command {
	var (
		commitOnEnable string
		address        string
		tracePath      string
		componentName  string
		audioFile      string
		repairModel    string
	)
	return &Command{
		Name:    "ibus-engine",
		Summary: "Run the IBus engine process (spawned by ibus-daemon)",
		Usage:   "voiceime ibus-engine --ibus [--address unix:path=...]",
		Flags: func(set *flag.FlagSet) {
			set.Bool("ibus", false, "accepted and ignored; ibus-daemon passes it")
			set.StringVar(&commitOnEnable, "commit-on-enable", "",
				"diagnostic: commit this text when the engine is activated, without audio")
			set.StringVar(&address, "address", "", "IBus bus address (default: IBUS_ADDRESS or the registry file)")
			set.StringVar(&tracePath, "trace", "", "append incoming D-Bus calls and the session log to this file")
			set.StringVar(&audioFile, "audio-file", "",
				"diagnostic: dictate from a WAV instead of the microphone")
			set.StringVar(&repairModel, "repair-model", "",
				"offline model for the repair pass (default: from config)")
			set.StringVar(&componentName, "name", ibus.DefaultComponentName,
				"bus name to own; must equal the component XML <name> of the file that spawned this process")
		},
		Run: func(ctx *Context, args []string) error {
			if tracePath != "" {
				// Enabled through the environment so the tracing code stays out of
				// the engine's own plumbing.
				os.Setenv("VOICEIME_IBUS_TRACE", tracePath)
			}
			return runIBusEngine(ctx, engineRunOptions{
				address:        address,
				commitOnEnable: commitOnEnable,
				componentName:  componentName,
				audioFile:      audioFile,
				repairModel:    repairModel,
				tracePath:      tracePath,
			})
		},
	}
}

// engineRunOptions is the engine process's whole configuration. It is a struct
// because the argument list had grown past the point where position is readable.
type engineRunOptions struct {
	address        string
	commitOnEnable string
	componentName  string
	audioFile      string
	repairModel    string
	tracePath      string
}

func runIBusEngine(ctx *Context, options engineRunOptions) error {
	address, commitOnEnable, componentName := options.address, options.commitOnEnable, options.componentName
	audioFile, repairModel, tracePath := options.audioFile, options.repairModel, options.tracePath
	if address == "" {
		resolved, err := ibus.ResolveAddress(ibus.Ping)
		if err != nil {
			return PreconditionError("ibus restart", "%v", err)
		}
		address = resolved
	}
	component := ibus.DefaultComponent(ctx.Config, "voiceime ibus-engine --ibus")
	// The daemon finds the engine process under the component name, so a mismatch
	// here is silent from our side and a timeout on the daemon's: it spawns the
	// process, then waits for a name nobody ever claims. Keeping the name
	// configurable is what makes a probe component (a second XML with its own
	// engine name) testable at all.
	if componentName != "" {
		component.Name = componentName
	}

	runCtx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Session logs must go somewhere: ibus-daemon spawns this process detached, so
	// its stdout is discarded. A trace file is where a user can find out why
	// dictation did nothing.
	logf := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		if tracePath != "" {
			appendLogLine(tracePath, "session: "+line)
			return
		}
		fmt.Fprintf(ctx.Stderr, "voiceime: %s\n", line)
	}

	var handler ibus.Handler
	switch {
	case commitOnEnable != "":
		// Diagnostic: commit a fixed string, no audio pipeline.
		ctx.Print("engine started on %s (diagnostic commit only)", address)
		handler = &diagnosticHandler{text: commitOnEnable, out: ctx.Stdout}
	default:
		pipeline, err := newPipelineHandler(runCtx, ctx, logf, audioFile, repairModel)
		if err != nil {
			return err
		}
		ctx.Print("engine started on %s; dictation begins when the engine is activated", address)
		handler = pipeline
	}
	engineErr := ibus.Run(runCtx, component, handler, address)
	if engineErr != nil {
		return FailureError("%v", engineErr)
	}
	return ctx.Emit("ibus-engine", map[string]any{"address": address, "stopped": true})
}

// newPipelineHandler assembles the dictation pipeline the engine runs: the resident
// server it talks to, the two entries a session uses, and either the microphone or a
// file as the audio source.
//
// There is no process to start and no model to load here any more. Both roles are
// served by the unit `voiceime server install` writes, which loads its models at
// startup, so activating the engine opens a connection and nothing else.
func newPipelineHandler(runCtx context.Context, ctx *Context, logf func(string, ...any), audioFile, repairModelOverride string) (*daemon.EngineHandler, error) {
	serverURL := strings.TrimSpace(ctx.Config.ASRServerURL)
	if serverURL == "" {
		return nil, PreconditionError("voiceime server install",
			"dictation needs the repair server: nothing is configured to decode audio")
	}
	repairModel := strings.TrimSpace(repairModelOverride)
	if repairModel == "" {
		repairModel = serverModelID(ctx.Config)
	}
	handler := &daemon.EngineHandler{
		Config:      ctx.Config,
		ServerURL:   serverURL,
		StreamModel: serverStreamModelID(ctx.Config),
		RepairModel: repairModel,
		Log:         logf,
	}
	logf("%s", serverDescription(ctx.Config, repairModel))
	if audioFile != "" {
		// Validated here rather than at first use: a typo in the path should fail
		// when the engine starts, not silently later.
		expanded := config.Expand(audioFile)
		if _, err := os.Stat(expanded); err != nil {
			return nil, FailureError("--audio-file %s: %v", expanded, err)
		}
		handler.AudioFile = expanded
	}
	return handler, nil
}

func appendLogLine(path, line string) {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer file.Close()
	fmt.Fprintln(file, line)
}

// diagnosticHandler commits a fixed string when activated, so that the delivery
// path can be verified without any audio at all.
type diagnosticHandler struct {
	text string
	out  interface{ Write([]byte) (int, error) }
}

func (h *diagnosticHandler) Enable(control ibus.Control) {
	fmt.Fprintf(h.out, "engine enabled; committing %q\n", h.text)
	if err := control.Commit(h.text); err != nil {
		fmt.Fprintf(h.out, "commit failed: %v\n", err)
		return
	}
	fmt.Fprintf(h.out, "committed\n")
}

func (h *diagnosticHandler) Disable() {
	fmt.Fprintln(h.out, "engine disabled")
}

func (h *diagnosticHandler) KeyEvent(keyval, keycode, state uint32) bool { return false }
