package gnome

import (
	"os/exec"
	"strings"
)

// GNOME's input-source list, and why voiceime has to appear in it.
const inputSourcesSchema = "org.gnome.desktop.input-sources"

// InputSource is one entry of that list: a kind and an id - a layout name for "xkb", an
// IBus engine name for "ibus".
type InputSource struct {
	Type string
	ID   string
}

// String is the pair in the spelling the dconf value uses, for messages.
func (s InputSource) String() string { return "('" + s.Type + "', '" + s.ID + "')" }

// InputSourcesAvailable reports whether this desktop carries the input-source schema. A
// false answer is not an error: a non-GNOME session simply has no list to appear in.
func InputSourcesAvailable() bool {
	if _, err := exec.LookPath("gsettings"); err != nil {
		return false
	}
	_, err := run("gsettings", "list-keys", inputSourcesSchema)
	return err == nil
}

// InputSources reads the configured list.
func InputSources() ([]InputSource, error) {
	raw, err := run("gsettings", "get", inputSourcesSchema, "sources")
	if err != nil {
		return nil, err
	}
	return parseSources(raw), nil
}

// HasInputSource reports whether source is configured.
func HasInputSource(source InputSource) (bool, error) {
	sources, err := InputSources()
	if err != nil {
		return false, err
	}
	return containsSource(sources, source), nil
}

func AddInputSource(source InputSource) (bool, error) {
	sources, err := InputSources()
	if err != nil {
		return false, err
	}
	if containsSource(sources, source) {
		return false, nil
	}
	return true, setInputSources(append(sources, source))
}

// RemoveInputSource drops source and reports whether it was there.
func RemoveInputSource(source InputSource) (bool, error) {
	sources, err := InputSources()
	if err != nil {
		return false, err
	}
	kept := make([]InputSource, 0, len(sources))
	for _, existing := range sources {
		if existing != source {
			kept = append(kept, existing)
		}
	}
	if len(kept) == len(sources) {
		return false, nil
	}
	return true, setInputSources(kept)
}

func containsSource(sources []InputSource, source InputSource) bool {
	for _, existing := range sources {
		if existing == source {
			return true
		}
	}
	return false
}

func parseSources(raw string) []InputSource {
	fields := parseList(raw)
	sources := make([]InputSource, 0, len(fields)/2)
	for index := 0; index+1 < len(fields); index += 2 {
		sources = append(sources, InputSource{Type: fields[index], ID: fields[index+1]})
	}
	return sources
}

func setInputSources(sources []InputSource) error {
	value := "@a(ss) []"
	if len(sources) > 0 {
		items := make([]string, len(sources))
		for index, source := range sources {
			items[index] = "(" + quote(source.Type) + ", " + quote(source.ID) + ")"
		}
		value = "[" + strings.Join(items, ", ") + "]"
	}
	return run0("gsettings", "set", inputSourcesSchema, "sources", value)
}
