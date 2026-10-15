package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/gnome"
	"github.com/wallacegibbon/voiceime/internal/ibus"
)

// cmdHotkey runs the push-to-talk listener: hold the configured key to dictate,
// release to return to whatever input method was selected before.
func cmdHotkey() *Command {
	var (
		spec      string
		keyspec   string
		quiet     bool
		install   bool
		logEvents bool
	)
	return &Command{
		Name:    "hotkey",
		Summary: "Listen for a global shortcut and switch to voiceime while it is held",
		Usage:   "voiceime hotkey <run|toggle|install|uninstall|status> [--keys <combo>]",
		Flags: func(set *flag.FlagSet) {
			set.StringVar(&spec, "keys", "", "shortcut to hold, e.g. <Control>apostrophe (default: from config)")
			set.StringVar(&keyspec, "engine", "", "engine to switch to while held (default: from config)")
			set.BoolVar(&quiet, "quiet", false, "only report state changes")
			set.BoolVar(&install, "yes", false, "accepted and ignored; keeps scripts uniform")
			set.BoolVar(&logEvents, "log-events", false, "log every shortcut event the daemon sends, not just the ones acted on")
		},
		Run: func(ctx *Context, args []string) error {
			action := "status"
			if len(args) > 0 {
				action = args[0]
			}
			switch action {
			case "run":
				return runHotkey(ctx, spec, quiet, logEvents)
			case "toggle":
				return toggleHotkey(ctx)
			case "install":
				return installHotkeyService(ctx, spec)
			case "uninstall":
				return uninstallHotkeyService(ctx)
			case "status":
				return hotkeyStatus(ctx, spec)
			default:
				return UsageError("unknown hotkey action %q", action)
			}
		},
	}
}

func hotkeySpec(ctx *Context, override string) string {
	if override != "" {
		return override
	}
	if ctx.Config.Hotkey != "" {
		return ctx.Config.Hotkey
	}
	return "<Control>apostrophe"
}

// runHotkey is the listener itself. It is meant to be run by systemd as a user
// service, but works in a terminal, which is how it is tested.
func runHotkey(ctx *Context, spec string, quiet, logEvents bool) error {
	address, err := ibus.ResolveAddress(ibus.Ping)
	if err != nil {
		return PreconditionError("ibus restart", "%v", err)
	}
	key, err := ibus.ParseShortcut(hotkeySpec(ctx, spec))
	if err != nil {
		return UsageError("%v", err)
	}
	target := ctx.Config.IBusEngineName

	listener, err := ibus.NewShortcutListener(address, byte(ctx.Config.HotkeyShortcutType), []ibus.Key{key})
	if err != nil {
		return FailureError("%v", err)
	}
	// The listener is a long-running command, so --json emits one object per event
	// rather than a single document: an agent watching it needs the transitions as
	// they happen, and the process does not end.
	emit := func(kind string, fields map[string]any) {
		if ctx.JSON {
			if fields == nil {
				fields = map[string]any{}
			}
			fields["event"] = kind
			encoder := json.NewEncoder(ctx.Stdout)
			_ = encoder.Encode(fields)
			return
		}
		if quiet {
			return
		}
		switch kind {
		case "log":
			ctx.Print("%v", fields["message"])
		case "dictation_on":
			ctx.Print("dictation on (was %v)", fields["previous"])
		case "dictation_off":
			ctx.Print("dictation off; back to %v", fields["previous"])
		}
	}
	logf := func(format string, args ...any) {
		emit("log", map[string]any{"message": fmt.Sprintf(format, args...)})
	}
	logf("tap %s to start dictating, tap it again to return to the previous engine",
		hotkeySpec(ctx, spec))
	logf("engine while dictating: %s; waiting for the shortcut", target)

	var (
		engineMu sync.Mutex
		previous string
		held     bool
	)

	toggle := func() {
		current, err := ibus.CurrentEngine(address)
		if err != nil {
			fmt.Fprintf(ctx.Stderr, "voiceime: %v\n", err)
			return
		}
		if current == target {
			if previous == "" {
				logf("already dictating, but the previous engine is not known; not switching")
				return
			}
			if err := ibus.SetEngine(address, previous); err != nil {
				fmt.Fprintf(ctx.Stderr, "voiceime: %v\n", err)
				return
			}
			emit("dictation_off", map[string]any{"previous": previous})
			previous = ""
			return
		}
		previous = current
		if err := ibus.SetEngine(address, target); err != nil {
			fmt.Fprintf(ctx.Stderr, "voiceime: %v\n", err)
			return
		}
		emit("dictation_on", map[string]any{"previous": previous})
	}

	runCtx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// The daemon cannot be told to forget a shortcut (it aborts on an empty key
	// list), so its registration outlives this process. Saying so is better than
	// leaving the user wondering why the key is still swallowed.
	defer func() {
		emit("log", map[string]any{"message": "listener stopped; " + hotkeySpec(ctx, spec) + " released"})
	}()

	if err := ctx.Emit("hotkey run", map[string]any{
		"listening":     true,
		"shortcut":      hotkeySpec(ctx, spec),
		"engine":        target,
		"address":       address,
		"keyval":        key.Keyval,
		"modifiers":     key.State,
		"shortcut_type": ctx.Config.HotkeyShortcutType,
		"semantics":     "tap to toggle",
		"events":        "one JSON object per event on stdout",
	}); err != nil {
		return err
	}

	deliver := func(event ibus.ShortcutEvent) {
		if logEvents {
			logf("event type=%d pressed=%v backward=%v", event.Type, event.Pressed, event.Backward)
		}
		// The property is a single slot, so another consumer's events (the panel's
		// emoji shortcut, for one) would arrive here too. Only ours is acted on.
		if event.Type != byte(ctx.Config.HotkeyShortcutType) {
			return
		}

		engineMu.Lock()
		defer engineMu.Unlock()

		if event.Pressed {
			// The daemon reports every auto-repeat tick as a press, so a held key
			// must not toggle repeatedly: only the first press of a hold counts.
			if held {
				return
			}
			held = true
			toggle()
			return
		}
		held = false
	}

	// Close is what releases the user's key, and it can fail: the daemon aborts on
	// an empty key list, so the listener hands the slot to an inert combination
	// instead, and that write is the only thing standing between the user and a
	// key that stays swallowed. Reporting the failure - with the command that
	// clears the registration outright - is the difference between a confusing
	// bug and a one-line fix.
	listenErr := listener.Listen(runCtx, deliver)
	listener.Close()
	if closeErr := listener.CloseError(); closeErr != nil {
		emit("log", map[string]any{"message": fmt.Sprintf(
			"could not release %s: %v; `ibus restart` clears the registration", hotkeySpec(ctx, spec), closeErr)})
	}
	return listenErr
}

