package gnome

import (
	"os/exec"
	"strings"
)

const inputSourcesSchema = "org.gnome.desktop.input-sources"

const (
	sourcesKey = "sources"
	// mruKey is the desktop's own ordering of what the user used last. voiceime removes
	// its entry from it when uninstalling and never writes it otherwise.
	mruKey = "mru-sources"
)

// InputSource is one entry of GNOME's input-source list: a kind and an id - a layout name
// for "xkb", an IBus engine name for "ibus".
type InputSource struct {
	Type string
	ID   string
}

func (s InputSource) String() string { return "('" + s.Type + "', '" + s.ID + "')" }

// InputSourcesAvailable reports whether this desktop carries the input-source schema. A
// false answer is not an error: a non-GNOME session has no list to appear in.
func InputSourcesAvailable() bool {
	if _, err := exec.LookPath("gsettings"); err != nil {
		return false
	}
	_, err := run("gsettings", "list-keys", inputSourcesSchema)
	return err == nil
}

// InputSources reads the configured list.
func InputSources() ([]InputSource, error) { return readSources(sourcesKey) }

// HasInputSource reports whether source is configured.
func HasInputSource(source InputSource) (bool, error) {
	sources, err := InputSources()
	if err != nil {
		return false, err
	}
	return containsSource(sources, source), nil
}

// AddInputSource appends source unless it is already there, and reports whether the list
// changed.
//
// This is the only write voiceime ever makes to the user's sources, and it is an append:
// the entries the user configured stay in their order, nothing is removed, and nothing else
// is touched - not the desktop's recent-use order, not another engine's entry.
func AddInputSource(source InputSource) (bool, error) {
	sources, err := InputSources()
	if err != nil {
		return false, err
	}
	updated, added := withSource(sources, source)
	if !added {
		return false, nil
	}
	return true, writeSources(sourcesKey, updated)
}

// RemoveInputSource drops source and reports whether it was there. Only voiceime's own
// entry is removed, from the list and from the desktop's recent-use order.
func RemoveInputSource(source InputSource) (bool, error) {
	sources, err := InputSources()
	if err != nil {
		return false, err
	}
	updated, removed := withoutSource(sources, source)
	if !removed {
		return false, nil
	}
	if err := writeSources(sourcesKey, updated); err != nil {
		return false, err
	}
	// A stale entry in the recent-use list would name a source that no longer exists.
	if mru, err := readSources(mruKey); err == nil {
		if kept, had := withoutSource(mru, source); had && len(kept) > 0 {
			_ = writeSources(mruKey, kept)
		}
	}
	return true, nil
}

// withSource appends source if it is missing, leaving every existing entry in place.
func withSource(sources []InputSource, source InputSource) ([]InputSource, bool) {
	if containsSource(sources, source) {
		return sources, false
	}
	updated := make([]InputSource, len(sources), len(sources)+1)
	copy(updated, sources)
	return append(updated, source), true
}

// withoutSource removes every copy of source, leaving the rest in order.
func withoutSource(sources []InputSource, source InputSource) ([]InputSource, bool) {
	kept := make([]InputSource, 0, len(sources))
	for _, existing := range sources {
		if existing != source {
			kept = append(kept, existing)
		}
	}
	if len(kept) == len(sources) {
		return sources, false
	}
	return kept, true
}

func containsSource(sources []InputSource, source InputSource) bool {
	for _, existing := range sources {
		if existing == source {
			return true
		}
	}
	return false
}

func readSources(key string) ([]InputSource, error) {
	raw, err := run("gsettings", "get", inputSourcesSchema, key)
	if err != nil {
		return nil, err
	}
	return parseSources(raw), nil
}

func writeSources(key string, sources []InputSource) error {
	value := "@a(ss) []"
	if len(sources) > 0 {
		items := make([]string, len(sources))
		for index, source := range sources {
			items[index] = "(" + quote(source.Type) + ", " + quote(source.ID) + ")"
		}
		value = "[" + strings.Join(items, ", ") + "]"
	}
	return run0("gsettings", "set", inputSourcesSchema, key, value)
}

// parseSources reads what `gsettings get` prints: `[('xkb', 'us'), ('ibus', 'pinyin')]`,
// or `@a(ss) []` when empty.
func parseSources(raw string) []InputSource {
	fields := parseList(raw)
	sources := make([]InputSource, 0, len(fields)/2)
	for index := 0; index+1 < len(fields); index += 2 {
		sources = append(sources, InputSource{Type: fields[index], ID: fields[index+1]})
	}
	return sources
}
