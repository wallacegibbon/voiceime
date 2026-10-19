package ibus

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// An installed component has to survive a round trip.
//
// `config apply` reads the installed file, changes the engine name the configuration owns,
// and writes it back. Anything the reader loses is silently dropped from the file IBus
// reads next - the exec line above all, which would repoint the engine at whatever binary
// ran apply, and the symbol, which was stored as an XML entity and would have been
// double-escaped on the way out ("&amp;#x1F3A4;").
func TestComponentRoundTripsThroughTheInstalledFile(t *testing.T) {
	cfg := config.Default()
	component := DefaultComponent(cfg, "/home/u/.local/bin/voiceime ibus-engine --ibus")

	dir := t.TempDir()
	path := filepath.Join(dir, "voiceime.xml")
	if err := os.WriteFile(path, []byte(component.XML()), 0o644); err != nil {
		t.Fatal(err)
	}

	read, err := ReadComponent(path)
	if err != nil {
		t.Fatalf("ReadComponent: %v", err)
	}
	if read != component {
		t.Errorf("round trip changed the component:\n got %+v\nwant %+v", read, component)
	}
	if read.XML() != component.XML() {
		t.Errorf("re-rendering an unchanged component produced different XML:\n%s", read.XML())
	}
	if read.Symbol != "🎤" {
		t.Errorf("symbol = %q, want the character the file carries", read.Symbol)
	}
}

// A file with no engine registers nothing, which is a broken install rather than an empty
// one - `config apply` must not "fix" it into something that looks fine.
func TestReadComponentRejectsAFileWithoutAnEngine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "voiceime.xml")
	body := `<?xml version="1.0" encoding="utf-8"?>
<component>
	<name>org.freedesktop.IBus.Voiceime</name>
	<exec>voiceime ibus-engine --ibus</exec>
</component>
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadComponent(path); err == nil {
		t.Fatal("ReadComponent accepted a component with no engine")
	}
}

// A component written by an older version is healed on the way through.
//
// <symbol> was stored as the entity and escaped, so the installed file carries the
// literal text "&#x1F3A4;" and IBus has been drawing that instead of a microphone. Read
// decodes it, which makes the re-render differ and is what lets `config apply` repair the
// file rather than calling it unchanged.
func TestReadComponentHealsAnEscapedSymbol(t *testing.T) {
	path := filepath.Join(t.TempDir(), "voiceime.xml")
	body := `<?xml version="1.0" encoding="utf-8"?>
<component>
	<name>org.freedesktop.IBus.Voiceime</name>
	<description>Voiceime speech input method</description>
	<exec>/home/u/.local/bin/voiceime ibus-engine --ibus</exec>
	<version>0.1.0</version>
	<author>voiceime</author>
	<license>GPL</license>
	<homepage>https://github.com/wallacegibbon/voiceime</homepage>
	<textdomain>voiceime</textdomain>
	<engines>
		<engine>
			<name>voiceime</name>
			<language>zh</language>
			<license>GPL</license>
			<author>voiceime</author>
			<layout>default</layout>
			<longname>Voiceime (speech)</longname>
			<description>Dictate into any application</description>
			<rank>1</rank>
			<symbol>&amp;#x1F3A4;</symbol>
		</engine>
	</engines>
</component>
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	component, err := ReadComponent(path)
	if err != nil {
		t.Fatalf("ReadComponent: %v", err)
	}
	if component.Symbol != "🎤" {
		t.Fatalf("symbol = %q, want the decoded character", component.Symbol)
	}
	if !strings.Contains(component.XML(), "<symbol>🎤</symbol>") {
		t.Errorf("re-rendered symbol is not the character:\n%s", component.XML())
	}
}

// Anything that is not a character reference is left exactly as it was: a symbol may be a
// name or a plain character, and rewriting those would be a change nobody asked for.
func TestDecodeCharacterReferenceLeavesOtherSymbolsAlone(t *testing.T) {
	for _, value := range []string{"🎤", "microphone", "&#xZZ;", "&#;", "&amp;", ""} {
		if got := decodeCharacterReference(value); got != value {
			t.Errorf("decodeCharacterReference(%q) = %q, want it unchanged", value, got)
		}
	}
	for value, want := range map[string]string{
		"&#x1F3A4;": "🎤",
		"&#127908;": "🎤",
	} {
		if got := decodeCharacterReference(value); got != want {
			t.Errorf("decodeCharacterReference(%q) = %q, want %q", value, got, want)
		}
	}
}