// servicePath is the systemd --user unit that keeps the listener alive across
// logins, so a user does not have to start it by hand after every boot.
func servicePath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "systemd", "user", "voiceime-hotkey.service")
}

func installHotkeyService(ctx *Context, spec string) error {
	binary, err := os.Executable()
	if err != nil {
		return FailureError("cannot locate this binary: %v", err)
	}
	path := servicePath()
	if path == "" {
		return FailureError("cannot determine where to write the unit file")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return FailureError("%v", err)
	}

	unit := fmt.Sprintf(`[Unit]
Description=voiceime push-to-talk listener
Documentation=file://%s

[Service]
Type=simple
ExecStart=%s hotkey run --keys %s
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
`, filepath.Join(filepath.Dir(binary), "..", "docs", "AGENT.md"), binary, shellQuote(hotkeySpec(ctx, spec)))
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		return FailureError("%v", err)
	}

	commands := []string{"systemctl --user daemon-reload", "systemctl --user enable --now voiceime-hotkey.service"}
	if !ctx.JSON {
		ctx.Print("wrote %s", path)
		for _, command := range commands {
			ctx.Print("  %s", command)
		}
	}
	applied := false
	if _, err := exec.LookPath("systemctl"); err == nil {
		for _, command := range commands {
			if err := runCommand(command); err != nil {
				// Not fatal: a user may run this outside a systemd session, and the
				// unit is still correct for the next login.
				ctx.Print("could not run `%s`: %v", command, err)
				continue
			}
			applied = true
		}
	}
	// The IBus registration only fires where a component honours it, which in
	// this project means Wayland: there the compositor grabs the key and echoes
	// the event back. On X11 nothing grabs it - measured: ibus-daemon has no
	// display connection at all, and the desktop's ibus integration never reads
	// the property - so an X11 session also gets a GNOME keybinding, because
	// gsd-media-keys does grab keys there. Both are installed on X11: the unit is
	// harmless and keeps working if the session ever moves to Wayland.
	session := sessionType()
	mechanisms := []string{"ibus-global-shortcut"}
	var keybinding any
	switch session {
	case "x11":
		mechanisms = append(mechanisms, "gnome-keybinding")
		keybinding = installGnomeKeybinding(ctx, hotkeySpec(ctx, spec), binary)
	case "wayland":
		// Converge rather than only add: a machine that once ran X11 may still
		// have the GNOME keybinding, and on Wayland the IBus shortcut works too,
		// so two consumers would fight over one key.
		removeStaleGnomeKeybinding(ctx)
	}

	return ctx.Emit("hotkey install", map[string]any{
		"unit":          path,
		"commands":      commands,
		"applied":       applied,
		"keys":          hotkeySpec(ctx, spec),
		"session":       session,
		"mechanisms":    mechanisms,
		"gnome_binding": keybinding,
	})
}

