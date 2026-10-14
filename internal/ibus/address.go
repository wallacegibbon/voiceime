// Package ibus is voiceime's text-delivery layer: an IBus engine.
//
// IBus is the input-method framework GNOME already runs, so becoming an engine is
// the only way to put text into the focused application without privileges. The
// protocol is not documented for third-party implementers, so it was established
// empirically against a live daemon and is recorded here in full; see
// docs/ibus-protocol.md for the transcript of that investigation.
//
// Three facts shape this implementation, all verified:
//
//  1. ibus-daemon builds its engine registry from component XML under
//     $IBUS_COMPONENT_PATH (or /usr/share/ibus/component). It does *not* scan
//     ~/.local/share/ibus/component, and setting IBUS_COMPONENT_PATH *replaces*
//     the default path, so an install must list both directories.
//  2. The daemon spawns the component's <exec> when the engine is activated, and
//     the spawned process must own the bus name from the component <name> and
//     expose a factory at /org/freedesktop/IBus/Factory.
//  3. A malformed component passed to RegisterComponent makes the daemon abort
//     (reproduced in a sandbox). Text delivery therefore uses the file-based
//     route only and never hand-marshals a component at runtime.
package ibus

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DefaultComponentName is the well-known bus name the engine process requests.
// IBus uses the component <name> as the bus name, which is why this must match
// the XML.
const DefaultComponentName = "org.freedesktop.IBus.Voiceime"

// FactoryPath is where the daemon looks for CreateEngine on the engine process.
const FactoryPath = "/org/freedesktop/IBus/Factory"

// EnginePathPrefix is where an engine instance is exported. The daemon passes the
// object path it got from CreateEngine, so the exact numbering is ours to choose;
// this mirrors libibus's /org/freedesktop/IBus/Engine/<n>.
const EnginePathPrefix = "/org/freedesktop/IBus/Engine"

// ResolveAddress finds the address of the IBus daemon's private bus.
//
// Three sources, in order of authority:
//
//  1. IBUS_ADDRESS, which libibus itself honours first.
//  2. The registry file the daemon writes at
//     $XDG_CONFIG_HOME/ibus/bus/<machine-id>-<display>. Every candidate file is
//     pinged, because the daemon writes a new one on each restart and stale files
//     outlive it: connecting to a stale address is the failure mode that made this
//     function exist.
//  3. Nothing else: the session-bus GetAddress route is deprecated upstream and
//     the file route is what the daemon's own clients use.
func ResolveAddress(probe func(address string) error) (string, error) {
	if address := strings.TrimSpace(os.Getenv("IBUS_ADDRESS")); address != "" {
		return address, nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate the home directory to find the IBus bus: %w", err)
	}
	registryDir := filepath.Join(configDir(home), "ibus", "bus")
	entries, err := os.ReadDir(registryDir)
	if err != nil {
		return "", fmt.Errorf("no IBUS_ADDRESS is set and %s cannot be read: %w", registryDir, err)
	}

	// Prefer the file for the current display, but fall back to any that answers:
	// the display-encoded name is an implementation detail, and a live daemon is
	// the thing actually being looked for.
	var preferred, others []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		address := readRegistryAddress(filepath.Join(registryDir, entry.Name()))
		if address == "" {
			continue
		}
		if displayMatches(entry.Name()) {
			preferred = append(preferred, address)
		} else {
			others = append(others, address)
		}
	}
	sort.Strings(preferred)
	sort.Strings(others)

	for _, address := range append(preferred, others...) {
		if probe == nil {
			return address, nil
		}
		if err := probe(address); err == nil {
			return address, nil
		}
	}
	return "", fmt.Errorf("no IBus daemon answered: %d registry file(s) in %s, none reachable", len(preferred)+len(others), registryDir)
}

// readRegistryAddress parses the IBUS_ADDRESS line out of a registry file. The
// file is plain text with comments; see docs/ibus-protocol.md.
func readRegistryAddress(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if value, found := strings.CutPrefix(line, "IBUS_ADDRESS="); found {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// displayMatches reports whether a registry file name refers to this session.
// The daemon names them <machine-id>-<display>, and the display half is either the
// X11 display or WAYLAND_DISPLAY, so either may match.
func displayMatches(name string) bool {
	suffix := strings.ToLower(name)
	for _, display := range []string{os.Getenv("WAYLAND_DISPLAY"), os.Getenv("DISPLAY")} {
		if display == "" {
			continue
		}
		normalized := strings.ToLower(strings.NewReplacer(":", "", ".", "_").Replace(display))
		if strings.Contains(suffix, normalized) {
			return true
		}
	}
	return false
}

// configDir mirrors the XDG lookup for the IBus registry, which lives under the
// config directory rather than voiceime's own.
func configDir(home string) string {
	if value := os.Getenv("XDG_CONFIG_HOME"); value != "" {
		return value
	}
	return filepath.Join(home, ".config")
}
