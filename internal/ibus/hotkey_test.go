package ibus

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wallacegibbon/voiceime/internal/config"
)

func TestParseShortcutSpellings(t *testing.T) {
	cases := []struct {
		spec   string
		keyval uint32
		state  uint32
	}{
		{"<Control>apostrophe", 0x27, ModControl},
		{"Control+apostrophe", 0x27, ModControl},
		{"control+apostrophe", 0x27, ModControl},
		{"<Control><Alt>space", 0x20, ModControl | ModMod1},
		{"apostrophe", 0x27, 0},
		{"<Super>period", 0x2e, ModMod4},
		{"<Control>a", 0x61, ModControl},
		{"<Control><Shift>space", 0x20, ModControl | ModShift},
		// The recommended trigger: a key that is not a modifier, because a
		// modifier whose release the daemon swallows latches in the client.
		{"<F9>", 0xffc6, 0},
		{"F9", 0xffc6, 0},
		{"<F12>", 0xffc9, 0},
	}
	for _, testCase := range cases {
		key, err := ParseShortcut(testCase.spec)
		if err != nil {
			t.Errorf("ParseShortcut(%q): %v", testCase.spec, err)
			continue
		}
		if key.Keyval != testCase.keyval || key.State != testCase.state {
			t.Errorf("ParseShortcut(%q) = keyval %#x state %d, want %#x/%d",
				testCase.spec, key.Keyval, key.State, testCase.keyval, testCase.state)
		}
		if key.Keycode != 0 {
			t.Errorf("ParseShortcut(%q) set keycode %d; 0 means \"any keycode\" and is the only portable choice",
				testCase.spec, key.Keycode)
		}
	}
}

func TestParseShortcutRejectsBadInput(t *testing.T) {
	for _, spec := range []string{"", "<Control>", "Control+apostrophe+space", "<Control", "F99", "Control+nonsense", "F0"} {
		if _, err := ParseShortcut(spec); err == nil {
			t.Errorf("ParseShortcut(%q) was accepted, want an error", spec)
		}
	}
}

// The component file records an absolute path that ibus-daemon will exec, which
// is the thing a moved checkout breaks. ComponentExec reads it back so doctor can
// ask whether it still exists.
func TestComponentExecRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "voiceime.xml")
	component := DefaultComponent(config.Default(), "/home/u/.local/bin/voiceime ibus-engine --ibus")
	if err := os.WriteFile(path, []byte(component.XML()), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ComponentExec(path)
	if err != nil {
		t.Fatalf("ComponentExec: %v", err)
	}
	if want := "/home/u/.local/bin/voiceime"; got != want {
		t.Errorf("ComponentExec = %q, want %q", got, want)
	}
}

func TestComponentExecRejectsAnEmptyCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.xml")
	if err := os.WriteFile(path, []byte("<component><name>x</name><exec></exec></component>"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ComponentExec(path); err == nil {
		t.Error("a component with an empty <exec> was accepted; nothing would ever start")
	}
}
