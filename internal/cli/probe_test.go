package cli

import "testing"

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
