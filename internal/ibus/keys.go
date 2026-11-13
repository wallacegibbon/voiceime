package ibus

import (
	"fmt"
	"strings"
)

// Key is one key combination, in the form IBus reports key events in.
type Key struct {
	Keyval uint32
	State  uint32
}

// IBus modifier masks (ibustypes.h).
const (
	ModShift   uint32 = 1 << 0
	ModLock    uint32 = 1 << 1
	ModControl uint32 = 1 << 2
	ModMod1    uint32 = 1 << 3 // Alt
	ModMod4    uint32 = 1 << 6 // Super
)

// ReleaseMask is IBUS_RELEASE_MASK: set in the state of a key release.
const ReleaseMask uint32 = 1 << 30

// ModifierMask is every modifier a shortcut can name.
const ModifierMask = ModShift | ModLock | ModControl | ModMod1 | ModMod4

// Held reports whether every modifier this key requires is down in state.
func (k Key) Held(state uint32) bool { return state&k.State == k.State }

// ModifierMaskOf names the state bit a modifier keysym sets, or 0 for any other key.
func ModifierMaskOf(keyval uint32) uint32 {
	switch keyval {
	case 0xffe1, 0xffe2: // Shift_L, Shift_R
		return ModShift
	case 0xffe3, 0xffe4: // Control_L, Control_R
		return ModControl
	case 0xffe5, 0xffe6: // Caps_Lock, Shift_Lock
		return ModLock
	case 0xffe7, 0xffe8, 0xffe9, 0xffea: // Meta_L/R, Alt_L/R
		return ModMod1
	case 0xffeb, 0xffec, 0xffed, 0xffee: // Super_L/R, Hyper_L/R
		return ModMod4
	}
	return 0
}

// ParseShortcut turns "<Control>grave" or "Control+grave" into the tuple IBus compares
// against.
func ParseShortcut(spec string) (Key, error) {
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" {
		return Key{}, fmt.Errorf("empty shortcut")
	}
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
		"return":       0xff0d,
		"Escape":       0xff1b,
		"escape":       0xff1b,
		"Tab":          0xff09,
		"tab":          0xff09,
		"BackSpace":    0xff08,
		"backspace":    0xff08,
		"Delete":       0xffff,
		"delete":       0xffff,
	}
	if value, ok := named[name]; ok {
		return value, nil
	}
	if number, ok := functionKeyNumber(name); ok {
		return 0xffbe + number - 1, nil
	}
	if len(name) == 1 && name[0] >= 0x20 && name[0] < 0x7f {
		return uint32(name[0]), nil
	}
	if len(name) == 1 {
		return uint32(strings.ToLower(name)[0]), nil
	}
	return 0, fmt.Errorf("unknown key name %q", name)
}

// functionKeyNumber extracts n from "F<n>", accepting F1 through F35.
func functionKeyNumber(name string) (uint32, bool) {
	upper := strings.ToUpper(name)
	if !strings.HasPrefix(upper, "F") || len(upper) < 2 {
		return 0, false
	}
	number := 0
	for _, r := range upper[1:] {
		if r < '0' || r > '9' {
			return 0, false
		}
		number = number*10 + int(r-'0')
	}
	if number < 1 || number > 35 {
		return 0, false
	}
	return uint32(number), true
}
