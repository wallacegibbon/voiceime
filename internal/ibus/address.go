// Package ibus is voiceime's text-delivery layer: an IBus engine.
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
const DefaultComponentName = "org.freedesktop.IBus.Voiceime"

// FactoryPath is where the daemon looks for CreateEngine on the engine process.
const FactoryPath = "/org/freedesktop/IBus/Factory"

const EnginePathPrefix = "/org/freedesktop/IBus/Engine"

// ResolveAddress finds the address of the IBus daemon's private bus.
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

// RegistryCachePath is the daemon's cached component registry.
func RegistryCachePath() string {
	base := os.Getenv("XDG_CACHE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".cache")
	}
	return filepath.Join(base, "ibus", "bus", "registry")
}

// configDir mirrors the XDG lookup for the IBus registry, which lives under the
// config directory rather than voiceime's own.
func configDir(home string) string {
	if value := os.Getenv("XDG_CONFIG_HOME"); value != "" {
		return value
	}
	return filepath.Join(home, ".config")
}
