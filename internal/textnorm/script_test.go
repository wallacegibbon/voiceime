package textnorm

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

// The t2s tables are copied from OpenCC. A checksum is the cheap regression
// test for the copy: it does not prove the matcher is right (the behaviour tests
// do), but it makes an accidental edit or a partial copy fail loudly instead of
// changing what the binary recognizes. When the data is intentionally updated,
// update the checksum in the same commit and re-verify against `opencc -c t2s`.
func TestDictionaryDataIsTheCopiedOpenCCData(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
	}{
		{"TSCharacters.txt", tsCharactersData, "6b5a0a799bea2bb22c001f635eaa3fc2904310f0c08addbff275477a80ecf09a"},
		{"TSPhrases.txt", tsPhrasesData, "b2ef895dd4953b4bb77fc8ef8d26a2a9ca6d43a760ed9a1d767672cfafa6324f"},
	}
	for _, tc := range cases {
		sum := sha256.Sum256([]byte(tc.data))
		if got := hex.EncodeToString(sum[:]); got != tc.want {
			t.Errorf("%s checksum = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// Phrase precedence is what keeps names and set phrases from being converted
// character by character. It is the property the embed exists for.
func TestPhrasePrecedence(t *testing.T) {
	normalizer := New("zh", Options{Numbers: NumbersPreserve})
	cases := map[string]string{
		"乾隆皇帝": "乾隆皇帝",
		"乾坤":   "乾坤",
		"乾圖":   "乾图",
		"一目瞭然": "一目了然",
		"臺灣":   "台湾",
	}
	for input, want := range cases {
		if got := normalizer.Normalize(input); got != want {
			t.Errorf("Normalize(%q) = %q, want %q", input, got, want)
		}
	}
}
