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

// voiceime only appends. The user's own entries keep their order, and an install that runs
// twice changes nothing the second time.
func TestWithSourceOnlyAppends(t *testing.T) {
	existing := []InputSource{{"xkb", "us"}, {"ibus", "pinyin"}}
	ours := InputSource{"ibus", "voiceime"}

	updated, added := withSource(existing, ours)
	if !added {
		t.Fatal("withSource reported no change for a missing source")
	}
	want := []InputSource{{"xkb", "us"}, {"ibus", "pinyin"}, {"ibus", "voiceime"}}
	if len(updated) != len(want) {
		t.Fatalf("got %v, want %v", updated, want)
	}
	for index := range want {
		if updated[index] != want[index] {
			t.Fatalf("got %v, want the existing entries in order followed by ours", updated)
		}
	}
	if len(existing) != 2 || existing[0].ID != "us" {
		t.Errorf("withSource modified its input: %v", existing)
	}

	again, added := withSource(updated, ours)
	if added {
		t.Error("a second add reported a change")
	}
	if len(again) != len(updated) {
		t.Errorf("a second add changed the list: %v", again)
	}
}

// Removing ours leaves every other entry where it was, including when ours is in the middle.
func TestWithoutSourceKeepsTheRest(t *testing.T) {
	existing := []InputSource{{"xkb", "us"}, {"ibus", "voiceime"}, {"ibus", "pinyin"}}
	kept, removed := withoutSource(existing, InputSource{"ibus", "voiceime"})
	if !removed {
		t.Fatal("withoutSource did not find the source")
	}
	want := []InputSource{{"xkb", "us"}, {"ibus", "pinyin"}}
	if len(kept) != len(want) {
		t.Fatalf("got %v, want %v", kept, want)
	}
	for index := range want {
		if kept[index] != want[index] {
			t.Fatalf("got %v, want the remaining entries in order", kept)
		}
	}
	if _, removed := withoutSource(kept, InputSource{"ibus", "voiceime"}); removed {
		t.Error("removing a missing source reported a change")
	}
}

// What the writer produces is what the reader parses back, so a rewritten list cannot lose
// or alter an entry the user configured.
func TestSourcesRoundTrip(t *testing.T) {
	cases := [][]InputSource{
		nil,
		{{"xkb", "us"}},
		{{"xkb", "us"}, {"ibus", "pinyin"}, {"ibus", "voiceime"}},
		{{"xkb", "de"}, {"xkb", "us"}, {"ibus", "libpinyin"}, {"ibus", "chewing"}},
	}
	for _, want := range cases {
		var builder strings.Builder
		builder.WriteString("[")
		for index, source := range want {
			if index != 0 {
				builder.WriteString(", ")
			}
			builder.WriteString("(" + quote(source.Type) + ", " + quote(source.ID) + ")")
		}
		builder.WriteString("]")
		got := parseSources(builder.String())
		if len(got) != len(want) {
			t.Errorf("parseSources(%q) = %v, want %v", builder.String(), got, want)
			continue
		}
		for index := range want {
			if got[index] != want[index] {
				t.Errorf("round trip changed entry %d: %v, want %v", index, got[index], want[index])
			}
		}
	}
}
