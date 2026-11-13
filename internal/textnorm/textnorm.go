// Package textnorm cleans up what the recognisers produce before the user sees
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

func (n *Normalizer) Normalize(text string) string {
	if n == nil || text == "" {
		return text
	}
	return traditionalToSimplified.convert(text)
}
