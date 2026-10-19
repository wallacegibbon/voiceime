package ibus

import (
	"testing"

	"github.com/godbus/dbus/v5"
)

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
