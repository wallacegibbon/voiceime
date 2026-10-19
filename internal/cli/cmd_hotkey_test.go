package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wallacegibbon/voiceime/internal/config"
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

// The listener's unit must let the configuration own the key.
//
// It used to bake the *configured* key into ExecStart, and `hotkey run` prefers an
// explicit --keys over the file. So `config set hotkey '<F10>'` followed by
// `config apply` - the documented way to make the running system match the file -
// restarted the unit straight back onto the old key. Named only when the caller asked
// for one, the configuration is what decides.
func TestHotkeyUnitLetsTheConfigurationOwnTheKey(t *testing.T) {
	unit := hotkeyUnit("/home/u/.local/bin/voiceime", "/home/u/.config/voiceime/config.toml", "")
	if strings.Contains(unit, "--keys") {
		t.Errorf("unit pins a key, so a changed `hotkey` setting cannot be applied:\n%s", unit)
	}
	if !strings.Contains(unit, "--config /home/u/.config/voiceime/config.toml") {
		t.Errorf("unit does not name the configuration file:\n%s", unit)
	}
	if !strings.Contains(unit, "/home/u/.local/bin/voiceime hotkey run") {
		t.Errorf("unit does not run the listener:\n%s", unit)
	}

	// An explicit --keys is still honoured: it is the one-off override.
	explicit := hotkeyUnit("/home/u/.local/bin/voiceime", "/home/u/.config/voiceime/config.toml", "<F10>")
	if !strings.Contains(explicit, "--keys <F10>") {
		t.Errorf("an explicit key was dropped from the unit:\n%s", explicit)
	}
}

// A path with a space has to survive the unit's command line, which systemd splits
// itself.
func TestHotkeyUnitQuotesAPathWithSpaces(t *testing.T) {
	unit := hotkeyUnit("/home/u/my apps/voiceime", "/home/u/my apps/config.toml", "")
	if !strings.Contains(unit, `--config '/home/u/my apps/config.toml'`) {
		t.Errorf("the configuration path is not quoted:\n%s", unit)
	}
}

// The listener and the one-shot toggle share one record of the engine to return to.
//
// They used to keep it in different places - memory and a file - so after a reboot the
// listener knew and the toggle did not, and the toggle's fallback was `xkb:us::eng`: on
// any machine whose layout is not US English, "stop dictating" silently changed the
// keyboard. One fact, one place, and the place is a file because a restart of either
// process must not lose it.
func TestPreviousEngineIsRecordedOnceForBothPaths(t *testing.T) {
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	if got := previousEngine(); got != "" {
		t.Fatalf("previousEngine() = %q with nothing recorded", got)
	}

	rememberPreviousEngine("mozc-jp")
	if got := previousEngine(); got != "mozc-jp" {
		t.Errorf("previousEngine() = %q, want the engine that was recorded", got)
	}
	if got, want := previousEnginePath(), filepath.Join(config.CacheDir(), "hotkey-previous"); got != want {
		t.Errorf("record at %q, want %q", got, want)
	}

	// An empty name is not a record: writing one would make the next toggle believe it
	// knows where to go back to.
	rememberPreviousEngine("  ")
	if got := previousEngine(); got != "mozc-jp" {
		t.Errorf("previousEngine() = %q after an empty record, want the previous one", got)
	}

	forgetPreviousEngine()
	if got := previousEngine(); got != "" {
		t.Errorf("previousEngine() = %q after forgetting", got)
	}
	// Forgetting twice, or with nothing there, is not an error.
	forgetPreviousEngine()
}
