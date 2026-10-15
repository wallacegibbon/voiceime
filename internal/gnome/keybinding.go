// Package gnome manages a GNOME custom keybinding through gsettings.
//
// It exists for one measured reason. The global shortcut voiceime registers
// through IBus (org.freedesktop.IBus.GlobalShortcutKeys) is honoured on Wayland
// by the compositor, but on X11 nothing grabs the key: a second client can take
// a passive grab on it for free, and ibus-daemon itself has no display
// connection at all, so the property is stored and never fires. GNOME's settings
// daemon (gsd-media-keys) does grab custom keybindings on X11, so an X11 session
// gets one of these as well - it runs `voiceime hotkey toggle`.
package gnome

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// KeybindingPath is the dconf path of the binding voiceime owns. It is fixed
// rather than generated so that install, status and uninstall agree without any
// state of their own.
const KeybindingPath = "/org/gnome/settings-daemon/plugins/media-keys/custom-keybindings/voiceime/"

const (
	mediaKeysSchema     = "org.gnome.settings-daemon.plugins.media-keys"
	customBindingSchema = mediaKeysSchema + ".custom-keybinding"
)

// Available reports whether this desktop exposes the custom-keybinding schema.
// A false answer is not an error: a non-GNOME session simply gets no extra
// keybinding, and the caller decides whether that matters.
func Available() bool {
	if _, err := exec.LookPath("gsettings"); err != nil {
		return false
	}
	_, err := run("gsettings", "list-keys", customBindingSchema+":"+KeybindingPath)
	return err == nil
}

// Install writes the binding and ensures its path is in the daemon's list of
// custom keybindings. Re-running replaces the values and never duplicates the
// path, which is what keeps `voiceime hotkey install` idempotent.
func Install(path, name, command, binding string) error {
	schema := customBindingSchema + ":" + path
	values := []struct{ key, value string }{
		{"name", name},
		{"command", command},
		{"binding", binding},
	}
	for _, entry := range values {
		if err := run0("gsettings", "set", schema, entry.key, quote(entry.value)); err != nil {
			return err
		}
	}

	paths, err := list()
	if err != nil {
		return err
	}
	for _, existing := range paths {
		if existing == path {
			return nil
		}
	}
	return setList(append(paths, path))
}

// Remove drops the path from the list and clears the values behind it, so a
// stale entry can neither be listed nor fire. Clearing is best-effort: once the
// path is out of the list the daemon ignores it, which is the part that matters.
func Remove(path string) error {
	paths, err := list()
	if err != nil {
		return err
	}
	kept := make([]string, 0, len(paths))
	for _, existing := range paths {
		if existing != path {
			kept = append(kept, existing)
		}
	}
	if len(kept) != len(paths) {
		if err := setList(kept); err != nil {
			return err
		}
	}
	schema := customBindingSchema + ":" + path
	for _, key := range []string{"name", "command", "binding"} {
		_ = run0("gsettings", "reset", schema, key)
	}
	return nil
}

// Status reports whether the path is registered and, if so, what it is bound to.
func Status(path string) (registered bool, binding, command string, err error) {
	paths, err := list()
	if err != nil {
		return false, "", "", err
	}
	found := false
	for _, existing := range paths {
		if existing == path {
			found = true
		}
	}
	if !found {
		return false, "", "", nil
	}
	schema := customBindingSchema + ":" + path
	binding, _ = get(schema, "binding")
	command, _ = get(schema, "command")
	return true, binding, command, nil
}

// list reads the daemon's custom-keybinding paths.
func list() ([]string, error) {
	raw, err := run("gsettings", "get", mediaKeysSchema, "custom-keybindings")
	if err != nil {
		return nil, err
	}
	return parseList(raw), nil
}

func setList(paths []string) error {
	quoted := make([]string, len(paths))
	for i, path := range paths {
		quoted[i] = quote(path)
	}
	return run0("gsettings", "set", mediaKeysSchema, "custom-keybindings", "["+strings.Join(quoted, ", ")+"]")
}

func get(schema, key string) (string, error) {
	raw, err := run("gsettings", "get", schema, key)
	if err != nil {
		return "", err
	}
	return unquote(raw), nil
}

// parseList pulls the strings out of a GVariant array-of-strings, which is what
// `gsettings get` prints: `['/a/', '/b/']`, or `@as []` when empty. A full
// GVariant parser would be a dependency for a format that only ever carries
// dconf paths.
func parseList(raw string) []string {
	raw = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(raw), "@as "))
	raw = strings.TrimSpace(raw)
	var out []string
	for {
		start := strings.Index(raw, "'")
		if start < 0 {
			return out
		}
		end := strings.Index(raw[start+1:], "'")
		if end < 0 {
			return out
		}
		out = append(out, raw[start+1:start+1+end])
		raw = raw[start+1+end+1:]
	}
}

func quote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `\'`) + "'"
}

func unquote(value string) string {
	if len(value) >= 2 && strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'") {
		return strings.ReplaceAll(value[1:len(value)-1], `\'`, "'")
	}
	return value
}

func run(name string, args ...string) (string, error) {
	command := exec.Command(name, args...)
	out, err := command.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(exit.Stderr) > 0 {
			return "", fmt.Errorf("%s %s: %s", name, strings.Join(args, " "), strings.TrimSpace(string(exit.Stderr)))
		}
		return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

func run0(name string, args ...string) error {
	_, err := run(name, args...)
	return err
}
