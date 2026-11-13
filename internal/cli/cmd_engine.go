package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/daemon"
	"github.com/wallacegibbon/voiceime/internal/gnome"
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
		// The component is system-wide; the list the desktop's own switcher reads is
		// per user, so it is written here either way.
		desktop, desktopReport := addInputSource(ctx.Config.IBusEngineName)
		if !quiet {
			ctx.Print("desktop: %s", desktop)
		}
		return ctx.Emit("engine install", map[string]any{
			"mode":               "system",
			"component":          path,
			"target":             target,
			"run":                command,
			"installed":          false,
			"gnome_input_source": desktopReport,
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

	// The desktop's own switcher is built from its input-source list, not from the engines
	// ibus-daemon knows: without this entry the menu cannot show the engine at all.
	desktop, desktopReport := addInputSource(ctx.Config.IBusEngineName)
	if !quiet {
		ctx.Print("desktop: %s", desktop)
	}

	registered, detail := engineRegistered(ctx)
	return ctx.Emit("engine install", map[string]any{
		"mode":               "user",
		"component":          path,
		"component_paths":    paths,
		"run":                []string{setEnv, "ibus restart"},
		"applied":            apply,
		"engine_registered":  registered,
		"registration_note":  detail,
		"gnome_input_source": desktopReport,
	})
}

func runEngineUninstall(ctx *Context) error {
	if err := ibus.Uninstall(); err != nil {
		return FailureError("cannot remove the component: %v", err)
	}
	removed, sourceReport := removeInputSource(ctx.Config.IBusEngineName)
	ctx.Print("component removed from %s", ibus.ComponentDir())
	if removed {
		ctx.Print("removed it from the GNOME input sources as well")
	}
	ctx.Print("IBUS_COMPONENT_PATH may still name that directory; unset it and run `ibus restart` to finish.")
	return ctx.Emit("engine uninstall", map[string]any{
		"component_dir":      ibus.ComponentDir(),
		"installed":          false,
		"gnome_input_source": sourceReport,
	})
}

// engineInputSource is the desktop-level half of an install: the pair GNOME's own switcher
// and its indicator are built from.
func engineInputSource(name string) gnome.InputSource {
	return gnome.InputSource{Type: "ibus", ID: name}
}

// addInputSource makes the engine appear in the desktop's input-source list.
func addInputSource(name string) (string, map[string]any) {
	source := engineInputSource(name)
	report := map[string]any{"source": source.String()}
	if !gnome.InputSourcesAvailable() {
		report["supported"] = false
		return "no GNOME input-source list on this desktop", report
	}
	report["supported"] = true
	added, err := gnome.AddInputSource(source)
	if err != nil {
		report["added"] = false
		report["error"] = err.Error()
		return "could not add " + source.String() + " to the GNOME input sources: " + err.Error(), report
	}
	report["added"] = added
	report["listed"] = true
	if added {
		return "appended " + source.String() + " to the GNOME input sources; the sources already configured are untouched", report
	}
	return "already in the GNOME input sources", report
}

// removeInputSource takes the engine back out of that list.
func removeInputSource(name string) (bool, map[string]any) {
	source := engineInputSource(name)
	report := map[string]any{"source": source.String()}
	if !gnome.InputSourcesAvailable() {
		report["supported"] = false
		return false, report
	}
	report["supported"] = true
	removed, err := gnome.RemoveInputSource(source)
	if err != nil {
		report["error"] = err.Error()
		return false, report
	}
	report["removed"] = removed
	return removed, report
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
		"log":                 engineLogPath(),
	}
	if listing, err := probe("ibus", "list-engine"); err == nil {
		state["engine_visible"] = listEngineContains(listing, ctx.Config.IBusEngineName)
	}
	if current, err := probe("ibus", "engine"); err == nil {
		state["current_engine"] = firstLine(current)
	}
	listed, listErr := gnome.HasInputSource(engineInputSource(ctx.Config.IBusEngineName))
	switch {
	case listErr != nil:
		state["gnome_input_source"] = map[string]any{"supported": false, "error": listErr.Error()}
	case listed:
		state["gnome_input_source"] = map[string]any{"supported": true, "listed": true}
	default:
		state["gnome_input_source"] = map[string]any{"supported": true, "listed": false}
	}
	ctx.Print("component: %s (installed: %v)", ibus.ComponentPath(), ibus.Installed())
	ctx.Print("engine %s registered: %v", ctx.Config.IBusEngineName, registered)
	if listErr == nil && !listed {
		// Not fatal - the engine still works, and the toggle says so - but it is why the
		// desktop's own menu and indicator cannot show it.
		ctx.Print("desktop: not in the GNOME input sources, so the top-right menu cannot show it")
		ctx.Print("         fix: voiceime engine install --apply")
	}
	// The engine is spawned by ibus-daemon, which discards what it prints. This file is
	// the only place a session's own account of a dictation survives, so it is named
	// where a user looks after "I pressed the key and nothing happened".
	ctx.Print("session log: %s", engineLogPath())
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

func cmdIBusEngine() *Command {
	var (
		commitOnEnable string
		address        string
		tracePath      string
		componentName  string
		audioFile      string
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
				"diagnostic: dictate this WAV when the engine is activated, instead of waiting for the key")
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
	tracePath      string
}

func runIBusEngine(ctx *Context, options engineRunOptions) error {
	address, commitOnEnable, componentName := options.address, options.commitOnEnable, options.componentName
	audioFile, tracePath := options.audioFile, options.tracePath
	if address == "" {
		resolved, err := ibus.ResolveAddress(ibus.Ping)
		if err != nil {
			return PreconditionError("ibus restart", "%v", err)
		}
		address = resolved
	}
	component := ibus.DefaultComponent(ctx.Config, "voiceime ibus-engine --ibus")
	if componentName != "" {
		component.Name = componentName
	}

	runCtx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	logPath := tracePath
	if logPath == "" {
		logPath = engineLogPath()
	}
	if logPath != "" {
		// One file per engine lifetime, bounded: a log that grows across months of
		// dictation is a disk leak with a useful name.
		rotateLog(logPath, maxEngineLogBytes)
	}
	logf := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		if logPath != "" {
			appendLogLine(logPath, "session: "+line)
		}
		if file, ok := ctx.Stderr.(*os.File); ok && isTerminalFile(file) {
			fmt.Fprintf(ctx.Stderr, "voiceime: %s\n", line)
		}
	}

	var handler ibus.Handler
	switch {
	case commitOnEnable != "":
		// Diagnostic: commit a fixed string, no audio pipeline.
		ctx.Print("engine started on %s (diagnostic commit only)", address)
		handler = &diagnosticHandler{text: commitOnEnable, out: ctx.Stdout}
	default:
		pipeline, err := newPushToTalk(ctx, logf, audioFile)
		if err != nil {
			return err
		}
		ctx.Print("engine started on %s; hold %s to dictate", address, pipeline.TriggerSpec())
		handler = pipeline
	}
	engineErr := ibus.Run(runCtx, component, handler, address)
	if engineErr != nil {
		return FailureError("%v", engineErr)
	}
	return ctx.Emit("ibus-engine", map[string]any{"address": address, "stopped": true})
}

