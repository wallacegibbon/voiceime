package ibus

import (
	"context"
	"fmt"
	"strings"

	"github.com/godbus/dbus/v5"
)

// Global shortcut support.
//
// IBus 1.5.29 added a write-only property on the daemon, `GlobalShortcutKeys`
// of type `(y a(uuu))`: a consumer byte plus a list of (keyval, keycode, state)
// keys. When one of those keys is pressed or released the daemon emits
// `GlobalShortcutKeyResponded(y type, b pressed, b backward)`.
//
// That signal carries *both* edges, which is what makes push-to-talk possible at
// all: every older IBus mechanism (engine hotkeys in the component XML, the
// gsettings hotkey lists) reports a press and nothing else, so "hold to talk,
// release to switch back" cannot be built on them.
//
// Verified on a private daemon: a third-party client may set the property (the
// call succeeds and does not need a privileged name), and the property is
// write-only, so there is nothing to read back. Whether the daemon sees a
// particular key combination on a Wayland session is a property of the session,
// not of this code, and has to be tested by pressing the key.

const (
	ifaceIBus        = "org.freedesktop.IBus"
	ibusObjectPath   = dbus.ObjectPath("/org/freedesktop/IBus")
	propShortcutKeys = "GlobalShortcutKeys"
	sigShortcutKey   = ifaceIBus + ".GlobalShortcutKeyResponded"
)

// shortcutKeyTuple is the wire shape of one key in the property: a D-Bus struct
// "(uuu)", not an array. Marshalling a [3]uint32 produces "aau", which the daemon
// rejects - the exact error is worth recording because the types look alike:
//
//	Expected type "(ya(uuu))" but got "(yaau)"
type shortcutKeyTuple struct {
	Keyval  uint32
	Keycode uint32
	State   uint32
}

// shortcutRegistration is the property value: a consumer byte and the keys.
type shortcutRegistration struct {
	Type byte
	Keys []shortcutKeyTuple
}

// Key is one entry of the shortcut property. Keyval and state are X11 values,
// which is what IBus compares against; Keycode may be 0 to match any keycode for
// that keyval, which is the only portable choice for a program that has no X11
// keymap of its own.
type Key struct {
	Keyval  uint32
	Keycode uint32
	State   uint32
}

// IBus modifier masks, as IBus defines them.
const (
	ModShift   uint32 = 1 << 0
	ModLock    uint32 = 1 << 1
	ModControl uint32 = 1 << 2
	ModMod1    uint32 = 1 << 3 // Alt
	ModMod4    uint32 = 1 << 6 // Super
)

// ParseShortcut turns a human-readable combination such as
// "<Control>apostrophe" or "Control+space" into the key tuple IBus expects.
//
// Only the modifier masks and the key names the project actually needs are
// supported, plus the ASCII range: inventing a full keysym table would be a
// library, and a wrong entry in one would fail silently at match time.
func ParseShortcut(spec string) (Key, error) {
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" {
		return Key{}, fmt.Errorf("empty shortcut")
	}
	// Two spellings are accepted because both are in circulation: the GTK
	// accelerator form a user sees in GNOME's settings ("<Control>apostrophe")
	// and the plus form that appears in IBus's own configuration
	// ("Control+apostrophe"). Supporting only one of them would mean a silently
	// unregistered shortcut.
	var parts []string
	if strings.Contains(trimmed, "<") {
		rest := trimmed
		for strings.HasPrefix(rest, "<") {
			end := strings.Index(rest, ">")
			if end < 0 {
				return Key{}, fmt.Errorf("%q has an unclosed modifier", spec)
			}
			parts = append(parts, rest[1:end])
			rest = rest[end+1:]
		}
		if rest != "" {
			parts = append(parts, rest)
		}
	} else {
		parts = strings.Split(trimmed, "+")
	}

	key := Key{}
	keyName := ""
	for _, part := range parts {
		switch strings.ToLower(part) {
		case "control", "ctrl":
			key.State |= ModControl
		case "shift":
			key.State |= ModShift
		case "alt", "mod1":
			key.State |= ModMod1
		case "super", "mod4", "meta", "win":
			key.State |= ModMod4
		case "lock", "capslock":
			key.State |= ModLock
		default:
			if keyName != "" {
				return Key{}, fmt.Errorf("%q names two keys (%s and %s)", spec, keyName, part)
			}
			keyName = part
		}
	}
	if keyName == "" {
		return Key{}, fmt.Errorf("%q has modifiers but no key", spec)
	}
	keyval, err := keyvalFor(keyName)
	if err != nil {
		return Key{}, err
	}
	key.Keyval = keyval
	return key, nil
}

