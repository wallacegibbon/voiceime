package asr

import (
	"strings"
	"sync"
)

// Chunk is a piece of transcript with the audio span it came from.
type Chunk struct {
	Text        string `json:"text"`
	StartSample int64  `json:"start_sample"`
	EndSample   int64  `json:"end_sample"`
	// Source is "stream" for a window partial and "repair" for text produced by
	// re-transcribing a finished segment offline. Keeping it visible is what
	// makes the two-layer design debuggable: a bad transcript can be attributed
	// to the layer that produced it.
	Source string `json:"source"`
}

// Transcript is the two-layer text model: committed text that has been delivered
// to the application, and provisional text that has not.
//
// The split is not cosmetic. Streaming ASR here decodes independent windows, so
// every window boundary is a place where words get split or duplicated, and the
// only fix is to re-decode the finished segment with the offline path. That is
// possible exactly while the text is still provisional. Once text is committed
// (delivered through IBus) it can only be corrected by rewriting it in the
// application, which most applications do not support.
//
// Hence the rule enforced here: repair replaces provisional chunks only, and
// committing is an explicit decision made when the user has stopped speaking.
type Transcript struct {
	mu        sync.Mutex
	chunks    []Chunk
	committed int
}

// NewTranscript returns an empty transcript.
func NewTranscript() *Transcript { return &Transcript{} }

// AppendStream adds a provisional chunk decoded from the window that just ended.
//
// A window that starts inside audio already delivered to the application is
// dropped rather than kept. A window is decoded as one unit, so its text is a
// reading of audio the application already has, and there is no way to split that
// text at the delivered boundary; keeping it showed the same words again as
// preedit and then committed them a second time. Reproduced on the wire: after a
// segment closed at 4.50s, the next window covered [3.14s, 6.14s] and its text
// re-entered the transcript, so the committed sentence appeared underlined again
// and was delivered twice. Whether that window arrives before or after the commit
// is a race - it depends on how fast the repair lands - which is why this is
// enforced here rather than by ordering the calls.
//
// The return value says whether the chunk was kept, so a caller can report a drop
// instead of losing it silently. An empty decode returns false as well, and there
// is nothing to report about that.
func (t *Transcript) AppendStream(text string, startSample, endSample int64) bool {
	if text == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if startSample < t.deliveredEndLocked() {
		return false
	}
	t.chunks = append(t.chunks, Chunk{
		Text:        text,
		StartSample: startSample,
		EndSample:   endSample,
		Source:      "stream",
	})
	return true
}

// SetSnapshot replaces the provisional text with a whole-transcript hypothesis.
// Only revision-capable models emit one, but when they do, replacing is the only
// correct response: the hypothesis is the model's current best reading of
// everything still provisional.
//
// Known gap: a snapshot is a reading of the whole transcript, so one that arrives
// after a commit describes delivered audio too, and it is unlike AppendStream in
// that no part of it can be attributed to a sample range. No family voiceime
// selects has been observed to emit snapshots, so this is left as-is and recorded
// rather than guessed at: a snapshot-emitting family needs its hypothesis trimmed
// to the provisional range before it can be trusted after a commit.
func (t *Transcript) SetSnapshot(text string, startSample, endSample int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.chunks = t.chunks[:t.committed]
	t.chunks = append(t.chunks, Chunk{
		Text:        text,
		StartSample: startSample,
		EndSample:   endSample,
		Source:      "stream",
	})
}

