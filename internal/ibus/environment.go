package ibus

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// The variable ibus-daemon reads to find component files outside its default path.
//
// IBUS_COMPONENT_PATH *replaces* the default search path, so a value that does not list
// /usr/share/ibus/component takes every system engine away; see docs/ibus-protocol.md.
const ComponentPathVariable = "IBUS_COMPONENT_PATH"

// EnvironmentFile is where the search path has to live to survive a login.
//
// `engine install --apply` also runs `dbus-update-activation-environment --systemd`, but
// that edits the activation environment of the *running* session only: the next login
// starts ibus-daemon without it, the component directory is never scanned, and the engine
// is gone from `ibus list-engine` again. environment.d is read by the systemd user manager
// at the start of every session, and on GNOME ibus-daemon runs as a systemd user service
// (org.freedesktop.IBus.session.GNOME.service), so a file here is what makes a user-level
// install outlive the session that installed it.
func EnvironmentFile() string {
	return filepath.Join(config.EnvironmentDir(), "voiceime.conf")
}

// EnvironmentPath is the value environment.d has to carry for the user-level install.
func EnvironmentPath() string { return InstallPaths("") }

// WriteEnvironment records the search path for every future session. The file is
// voiceime's own, so it is written whole rather than merged with what is there.
func WriteEnvironment() (string, error) {
	path := EnvironmentFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	body := "# Written by `voiceime engine install --apply`; removed by `engine uninstall`.\n" +
		"# ibus-daemon does not read ~/.local/share/voiceime/component on its own, and the\n" +
		"# activation environment is per session, so the path is recorded here for every\n" +
		"# future login. docs/ibus-protocol.md has the measurements.\n" +
		ComponentPathVariable + "=" + environmentValue(EnvironmentPath()) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// RemoveEnvironment deletes the file, reporting whether there was one to delete.
func RemoveEnvironment() (bool, error) {
	err := os.Remove(EnvironmentFile())
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// EnvironmentSet reports whether environment.d already names the current search path. A
// file with an older value is not set: the engine it names is not the one installed now.
func EnvironmentSet() bool {
	raw, err := os.ReadFile(EnvironmentFile())
	if err != nil {
		return false
	}
	value, found := environmentPath(string(raw))
	return found && value == EnvironmentPath()
}

// environmentPath finds the variable in an environment.d file. Lines that are not it, and
// comments, are skipped: the file may have been edited by hand.
func environmentPath(raw string) (string, bool) {
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found || strings.TrimSpace(key) != ComponentPathVariable {
			continue
		}
		value = strings.TrimSpace(value)
		// systemd parses a quoted value with C-style escapes, which is what
		// strconv.Quote writes; an unquoted path is taken as it stands.
		if strings.HasPrefix(value, `"`) {
			if unquoted, err := strconv.Unquote(value); err == nil {
				return unquoted, true
			}
		}
		return value, true
	}
	return "", false
}

// environmentValue renders a path for environment.d. A plain path is left unquoted so the
// file stays readable; only a value that the parser would split is quoted.
func environmentValue(value string) string {
	if !strings.ContainsAny(value, " \t\"'\\") {
		return value
	}
	return strconv.Quote(value)
}