// keyvalFor resolves a key name to its X11 keysym. Names are matched
// case-insensitively; a single ASCII character stands for itself, which is where
// most of the useful combinations live.
func keyvalFor(name string) (uint32, error) {
	named := map[string]uint32{
		"apostrophe":   0x0027,
		"quoteleft":    0x0060,
		"grave":        0x0060,
		"space":        0x0020,
		"period":       0x002e,
		"comma":        0x002c,
		"semicolon":    0x003b,
		"slash":        0x002f,
		"backslash":    0x005c,
		"minus":        0x002d,
		"equal":        0x003d,
		"bracketleft":  0x005b,
		"bracketright": 0x005d,
		"Return":       0xff0d,
		"enter":        0xff0d,
		"Escape":       0xff1b,
		"escape":       0xff1b,
		"Tab":          0xff09,
		"tab":          0xff09,
		"BackSpace":    0xff08,
		"backspace":    0xff08,
		"Delete":       0xffff,
		"delete":       0xffff,
		"F1":           0xffbe,
		"F2":           0xffbf,
		"F3":           0xffc0,
		"F4":           0xffc1,
	}
	if value, ok := named[name]; ok {
		return value, nil
	}
	if value, ok := named[strings.ToLower(name)]; ok {
		return value, nil
	}
	// Function keys are a contiguous run of keysyms starting at XK_F1, so they are
	// computed rather than listed: the whole point of recommending one is that
	// F9..F12 are as available as F1.
	if number, ok := functionKeyNumber(name); ok {
		return 0xffbe + number - 1, nil
	}
	if len(name) == 1 && name[0] >= 0x20 && name[0] < 0x7f {
		return uint32(name[0]), nil
	}
	// A single letter is also a keysym, but the lowercase and uppercase keysyms
	// differ; Ctrl+letter is conventionally reported with the lowercase keysym.
	if len(name) == 1 {
		return uint32(strings.ToLower(name)[0]), nil
	}
	return 0, fmt.Errorf("unknown key name %q", name)
}

// functionKeyNumber extracts n from "F<n>", accepting F1 through F35 (the range
// X11 defines keysyms for).
func functionKeyNumber(name string) (uint32, bool) {
	upper := strings.ToUpper(name)
	if !strings.HasPrefix(upper, "F") || len(upper) < 2 {
		return 0, false
	}
	digits := upper[1:]
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	number := 0
	for _, r := range digits {
		number = number*10 + int(r-'0')
	}
	if number < 1 || number > 35 {
		return 0, false
	}
	return uint32(number), true
}

// ShortcutEvent is one press or release of a registered shortcut.
type ShortcutEvent struct {
	Type     byte
	Pressed  bool
	Backward bool
	Key      Key
}

// ShortcutListener owns the daemon property and delivers events.
type ShortcutListener struct {
	conn         *dbus.Conn
	shortcutType byte
	keys         []Key
	address      string
}

// NewShortcutListener connects to the IBus bus and registers the shortcut.
func NewShortcutListener(address string, shortcutType byte, keys []Key) (*ShortcutListener, error) {
	if len(keys) == 0 {
		return nil, fmt.Errorf("no shortcut keys to register")
	}
	conn, err := dialIBus(address)
	if err != nil {
		return nil, err
	}
	listener := &ShortcutListener{conn: conn, shortcutType: shortcutType, keys: keys, address: address}
	if err := listener.register(); err != nil {
		conn.Close()
		return nil, err
	}
	return listener, nil
}

