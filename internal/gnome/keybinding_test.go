package gnome

import "testing"

// parseList has to read what `gsettings get <schema> <key>` actually prints,
// including its two empty spellings and its single-quoted items.
func TestParseList(t *testing.T) {
	cases := []struct {
		raw  string
		want []string
	}{
		{"[]", nil},
		{"@as []", nil},
		{"['/a/']", []string{"/a/"}},
		{"['/a/', '/b/']", []string{"/a/", "/b/"}},
		{"@as ['/a/']", []string{"/a/"}},
	}
	for _, testCase := range cases {
		got := parseList(testCase.raw)
		if len(got) != len(testCase.want) {
			t.Errorf("parseList(%q) = %v, want %v", testCase.raw, got, testCase.want)
			continue
		}
		for i := range got {
			if got[i] != testCase.want[i] {
				t.Errorf("parseList(%q)[%d] = %q, want %q", testCase.raw, i, got[i], testCase.want[i])
			}
		}
	}
}

// A keybinding value is written into a GVariant string, so quoting has to survive
// the round trip: a command that contains a space (always) or an apostrophe (a
// path can) must come back byte for byte, or the binding would run the wrong
// command.
func TestQuoteUnquoteRoundTrips(t *testing.T) {
	values := []string{
		"F9",
		"<Control>apostrophe",
		"/home/u/.local/bin/voiceime hotkey toggle",
		"/home/o'brien/bin/voiceime hotkey toggle",
	}
	for _, value := range values {
		if got := unquote(quote(value)); got != value {
			t.Errorf("unquote(quote(%q)) = %q", value, got)
		}
	}
}