// removeStaleGnomeKeybinding clears a keybinding a previous X11 install left
// behind, so switching a machine to Wayland does not leave two things grabbing
// the shortcut.
func removeStaleGnomeKeybinding(ctx *Context) {
	if !gnome.Available() {
		return
	}
	registered, _, _, err := gnome.Status(gnome.KeybindingPath)
	if err != nil || !registered {
		return
	}
	if err := gnome.Remove(gnome.KeybindingPath); err == nil {
		ctx.Print("removed the leftover GNOME keybinding; the IBus shortcut works on this session")
	}
}

// sessionType names the display server the command runs under. It is the one
// thing this deployment branches on, because it decides which component can grab
// the global shortcut. XDG_SESSION_TYPE is authoritative when the session set
// it; the two display variables cover a shell that did not inherit it (a plain
// login shell, or another user's environment).
func sessionType() string {
	if value := strings.ToLower(strings.TrimSpace(os.Getenv("XDG_SESSION_TYPE"))); value != "" {
		return value
	}
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		return "wayland"
	}
	if os.Getenv("DISPLAY") != "" {
		return "x11"
	}
	return ""
}

// installGnomeKeybinding adds the X11-only trigger and returns what happened, so
// the JSON result says whether the shortcut will actually fire rather than
// leaving a silent ineffective install.
func installGnomeKeybinding(ctx *Context, spec, binary string) map[string]any {
	info := map[string]any{"path": gnome.KeybindingPath, "installed": false}
	accelerator, err := ibus.GTKAccelerator(spec)
	if err != nil {
		info["error"] = fmt.Sprintf("cannot express %q as a GNOME accelerator: %v", spec, err)
		ctx.Print("note: no GNOME keybinding (%v)", err)
		return info
	}
	info["binding"] = accelerator
	if !gnome.Available() {
		info["error"] = "GNOME custom keybindings are not available (gsettings or the schema is missing)"
		ctx.Print("note: no GNOME keybinding; on X11 the shortcut will not fire")
		return info
	}
	// The keybinding's command is split by a shell-style parser, so the binary
	// path (which can contain spaces) is quoted with the same helper the unit
	// uses.
	command := shellQuote(binary) + " hotkey toggle"
	if err := gnome.Install(gnome.KeybindingPath, "voiceime dictation", command, accelerator); err != nil {
		info["error"] = err.Error()
		ctx.Print("note: could not add the GNOME keybinding: %v", err)
		return info
	}
	info["installed"] = true
	info["command"] = command
	ctx.Print("bound %s to `%s` through GNOME (the IBus shortcut does not fire on X11)", accelerator, command)
	return info
}

// toggleHotkey turns dictation on or off in one shot. A platform keybinding runs
// a command on a key press, so the persistent-listener design cannot be reused
// there; the engine the user was on before is remembered so the second press
// returns to it.
func toggleHotkey(ctx *Context) error {
	address, err := ibus.ResolveAddress(ibus.Ping)
	if err != nil {
		return PreconditionError("ibus restart", "%v", err)
	}
	target := ctx.Config.IBusEngineName
	current, err := ibus.CurrentEngine(address)
	if err != nil {
		return FailureError("%v", err)
	}
	statePath := filepath.Join(config.CacheDir(), "hotkey-previous")

	if current == target {
		previous := firstLineOfFile(statePath)
		if previous == "" {
			previous = "xkb:us::eng"
		}
		if err := ibus.SetEngine(address, previous); err != nil {
			return FailureError("%v", err)
		}
		_ = os.Remove(statePath)
		ctx.Print("dictation off; back to %s", previous)
		return ctx.Emit("hotkey toggle", map[string]any{
			"dictation": false, "engine": previous, "previous": previous,
		})
	}

	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err == nil {
		_ = os.WriteFile(statePath, []byte(current+"\n"), 0o644)
	}
	if err := ibus.SetEngine(address, target); err != nil {
		return FailureError("%v", err)
	}
	ctx.Print("dictation on (was %s)", current)
	return ctx.Emit("hotkey toggle", map[string]any{
		"dictation": true, "engine": target, "previous": current,
	})
}

