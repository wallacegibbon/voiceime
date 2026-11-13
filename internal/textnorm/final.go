package textnorm

import "strings"

// Final cleans what a recogniser produced into the text that is typed.
//
// The recogniser ends its answer with a sentence-final period whether or not the speaker
// dictated one, and that is wrong for dictation: a word or a noun dropped at a cursor that
// is in the middle of a sentence arrives with a full stop after it. One trailing period is
// therefore removed. A question mark, an exclamation mark or a comma stays - those say
// something the speaker did say.
//
// The alternative is the model's own switch: SenseVoice answers without any punctuation at
// all when `enable_itn=false`, which would take the digits out of dates and numbers with it
// (measured: "嗯。" against "嗯"). This keeps the model's punctuation and removes only the
// mark at the end.
func Final(text string) string {
	text = strings.TrimSpace(text)
	for {
		trimmed := strings.TrimRight(text, "。.．")
		trimmed = strings.TrimSpace(trimmed)
		if trimmed == text {
			return text
		}
		text = trimmed
	}
}
