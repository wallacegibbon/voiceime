package ibus

import (
	"testing"

	"github.com/godbus/dbus/v5"
)

// The preedit is underlined, because an application only draws a composition
// underline when the text it receives says so.
//
// Measured on X11: the engine emitted UpdatePreeditText with an empty attribute
// list, GTK's IM module drew the provisional text as plain text, and a user saw no
// underline where libpinyin shows one. On Wayland the same signal was underlined,
// because the compositor's text-input path marks a composition by itself - which is
// exactly why the omission survived so long. The shape asserted here is the one
// libibus 1.5.29 itself serialises (`IBus.Attribute.serialize_object`):
//
//	('IBusAttribute', @a{sv} {}, uint32 1, uint32 1, uint32 0, uint32 6)
func TestPreeditTextUnderlinesTheWholeString(t *testing.T) {
	payload := NewPreeditText("中文输入法。")

	list, ok := payload.Attributes.Value().(ibusAttrList)
	if !ok {
		t.Fatalf("preedit attributes are %T, want an IBusAttrList", payload.Attributes.Value())
	}
	if list.TypeName != "IBusAttrList" {
		t.Errorf("attribute list type name = %q, want IBusAttrList", list.TypeName)
	}
	if len(list.Attributes) != 1 {
		t.Fatalf("preedit has %d attributes, want 1 (the underline)", len(list.Attributes))
	}
	attr, ok := list.Attributes[0].Value().(IBusAttribute)
	if !ok {
		t.Fatalf("attribute is %T, want IBusAttribute", list.Attributes[0].Value())
	}
	if attr.Type != attrTypeUnderline || attr.Value != attrUnderlineSingle {
		t.Errorf("attribute is type %d value %d, want underline/single (%d/%d)",
			attr.Type, attr.Value, attrTypeUnderline, attrUnderlineSingle)
	}
	// Indices are characters: six runes, eighteen bytes. A byte count would
	// underline past the end of the text and part of nothing.
	if attr.StartIndex != 0 || attr.EndIndex != 6 {
		t.Errorf("attribute covers [%d,%d), want [0,6)", attr.StartIndex, attr.EndIndex)
	}
}

// An empty preedit clears the composition, so there is nothing to underline and the
// attribute list stays empty.
func TestEmptyPreeditHasNoAttributes(t *testing.T) {
	payload := NewPreeditText("")
	if list, ok := payload.Attributes.Value().(ibusAttrList); !ok || len(list.Attributes) != 0 {
		t.Errorf("empty preedit attributes = %v, want an empty list", payload.Attributes.Value())
	}
}

// Committed text carries no attributes: it is final, not a composition.
func TestCommittedTextHasNoAttributes(t *testing.T) {
	payload := NewText("中文输入法。")
	if list, ok := payload.Attributes.Value().(ibusAttrList); !ok || len(list.Attributes) != 0 {
		t.Errorf("committed text attributes = %v, want an empty list", payload.Attributes.Value())
	}
}

// The engine answers the property names libibus actually declares.
//
// libibus 1.5.29 declares HasFocusId and ActiveSurroundingText on IBusEngine (checked
// against the typelib: `IBus.Engine.list_properties()`), while this answered FocusId -
// the name an older release used. The daemon then asked for a property it got an error
// for, which reads as a capability the engine lacks rather than a name it does not know.
// Both spellings answer now, so neither generation of daemon can miss it.
func TestEnginePropertiesAnswerTheDeclaredNames(t *testing.T) {
	properties := &propertiesObject{engine: &engineObject{}}

	for _, name := range []string{"HasFocusId", "FocusId"} {
		value, err := properties.Get(ifaceIBusEngine, name)
		if err != nil {
			t.Errorf("Get(%s): %v", name, err)
			continue
		}
		if value.Value() != true {
			t.Errorf("Get(%s) = %v, want true: the engine has a focus id", name, value.Value())
		}
	}
	if value, err := properties.Get(ifaceIBusEngine, "ActiveSurroundingText"); err != nil {
		t.Errorf("Get(ActiveSurroundingText): %v", err)
	} else if value.Value() != false {
		t.Errorf("ActiveSurroundingText = %v, want false: voiceime does not claim surrounding text", value.Value())
	}

	// An unknown property is an error rather than a plausible-looking empty value.
	if _, err := properties.Get(ifaceIBusEngine, "NotAProperty"); err == nil {
		t.Error("Get accepted an unknown property")
	}
}

// GetAll lists what this engine can honestly claim, under both spellings, and does not
// invent a type for ContentType.
func TestEngineGetAllListsBothSpellings(t *testing.T) {
	properties := &propertiesObject{engine: &engineObject{}}
	all, err := properties.GetAll(ifaceIBusEngine)
	if err != nil {
		t.Fatalf("GetAll: %v", err)
	}
	for _, name := range []string{"HasFocusId", "FocusId", "ActiveSurroundingText"} {
		if _, ok := all[name]; !ok {
			t.Errorf("GetAll does not list %s: %v", name, all)
		}
	}
	if _, ok := all["ContentType"]; ok {
		t.Error("GetAll lists ContentType with a type this engine only guesses at")
	}
}

// The daemon writes ContentType and voiceime does not vary its behaviour by input purpose,
// so accepting and ignoring the write is correct - and a Set must not fail, or the daemon
// logs a warning on every focus change.
func TestEngineAcceptsContentType(t *testing.T) {
	properties := &propertiesObject{engine: &engineObject{}}
	if err := properties.Set(ifaceIBusEngine, "ContentType", dbus.MakeVariant(uint32(0))); err != nil {
		t.Errorf("Set(ContentType): %v", err)
	}
}