func dialIBus(address string) (*dbus.Conn, error) {
	conn, err := dbus.Dial(address)
	if err != nil {
		return nil, fmt.Errorf("connect to the IBus bus at %s: %w", address, err)
	}
	if err := conn.Auth(nil); err != nil {
		conn.Close()
		return nil, err
	}
	if err := conn.Hello(); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

// register writes the shortcut property. The property is a single slot: whoever
// writes it last owns the shortcut, which is why voiceime registers only the one
// combination a user configured and says so in `hotkey status`.
func (l *ShortcutListener) register() error {
	tuples := make([]shortcutKeyTuple, 0, len(l.keys))
	for _, key := range l.keys {
		tuples = append(tuples, shortcutKeyTuple{Keyval: key.Keyval, Keycode: key.Keycode, State: key.State})
	}
	value := shortcutRegistration{Type: l.shortcutType, Keys: tuples}

	call := l.conn.Object(ifaceIBus, ibusObjectPath).Call(
		"org.freedesktop.DBus.Properties.Set", 0, ifaceIBus, propShortcutKeys, dbus.MakeVariant(value))
	if call.Err != nil {
		return fmt.Errorf("register the shortcut %v: %w", l.keys, call.Err)
	}
	return nil
}

// Close releases the shortcut by registering an empty key list.
func (l *ShortcutListener) Close() {
	empty := shortcutRegistration{Type: l.shortcutType, Keys: []shortcutKeyTuple{}}
	_ = l.conn.Object(ifaceIBus, ibusObjectPath).Call(
		"org.freedesktop.DBus.Properties.Set", 0, ifaceIBus, propShortcutKeys, dbus.MakeVariant(empty)).Err
	l.conn.Close()
}

// Listen delivers shortcut events until the context ends or the bus closes.
func (l *ShortcutListener) Listen(ctx context.Context, deliver func(ShortcutEvent)) error {
	channel := make(chan *dbus.Signal, 32)
	l.conn.Signal(channel)
	if err := l.conn.AddMatchSignal(
		dbus.WithMatchInterface(ifaceIBus),
		dbus.WithMatchMember("GlobalShortcutKeyResponded"),
	); err != nil {
		return fmt.Errorf("subscribe to shortcut events: %w", err)
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case signal, ok := <-channel:
			if !ok {
				return fmt.Errorf("the IBus bus closed")
			}
			if signal.Name != sigShortcutKey || len(signal.Body) < 3 {
				continue
			}
			event := ShortcutEvent{}
			if shortcutType, ok := signal.Body[0].(byte); ok {
				event.Type = shortcutType
			}
			if pressed, ok := signal.Body[1].(bool); ok {
				event.Pressed = pressed
			}
			if backward, ok := signal.Body[2].(bool); ok {
				event.Backward = backward
			}
			deliver(event)
		}
	}
}

// CurrentEngine asks the daemon which engine is selected, so it can be restored
// on release. GetGlobalEngine is deprecated upstream but it is the only call that
// answers this question, and `ibus engine` uses the same one.
func CurrentEngine(address string) (string, error) {
	conn, err := dialIBus(address)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	var value dbus.Variant
	if err := conn.Object(ifaceIBus, ibusObjectPath).Call(ifaceIBus+".GetGlobalEngine", 0).Store(&value); err != nil {
		return "", fmt.Errorf("ask the daemon for the current engine: %w", err)
	}
	// The reply is an IBusEngineDesc: (type name, attachments, fields...) with the
	// engine name as the first field after the attachments, per the serialisation
	// convention recorded in docs/ibus-protocol.md.
	fields, ok := value.Value().([]interface{})
	if !ok || len(fields) < 3 {
		return "", fmt.Errorf("unexpected engine description: %T", value.Value())
	}
	name, ok := fields[2].(string)
	if !ok {
		return "", fmt.Errorf("engine description has no name: %v", fields[2])
	}
	return name, nil
}

// SetEngine switches the selected engine.
func SetEngine(address, name string) error {
	conn, err := dialIBus(address)
	if err != nil {
		return err
	}
	defer conn.Close()
	if err := conn.Object(ifaceIBus, ibusObjectPath).Call(ifaceIBus+".SetGlobalEngine", 0, name).Err; err != nil {
		return fmt.Errorf("switch to %s: %w", name, err)
	}
	return nil
}
