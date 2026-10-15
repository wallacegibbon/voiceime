package ibus

import (
	"os"
	"path/filepath"
	"strconv"
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

// voiceime's shortcut spelling and GTK's overlap except exactly where the
// default lives: "<F9>" is "the F9 key" to voiceime and "modifier F9" (which GTK
// rejects) to GTK. That is why the conversion exists, and why it is not a no-op.
func TestGTKAccelerator(t *testing.T) {
	cases := []struct{ spec, want string }{
		{"<F9>", "F9"},
		{"F9", "F9"},
		{"<F12>", "F12"},
		{"<Control>apostrophe", "<Control>apostrophe"},
		{"Control+apostrophe", "<Control>apostrophe"},
		{"Control+space", "<Control>space"},
		{"<Super>period", "<Super>period"},
		{"<Control><Shift>space", "<Control><Shift>space"},
		{"apostrophe", "apostrophe"},
	}
	for _, testCase := range cases {
		got, err := GTKAccelerator(testCase.spec)
		if err != nil {
			t.Errorf("GTKAccelerator(%q): %v", testCase.spec, err)
			continue
		}
		if got != testCase.want {
			t.Errorf("GTKAccelerator(%q) = %q, want %q", testCase.spec, got, testCase.want)
		}
	}
	if _, err := GTKAccelerator("no-such-key"); err == nil {
		t.Error("a shortcut that is not a key name was accepted")
	}
}

func TestKeyNameInvertsKeyvalFor(t *testing.T) {
	// Every name keyvalFor accepts must round-trip through KeyName, or the GNOME
	// keybinding would silently disagree with the configured shortcut.
	names := []string{
		"apostrophe", "grave", "space", "period", "comma", "semicolon", "slash",
		"backslash", "minus", "equal", "bracketleft", "bracketright", "Return",
		"Escape", "Tab", "BackSpace", "Delete",
	}
	for _, name := range names {
		keyval, err := keyvalFor(name)
		if err != nil {
			t.Fatalf("keyvalFor(%q): %v", name, err)
		}
		if got := KeyName(keyval); got == "" {
			t.Errorf("KeyName(0x%04x) from %q is empty", keyval, name)
		}
	}
	for number := 1; number <= 35; number++ {
		name := "F" + strconv.Itoa(number)
		keyval, _ := keyvalFor(name)
		if got := KeyName(keyval); got != name {
			t.Errorf("KeyName(F%d) = %q, want %q", number, got, name)
		}
	}
}
