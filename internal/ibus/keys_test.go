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
	}
}

func TestParseShortcutRejectsBadInput(t *testing.T) {
	for _, spec := range []string{"", "<Control>", "Control+apostrophe+space", "<Control", "F99", "Control+nonsense", "F0"} {
		if _, err := ParseShortcut(spec); err == nil {
			t.Errorf("ParseShortcut(%q) was accepted, want an error", spec)
		}
	}
}

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

func TestKeyMatchesTheStateAnEngineIsHanded(t *testing.T) {
	trigger, err := ParseShortcut("<Control>apostrophe")
	if err != nil {
		t.Fatalf("ParseShortcut: %v", err)
	}
	if trigger.Keyval != 0x0027 || trigger.State != ModControl {
		t.Fatalf("parsed %+v, want apostrophe with Control", trigger)
	}
	if Held := trigger.Held(ModControl); !Held {
		t.Error("Control+apostrophe does not match a state with Control down")
	}
	if trigger.Held(0) {
		t.Error("Control+apostrophe matched a state with no modifier: a bare key would start a dictation")
	}
	// A release carries the same modifiers plus the release bit, which the caller masks
	// off before matching; the lock and button bits a client may add are not part of a
	// combination.
	release := (ModControl | ReleaseMask | ModLock) & ModifierMask
	if !trigger.Held(release) {
		t.Errorf("release state 0x%x does not match the trigger", release)
	}
	if ReleaseMask != 1<<30 {
		t.Errorf("ReleaseMask = %d, want IBUS_RELEASE_MASK (1<<30)", ReleaseMask)
	}
}

// A modifier release is one of the ways a hold ends, so each modifier keysym has to name
// the bit the trigger was matched against.
func TestModifierMaskOfCoversTheModifierKeysyms(t *testing.T) {
	cases := map[uint32]uint32{
		0xffe1: ModShift,   // XK_Shift_L
		0xffe4: ModControl, // XK_Control_R
		0xffea: ModMod1,    // XK_Alt_R
		0xffeb: ModMod4,    // XK_Super_L
		0xffee: ModMod4,    // XK_Hyper_R
	}
	for keyval, want := range cases {
		if got := ModifierMaskOf(keyval); got != want {
			t.Errorf("ModifierMaskOf(0x%04x) = 0x%x, want 0x%x", keyval, got, want)
		}
	}
	for _, keyval := range []uint32{0x0020, 'a', 0xff0d, 0xffbe, 0xffef} {
		if got := ModifierMaskOf(keyval); got != 0 {
			t.Errorf("ModifierMaskOf(0x%04x) = 0x%x, want 0 for a key that is not a modifier", keyval, got)
		}
	}
}
