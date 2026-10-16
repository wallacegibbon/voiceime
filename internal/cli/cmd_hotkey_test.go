package cli

import (
	"path/filepath"
	"testing"
	"time"
)

// The X11 trigger is a GNOME keybinding that runs `hotkey toggle` once per key
// event, and a held key repeats. Without the guard a one-second hold switched the
// engine on and off ~19 times (measured), so holding the key looked like it did
// nothing: the parity decided the end state. These two tests pin the boundary the
// guard has to hold.
func TestRepeatGuardCollapsesHold(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hotkey-last-press")
	at := time.Now()
	if suppressRepeat(path, at) {
		t.Fatal("the first press of a hold must act")
	}
	// A hold keeps arriving every auto-repeat interval (~30ms at rate 33/s). Each
	// one refreshes the marker, so the hold stays suppressed however long it lasts.
	for i := 0; i < 100; i++ {
		at = at.Add(30 * time.Millisecond)
		if !suppressRepeat(path, at) {
			t.Fatalf("repeat %d of a hold must be suppressed", i)
		}
	}
}

func TestRepeatGuardAllowsATapAfterTheWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hotkey-last-press")
	start := time.Now()
	if suppressRepeat(path, start) {
		t.Fatal("the first tap must act")
	}
	if suppressRepeat(path, start.Add(repeatGuard+time.Millisecond)) {
		t.Fatal("a tap past the window must act")
	}
}

// XDG_SESSION_TYPE can read "tty" in an ssh or console shell. Treating that as the
// session type is how an install chose neither trigger, so the display variables
// have to win when the value is not a display server.
func TestSessionTypeIgnoresNonDisplaySessionType(t *testing.T) {
	t.Setenv("XDG_SESSION_TYPE", "tty")
	t.Setenv("WAYLAND_DISPLAY", "")
	t.Setenv("DISPLAY", ":1")
	if got := sessionType(); got != "x11" {
		t.Fatalf("sessionType() = %q, want x11 (DISPLAY is set; tty is not a display server)", got)
	}
	t.Setenv("DISPLAY", "")
	if got := sessionType(); got != "tty" {
		t.Fatalf("sessionType() = %q, want the raw value %q when nothing else is set", got, "tty")
	}
	t.Setenv("XDG_SESSION_TYPE", "wayland")
	if got := sessionType(); got != "wayland" {
		t.Fatalf("sessionType() = %q, want wayland", got)
	}
}
