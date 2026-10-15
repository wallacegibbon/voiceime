package cli

import "testing"

// The sample is verbatim output from `ibus list-engine` (IBus 1.5.29-rc2): a
// language header per group and one indented line per engine. Getting this parse
// wrong is not cosmetic - doctor reads it to decide whether text delivery is even
// possible, and an exact whole-line match reports a correctly installed engine as
// missing.
const listEngineSample = `language: English
  xkb:us::eng - English (US)
  xkb:us:dvorak:eng - English (Dvorak)
language: Chinese
  libpinyin - Intelligent Pinyin
  voiceime - Voiceime (speech)
language: Korean
  hangul - Korean (Hangul)
`

func TestListEngineContainsFindsIndentedEngines(t *testing.T) {
	for _, name := range []string{"voiceime", "xkb:us::eng", "libpinyin", "hangul"} {
		if !listEngineContains(listEngineSample, name) {
			t.Errorf("listEngineContains(%q) = false, want true", name)
		}
	}
}

func TestListEngineContainsRejectsAbsentAndPartialNames(t *testing.T) {
	cases := map[string]string{
		"absent engine":          "voiceime-probe",
		"prefix of a real name":  "voice",
		"suffix of a real name":  "ime",
		"language group header":  "language:",
		"empty name":             "",
		"description text only":  "Voiceime (speech)",
		"case-different variant": "VoiceIME",
	}
	for what, name := range cases {
		if listEngineContains(listEngineSample, name) {
			t.Errorf("%s: listEngineContains(%q) = true, want false", what, name)
		}
	}
}
