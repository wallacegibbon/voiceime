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

	"github.com/wallacegibbon/voiceime/internal/config"
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
		ctx.Print("IBus does not read that directory by default. Enable it with:")
		ctx.Print("  %s", setEnv)
		ctx.Print("  ibus restart")
		ctx.Print("note: IBUS_COMPONENT_PATH replaces the default path, which is why")
		ctx.Print("      it must also list /usr/share/ibus/component")
	}

	if apply {
		if _, err := exec.LookPath("dbus-update-activation-environment"); err != nil {
			return PreconditionError("sudo apt install dbus", "dbus-update-activation-environment is missing: %v", err)
		}
		if err := runCommand(setEnv); err != nil {
			return FailureError("%v", err)
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
		state["engine_visible"] = containsLine(listing, ctx.Config.IBusEngineName) ||
			strings.Contains(listing, ctx.Config.IBusEngineName)
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
	if strings.Contains(listing, ctx.Config.IBusEngineName) {
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
		},
		Run: func(ctx *Context, args []string) error {
			return runIBusEngine(ctx, address, commitOnEnable)
		},
	}
}

func runIBusEngine(ctx *Context, address, commitOnEnable string) error {
	if address == "" {
		resolved, err := ibus.ResolveAddress(ibus.Ping)
		if err != nil {
			return PreconditionError("ibus restart", "%v", err)
		}
		address = resolved
	}
	component := ibus.DefaultComponent(ctx.Config, "voiceime ibus-engine --ibus")

	runCtx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// The engine hosts the dictation pipeline. Until that wiring lands, the
	// engine still speaks the protocol, which is what makes injection testable.
	service := "engine started on %s"
	if commitOnEnable == "" {
		service += " (no audio pipeline attached yet: text delivery cannot be exercised)"
	}
	ctx.Print(service, address)

	handler := ibus.Handler(nil)
	if commitOnEnable != "" {
		handler = &diagnosticHandler{text: commitOnEnable, out: ctx.Stdout, jsonMode: ctx.JSON}
	}
	engineErr := ibus.Run(runCtx, component, handler, address)
	if engineErr != nil {
		return FailureError("%v", engineErr)
	}
	return ctx.Emit("ibus-engine", map[string]any{"address": address, "stopped": true})
}

// diagnosticHandler commits a fixed string when activated and reports key events,
// so that a session can be verified end to end without a microphone.
type diagnosticHandler struct {
	text     string
	out      interface{ Write([]byte) (int, error) }
	jsonMode bool
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

// engineBinaryHint is used by doctor to explain what the component points at.
func engineBinaryHint() string {
	binary, err := os.Executable()
	if err != nil {
		return ""
	}
	return filepath.Clean(binary)
}
