package gnome

import (
	"strings"
	"testing"
)

// The value `gsettings get org.gnome.desktop.input-sources sources` prints, in every
// shape it takes on a real machine.
func TestParseSources(t *testing.T) {
	cases := []struct {
		raw  string
		want []InputSource
	}{
		{"@a(ss) []", nil},
		{"[]", nil},
		{"[('xkb', 'us')]", []InputSource{{Type: "xkb", ID: "us"}}},
		{
			"[('xkb', 'us'), ('ibus', 'pinyin')]",
			[]InputSource{{Type: "xkb", ID: "us"}, {Type: "ibus", ID: "pinyin"}},
		},
		// The reference machine's list, which is the case this whole file exists for.
		{
			"@a(ss) [('xkb', 'us'), ('ibus', 'pinyin'), ('ibus', 'voiceime')]",
			[]InputSource{{"xkb", "us"}, {"ibus", "pinyin"}, {"ibus", "voiceime"}},
		},
	}
	for _, testCase := range cases {
		got := parseSources(testCase.raw)
		if len(got) != len(testCase.want) {
			t.Errorf("parseSources(%q) = %v, want %v", testCase.raw, got, testCase.want)
			continue
		}
		for index := range got {
			if got[index] != testCase.want[index] {
				t.Errorf("parseSources(%q)[%d] = %v, want %v", testCase.raw, index, got[index], testCase.want[index])
			}
		}
	}
}

func TestInputSourceSpelling(t *testing.T) {
	if got := (InputSource{Type: "ibus", ID: "voiceime"}).String(); !strings.Contains(got, "ibus") ||
		!strings.Contains(got, "voiceime") {
		t.Errorf("InputSource.String() = %q, want the pair in the dconf spelling", got)
	}
}
