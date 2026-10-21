// Package textnorm cleans up what the recognisers produce before the user sees
// it: Traditional Chinese becomes Simplified, and Chinese numerals become Arabic
// digits.
//
// It exists because the two models in the pipeline disagree about text form.
// Voiceime's streaming model (SenseVoice) has inverse text normalization, so it
// writes "2025年10月21号". The repair model (Qwen3-ASR) is the one whose text is
// committed, and it has no ITN: the same sentence comes back as
// "二零二五年十月二十一号". The repair is also free to answer in Traditional
// Chinese, which is not what a Simplified Chinese dictation target expects.
//
// The settings are part of the configuration (text_script, text_numbers) because
// both are user preferences: a user in Taiwan wants Traditional, and someone
// dictating prose may prefer Chinese numerals. When the configured language is
// not Chinese the package is a no-op, so it cannot damage Japanese kanji or
// Korean hanja.
package textnorm

import "strings"

// Script and Numbers settings.
const (
	// ScriptSimplified converts Traditional Chinese to Simplified.
	ScriptSimplified = "simplified"
	// ScriptPreserve leaves the recogniser's characters alone.
	ScriptPreserve = "preserve"

	// NumbersArabic writes numbers as digits.
	NumbersArabic = "arabic"
	// NumbersPreserve leaves Chinese numerals as words.
	NumbersPreserve = "preserve"
)

// Options selects the two conversions. An empty value means the default.
type Options struct {
	// Script is ScriptSimplified or ScriptPreserve.
	Script string
	// Numbers is NumbersArabic or NumbersPreserve.
	Numbers string
}

// Normalizer applies the selected conversions. A nil *Normalizer is valid and
// leaves text unchanged, which is what a non-Chinese language gets.
type Normalizer struct {
	script  bool
	numbers bool
}

// New returns the normalizer for a language. Any language that is not Chinese
// gets nil: converting Japanese kanji to Simplified Chinese or Korean hanja to
// digits would be corruption, not cleanup.
func New(language string, options Options) *Normalizer {
	if !isChinese(language) {
		return nil
	}
	normalizer := &Normalizer{
		script:  resolve(options.Script, ScriptSimplified) == ScriptSimplified,
		numbers: resolve(options.Numbers, NumbersArabic) == NumbersArabic,
	}
	if !normalizer.script && !normalizer.numbers {
		return nil
	}
	return normalizer
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

// Normalize applies the conversions to raw recognizer text. It is safe on a nil
// receiver and on empty text, but it is not safe to apply twice: OpenCC's t2s
// data is not idempotent (乾圖 -> 乾图 -> 干图), so callers must normalize the
// model's raw output rather than text they have already stored in normalized
// form. The transcript does exactly that: it stores raw chunks and normalizes
// them when they are read.
func (n *Normalizer) Normalize(text string) string {
	if n == nil || text == "" {
		return text
	}
	if n.script {
		text = traditionalToSimplified.convert(text)
	}
	if n.numbers {
		text = normalizeNumbers(text)
	}
	return text
}
