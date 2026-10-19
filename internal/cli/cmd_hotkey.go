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
	"time"

	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/gnome"
	"github.com/wallacegibbon/voiceime/internal/ibus"
)

// cmdHotkey runs the dictation listener: tap the configured key to start
// dictating, tap it again to return to whatever input method was selected
// before. On Wayland the listener receives the key itself; on X11 a GNOME
// keybinding runs `hotkey toggle` instead, because the IBus registration is not
// grabbed there (see sessionType and docs/DEPLOY.md).
func cmdHotkey() *Command {
	var (
		spec      string
		quiet     bool
		install   bool
		logEvents bool
	)
	return &Command{
		Name:    "hotkey",
		Summary: "Listen for a global shortcut and toggle dictation when it is tapped",
		Usage:   "voiceime hotkey <run|toggle|install|uninstall|status> [--keys <combo>]",
		Flags: func(set *flag.FlagSet) {
			set.StringVar(&spec, "keys", "", "shortcut to tap, e.g. <Control>apostrophe (default: from config)")
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
		held     bool
	)

	toggle := func() {
		current, err := ibus.CurrentEngine(address)
		if err != nil {
			fmt.Fprintf(ctx.Stderr, "voiceime: %v\n", err)
			return
		}
		if current == target {
			// The same record the one-shot toggle reads (D34): one fact, one place.
			previous := previousEngine()
			if previous == "" {
				logf("already dictating, but the engine to return to is not recorded; not switching")
				return
			}
			if err := ibus.SetEngine(address, previous); err != nil {
				fmt.Fprintf(ctx.Stderr, "voiceime: %v\n", err)
				return
			}
			emit("dictation_off", map[string]any{"previous": previous})
			forgetPreviousEngine()
			return
		}
		rememberPreviousEngine(current)
		if err := ibus.SetEngine(address, target); err != nil {
			fmt.Fprintf(ctx.Stderr, "voiceime: %v\n", err)
			return
		}
		emit("dictation_on", map[string]any{"previous": current})
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

// hotkeyUnit is the systemd user unit that keeps the listener alive across logins.
//
// It names the configuration file - a unit has no working directory of its own, so a
// relative --config would be resolved somewhere else - and it names the key only when
// the caller asked for one. Baking the *configured* key in here made `config set
// hotkey` followed by `config apply` a no-op: apply restarts the unit, the unit passes
// the key it was installed with, and `hotkey run` prefers an explicit --keys over the
// file. Without the flag the listener reads the configuration, which is what the one
// command that applies a changed setting promises.
func hotkeyUnit(binary, configPath, keys string) string {
	execStart := shellQuote(binary) + " hotkey run --config " + shellQuote(configPath)
	if keys != "" {
		execStart += " --keys " + shellQuote(keys)
	}
	return fmt.Sprintf(`[Unit]
Description=voiceime dictation shortcut listener
Documentation=file://%s

[Service]
Type=simple
ExecStart=%s
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
`, filepath.Join(filepath.Dir(binary), "..", "docs", "AGENT.md"), execStart)
}

// servicePath is the systemd --user unit that keeps the listener alive across
// logins, so a user does not have to start it by hand after every boot.
func servicePath() string {
	dir := config.UserUnitDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, "voiceime-hotkey.service")
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

	// Documentation= names the agent guide beside the binary: `make install` puts
	// the docs at <prefix>/docs/, and a checkout run finds <repo>/docs/. A path
	// with nothing behind it is a dead link systemd cannot report.
	unit := hotkeyUnit(binary, absoluteConfigPath(ctx), spec)
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
	// The IBus registration is honoured on Wayland, where the compositor grabs the
	// key and echoes the event back to the listener. On X11 the desktop's ibus
	// integration never reads the property, so the key reaches the focused
	// application instead; gsd-media-keys does grab keys there, so an X11 session
	// also gets a GNOME custom keybinding that runs `voiceime hotkey toggle`. The
	// unit is installed either way - it is inert on X11 and keeps working if the
	// session ever moves to Wayland.
	session := sessionType()
	mechanisms := []string{"ibus-global-shortcut"}
	var keybinding any
	var warning string
	switch session {
	case "x11":
		mechanisms = append(mechanisms, "gnome-keybinding")
		keybinding = installGnomeKeybinding(ctx, hotkeySpec(ctx, spec), binary)
	case "wayland":
		// Converge rather than only add: a machine that once ran X11 may still
		// have the GNOME keybinding, and on Wayland the IBus shortcut works too,
		// so two consumers would fight over one key.
		removeStaleGnomeKeybinding(ctx)
	default:
		// Not a display server: a plain tty, an ssh session, cron. Only the IBus
		// shortcut is installed, and on X11 it is inert, so a trigger installed
		// from here may simply do nothing. Name the detected session and the fix
		// rather than reporting a successful install of nothing.
		warning = fmt.Sprintf("session type %q is not a display server; on X11 the IBus shortcut is inert, so the trigger cannot be confirmed. Run `voiceime hotkey install` from a terminal inside the graphical session", session)
		ctx.Print("note: %s", warning)
	}

	result := map[string]any{
		"unit":          path,
		"commands":      commands,
		"applied":       applied,
		"keys":          hotkeySpec(ctx, spec),
		"session":       session,
		"mechanisms":    mechanisms,
		"gnome_binding": keybinding,
	}
	if warning != "" {
		result["warning"] = warning
	}
	return ctx.Emit("hotkey install", result)
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
// the global shortcut.
//
// Only `x11` and `wayland` are display servers, so only those are taken from
// XDG_SESSION_TYPE. That variable can also read `tty` - an ssh or console shell -
// or be unset, and treating `tty` as a session type is how an install run over
// ssh chose neither trigger and reported success. When it is not a display
// server the display variables are the better evidence; the raw value is
// returned when nothing says otherwise, so a caller can report it instead of
// guessing.
func sessionType() string {
	declared := strings.ToLower(strings.TrimSpace(os.Getenv("XDG_SESSION_TYPE")))
	switch declared {
	case "x11", "wayland":
		return declared
	}
	if os.Getenv("WAYLAND_DISPLAY") != "" {
		return "wayland"
	}
	if os.Getenv("DISPLAY") != "" {
		return "x11"
	}
	return declared
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

// previousEnginePath is the one record of "the engine to return to".
//
// The listener kept it in memory and the one-shot toggle kept it in a file, so after a
// reboot one of them knew and the other did not - and the one that did not used to guess
// `xkb:us::eng`. One fact, one place (D34): the file, which both read and write.
func previousEnginePath() string { return filepath.Join(config.CacheDir(), "hotkey-previous") }

// rememberPreviousEngine records the engine dictation was started from.
func rememberPreviousEngine(name string) {
	if strings.TrimSpace(name) == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(previousEnginePath()), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(previousEnginePath(), []byte(name+"\n"), 0o644)
}

// previousEngine is the engine to return to, or "" when none is recorded.
func previousEngine() string { return firstLineOfFile(previousEnginePath()) }

// forgetPreviousEngine clears the record, so the next dictation stores its own start.
func forgetPreviousEngine() { _ = os.Remove(previousEnginePath()) }

// repeatGuard is how long after a press a second `hotkey toggle` is treated as
// the key repeating rather than a new press.
//
// The listener guards a hold with its `held` flag (D13), but the X11 trigger is a
// GNOME keybinding that runs this command once per key event - including every
// auto-repeat tick of a held key - so the guard has to live in the file system.
// It must exceed the X11 auto-repeat delay: a held key emits its *first* repeat
// only after that delay, so a shorter window would let that repeat through and
// toggle straight back to where it started. The reference delay is 500 ms (the
// xset default is 660 ms), so the window sits above both. The cost is that two
// deliberate presses closer than the window collapse into one, which for a
// tap-to-toggle key normally seconds apart is not a loss.
const repeatGuard = 700 * time.Millisecond

// repeatGuardPath records the time of the press that was last acted on. Fixed
// rather than generated, so repeat guard and toggle agree without any state.
func repeatGuardPath() string {
	return filepath.Join(config.CacheDir(), "hotkey-last-press")
}

// suppressRepeat reports whether this invocation is an auto-repeat of a press
// already acted on, refreshing the marker so a continuous hold stays one press
// until it ends. A missing or unreadable marker means "act": the first press of a
// hold, and every press on a machine that does not repeat, must get through. The
// marker's mtime is the clock - one file operation, and no format to parse.
func suppressRepeat(path string, now time.Time) bool {
	if info, err := os.Stat(path); err == nil {
		if age := now.Sub(info.ModTime()); age >= 0 && age < repeatGuard {
			_ = os.Chtimes(path, now, now)
			return true
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return false
	}
	_ = os.WriteFile(path, []byte(now.UTC().Format(time.RFC3339Nano)), 0o644)
	return false
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
	// A held key repeats, and gsd-media-keys runs this command on every repeat,
	// so without the guard the engine switches once per repeat and ends up
	// wherever the parity leaves it (measured: 52 toggles for a 2s hold).
	if suppressRepeat(repeatGuardPath(), time.Now()) {
		ctx.Print("ignored: %s is repeating", hotkeySpec(ctx, ""))
		return ctx.Emit("hotkey toggle", map[string]any{
			"dictation": current == target,
			"skipped":   "key repeat",
		})
	}
	if current == target {
		previous := previousEngine()
		if previous == "" {
			// Nothing recorded to go back to. Guessing is worse than saying so: the old
			// fallback was "xkb:us::eng", which moved a user on any other layout to a US
			// keyboard without a word. The listener path knows the previous engine in
			// memory and refuses the same way (D13's "not switching" case).
			if !ctx.JSON {
				ctx.Print("dictation off: the engine to return to is not recorded")
			}
			if err := ctx.Emit("hotkey toggle", map[string]any{
				"dictation": true,
				"engine":    target,
				"switched":  false,
				"reason":    "previous engine unknown",
			}); err != nil {
				return err
			}
			return FailureError("the engine to return to is not recorded, so the input method was left on %s: switch it manually (the shortcut listener remembers it; the one-shot toggle does not across a reboot)", target)
		}
		if err := ibus.SetEngine(address, previous); err != nil {
			return FailureError("%v", err)
		}
		forgetPreviousEngine()
		ctx.Print("dictation off; back to %s", previous)
		return ctx.Emit("hotkey toggle", map[string]any{
			"dictation": false, "engine": previous, "previous": previous,
		})
	}

	rememberPreviousEngine(current)
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
	switch session {
	case "x11":
		if _, ok := state["gnome_binding"]; ok {
			ctx.Print("trigger: GNOME keybinding - the IBus shortcut is not grabbed on X11")
		} else {
			ctx.Print("trigger: none - on X11 the IBus shortcut is inert; run `voiceime hotkey install`")
		}
	case "wayland":
		ctx.Print("trigger: IBus global shortcut")
	default:
		ctx.Print("trigger: unknown - session type is %q, not a display server; the IBus", session)
		ctx.Print("         shortcut is only honoured on Wayland, so this cannot be confirmed here")
	}
	ctx.Print("note: IBus keeps one global shortcut registration; another consumer's")
	ctx.Print("      registration does not include ours, and vice versa.")
	return ctx.Emit("hotkey status", state)
}