// Repair replaces the provisional text covering [startSample, endSample) with
// freshly decoded text.
//
// Two rules make this safe to call from a background worker at any time:
//
//   - Committed text is never touched, and a range that contains no provisional
//     chunk is a no-op. Otherwise a repair finishing late - the user already
//     paused and the text was delivered - would append its text a second time.
//   - An empty decode is a no-op as well. Empty output from the offline path is
//     indistinguishable from "the model found nothing there", and dropping text
//     the user has already watched appear is a worse failure than keeping text
//     that a window boundary damaged. Removing hallucinations reliably needs a
//     confidence signal this path does not have; that is recorded as a known
//     limitation rather than guessed at.
//
// A provisional chunk that overlaps the range at all is dropped, even if part of
// it lies outside: the range comes from the endpoint detector and covers a whole
// spoken segment, so an overlapping partial is always part of the same speech,
// and there is no way to split its text that is better than trusting the decode.
// The repaired chunk is inserted where the first replaced chunk was, so a repair
// that lands while newer provisional text exists keeps the transcript in order.
func (t *Transcript) Repair(startSample, endSample int64, text string) {
	if text == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	kept := make([]Chunk, 0, len(t.chunks)+1)
	replaced := false
	for index, chunk := range t.chunks {
		if index >= t.committed && overlaps(chunk, startSample, endSample) {
			if !replaced {
				kept = append(kept, Chunk{
					Text:        text,
					StartSample: startSample,
					EndSample:   endSample,
					Source:      "repair",
				})
			}
			replaced = true
			continue
		}
		kept = append(kept, chunk)
	}
	if !replaced {
		return
	}
	t.chunks = kept
}

// RepairSpan returns the audio that a repair for [startSample, endSample) has to
// re-decode: the union of that range and every provisional chunk the repair will
// replace.
//
// The union is the point. Repair drops any chunk that overlaps the range *at
// all*, and a streaming window is decoded as one unit, so a chunk can extend well
// beyond the range. Decoding only the range therefore discarded that chunk's text
// for the audio outside it - the words at the start or the end of the utterance,
// which is exactly where an energy detector is least certain and most likely to
// be short. That is the reported "only part of what I said was recorded".
//
// ok is false when no provisional text covers the range: there is nothing to
// correct then, and no reason to pay for a decode.
func (t *Transcript) RepairSpan(startSample, endSample int64) (int64, int64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	start, end := startSample, endSample
	found := false
	for index := t.committed; index < len(t.chunks); index++ {
		chunk := t.chunks[index]
		if !overlaps(chunk, startSample, endSample) {
			continue
		}
		found = true
		if chunk.StartSample < start {
			start = chunk.StartSample
		}
		if chunk.EndSample > end {
			end = chunk.EndSample
		}
	}
	return start, end, found
}

// CommitProvisional returns the text that is about to become committed, which is
// what the caller needs to hand to the application, and then commits it.
func (t *Transcript) CommitProvisional() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	pending := t.provisionalTextLocked()
	t.committed = len(t.chunks)
	return pending
}

// Text is committed plus provisional text: the full running transcript.
func (t *Transcript) Text() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.textLocked()
}

// ProvisionalText is text decoded but not yet delivered - the part a repair can
// still change and, if the application supports preedit, the part shown as an
// in-progress composition.
func (t *Transcript) ProvisionalText() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.provisionalTextLocked()
}

// deliveredEndLocked is the end of the audio the application has already been
// given: the furthest sample covered by a committed chunk.
func (t *Transcript) deliveredEndLocked() int64 {
	var end int64
	committed := min(t.committed, len(t.chunks))
	for index := 0; index < committed; index++ {
		if t.chunks[index].EndSample > end {
			end = t.chunks[index].EndSample
		}
	}
	return end
}

func (t *Transcript) textLocked() string {
	var builder strings.Builder
	for _, chunk := range t.chunks {
		builder.WriteString(chunk.Text)
	}
	return builder.String()
}

func (t *Transcript) provisionalTextLocked() string {
	var builder strings.Builder
	for index := t.committed; index < len(t.chunks); index++ {
		builder.WriteString(t.chunks[index].Text)
	}
	return builder.String()
}

// overlaps reports whether a chunk shares any audio with the range.
func overlaps(chunk Chunk, startSample, endSample int64) bool {
	return chunk.StartSample < endSample && chunk.EndSample > startSample
}