func newPushToTalk(ctx *Context, logf func(string, ...any), audioFile string) (*daemon.PushToTalk, error) {
	if strings.TrimSpace(ctx.Config.ASRServerURL) == "" {
		return nil, PreconditionError("voiceime server install",
			"dictation needs the recognition server: nothing is configured to decode audio")
	}
	handler, err := daemon.NewPushToTalk(ctx.Config, logf)
	if err != nil {
		return nil, PreconditionError(`voiceime config set push_to_talk "<Control>apostrophe"`, "%v", err)
	}
	logf("%s", serverDescription(ctx.Config))
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
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	// Timestamped: the point of this file is to line a dictation up with the journal of
	// the service it talked to, and "session: listening" alone cannot be placed in time.
	// 0600 because the lines quote what the user said.
	fmt.Fprintf(file, "%s %s\n", time.Now().Format(time.RFC3339), line)
}

// isTerminalFile reports whether writes go to a terminal rather than a pipe or a file.
// A character device is the test: /dev/null is one too, and writing to it is harmless.
func isTerminalFile(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func engineLogPath() string { return filepath.Join(config.CacheDir(), "engine.log") }

// maxEngineLogBytes bounds that file. 1 MiB is thousands of dictations; the cap is there
// so a long-lived install cannot fill a disk with the record of what it said.
const maxEngineLogBytes = 1 << 20

func rotateLog(path string, limit int64) {
	info, err := os.Stat(path)
	if err != nil || info.Size() <= limit {
		return
	}
	_ = os.Truncate(path, 0)
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