// firstLineOfFile reads the engine the toggle should return to. A missing file is
// normal: the first press on a machine that has never dictated has no history.
func firstLineOfFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
}

func shellQuote(value string) string {
	if !strings.ContainsAny(value, " \t\"'") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func uninstallHotkeyService(ctx *Context) error {
	// The GNOME keybinding goes first: on X11 it is the one that actually grabs
	// the key, so removing it is what stops the shortcut firing, and doing it
	// before the unit means a failure here cannot leave a key wired to a command
	// that no longer exists.
	keybindingRemoved := false
	if gnome.Available() {
		if err := gnome.Remove(gnome.KeybindingPath); err == nil {
			keybindingRemoved = true
		}
	}
	// The restart is part of the uninstall, not a nicety: the daemon keeps the
	// shortcut registration alive (it cannot be cleared while it runs), so the key
	// stays swallowed until it is restarted.
	commands := []string{
		"systemctl --user disable --now voiceime-hotkey.service",
		"ibus restart",
	}
	for _, command := range commands {
		if _, err := exec.LookPath("systemctl"); err == nil {
			_ = runCommand(command)
		}
	}
	path := servicePath()
	if path != "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return FailureError("%v", err)
		}
		_ = runCommand("systemctl --user daemon-reload")
	}
	return ctx.Emit("hotkey uninstall", map[string]any{
		"unit":                  path,
		"removed":               true,
		"gnome_binding_removed": keybindingRemoved,
	})
}

func hotkeyStatus(ctx *Context, spec string) error {
	spec = hotkeySpec(ctx, spec)
	key, err := ibus.ParseShortcut(spec)
	if err != nil {
		return UsageError("%v", err)
	}
	state := map[string]any{
		"shortcut":      spec,
		"keyval":        key.Keyval,
		"modifiers":     key.State,
		"engine":        ctx.Config.IBusEngineName,
		"shortcut_type": ctx.Config.HotkeyShortcutType,
		"unit":          servicePath(),
	}
	if path := servicePath(); path != "" {
		if _, err := os.Stat(path); err == nil {
			state["unit_installed"] = true
		}
	}
	if output, err := probe("systemctl", "--user", "is-active", "voiceime-hotkey.service"); err == nil {
		state["service_active"] = firstLine(output) == "active"
	}
	if current, err := probe("ibus", "engine"); err == nil {
		state["current_engine"] = firstLine(current)
	}

	// Which mechanism can actually fire is a property of the session, so it is
	// reported rather than left for the user to discover: on X11 the IBus
	// registration is stored but never grabbed, and the GNOME keybinding is the
	// one that works.
	session := sessionType()
	state["session"] = session
	mechanisms := []string{"ibus-global-shortcut"}
	if registered, binding, command, err := gnome.Status(gnome.KeybindingPath); err == nil && registered {
		mechanisms = append(mechanisms, "gnome-keybinding")
		state["gnome_binding"] = map[string]any{
			"path":    gnome.KeybindingPath,
			"binding": binding,
			"command": command,
		}
	}
	state["mechanisms"] = mechanisms

	ctx.Print("shortcut: %s (keyval=%d modifiers=%d)", spec, key.Keyval, key.State)
	ctx.Print("engine while dictating: %s", ctx.Config.IBusEngineName)
	ctx.Print("session: %s", session)
	if active, _ := state["service_active"].(bool); active {
		ctx.Print("listener: active")
	} else {
		ctx.Print("listener: not running (voiceime hotkey install)")
	}
	if session == "x11" {
		if _, ok := state["gnome_binding"]; ok {
			ctx.Print("trigger: GNOME keybinding - the IBus shortcut is not grabbed on X11")
		} else {
			ctx.Print("trigger: none - on X11 the IBus shortcut is inert; run `voiceime hotkey install`")
		}
	} else {
		ctx.Print("trigger: IBus global shortcut")
	}
	ctx.Print("note: IBus keeps one global shortcut registration; another consumer's")
	ctx.Print("      registration does not include ours, and vice versa.")
	return ctx.Emit("hotkey status", state)
}
