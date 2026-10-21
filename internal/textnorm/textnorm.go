// Package textnorm cleans up what the recognisers produce before the user sees
// it: Traditional Chinese becomes Simplified.
//
// It exists because the two models in the pipeline disagree about script. The
// repair model (Qwen3-ASR) is the one whose text is committed, and it is free to
// answer in Traditional Chinese, which is not what a Simplified Chinese dictation
// target expects.
//
// The setting is part of the configuration (text_script) because it is a user
// preference: a user in Taiwan wants Traditional. When the configured language is
// not Chinese the package is a no-op, so it cannot damage Japanese kanji or
// Korean hanja.
package textnorm

import "strings"

// Script settings.
const (
	// ScriptSimplified converts Traditional Chinese to Simplified.
	ScriptSimplified = "simplified"
	// ScriptPreserve leaves the recogniser's characters alone.
	ScriptPreserve = "preserve"
)

// Options selects the conversion. An empty value means the default.
type Options struct {
	// Script is ScriptSimplified or ScriptPreserve.
	Script string
}

// Normalizer applies the selected conversion. A nil *Normalizer is valid and
// leaves text unchanged, which is what a non-Chinese language gets.
type Normalizer struct{}

// New returns the normalizer for a language. Any language that is not Chinese
// gets nil: converting Japanese kanji to Simplified Chinese would be corruption,
// not cleanup.
func New(language string, options Options) *Normalizer {
	if !isChinese(language) {
		return nil
	}
	if resolve(options.Script, ScriptSimplified) != ScriptSimplified {
		return nil
	}
	return &Normalizer{}
}

func resolve(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

// isChinese reports whether a language code names Chinese. It accepts the codes
// the recognisers and the configuration use, including the regional tags a user
// may write by hand.
func isChinese(language string) bool {
	language = strings.TrimSpace(strings.ToLower(language))
	if language == "" {
		return false
	}
	for _, prefix := range []string{"zh", "cmn", "yue"} {
		if language == prefix || strings.HasPrefix(language, prefix+"-") || strings.HasPrefix(language, prefix+"_") {
			return true
		}
	}
	switch language {
	case "chinese", "mandarin", "cantonese":
		return true
	}
	return false
}

// Normalize applies the conversion to raw recognizer text. It is safe on a nil
// receiver and on empty text, but it is not safe to apply twice: OpenCC's t2s
// data is not idempotent (乾圖 -> 乾图 -> 干图), so callers must normalize the
// model's raw output rather than text they have already stored in normalized
// form. The transcript does exactly that: it stores raw chunks and normalizes
// them when they are read.
func (n *Normalizer) Normalize(text string) string {
	if n == nil || text == "" {
		return text
	}
	return traditionalToSimplified.convert(text)
}
