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
func (t *Transcript) AppendStream(text string, startSample, endSample int64) {
	if text == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.chunks = append(t.chunks, Chunk{
		Text:        text,
		StartSample: startSample,
		EndSample:   endSample,
		Source:      "stream",
	})
}

// SetSnapshot replaces the provisional text with a whole-transcript hypothesis.
// Only revision-capable models emit one, but when they do, replacing is the only
// correct response: the hypothesis is the model's current best reading of
// everything still provisional.
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

// Commit marks every provisional chunk as delivered.
func (t *Transcript) Commit() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.committed = len(t.chunks)
	return t.textLocked()
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

// CommittedText is what the application has already been given.
func (t *Transcript) CommittedText() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var builder strings.Builder
	for index := 0; index < t.committed && index < len(t.chunks); index++ {
		builder.WriteString(t.chunks[index].Text)
	}
	return builder.String()
}

// ProvisionalText is text decoded but not yet delivered - the part a repair can
// still change and, if the application supports preedit, the part shown as an
// in-progress composition.
func (t *Transcript) ProvisionalText() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.provisionalTextLocked()
}

// Chunks returns a copy of the chunk list, for logs and tests.
func (t *Transcript) Chunks() []Chunk {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Chunk, len(t.chunks))
	copy(out, t.chunks)
	return out
}

// Reset forgets everything, for a new dictation session.
func (t *Transcript) Reset() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.chunks = nil
	t.committed = 0
}

// ProvisionalRange returns the audio span the provisional text covers, which is
// what a repair pass should re-decode.
func (t *Transcript) ProvisionalRange() (int64, int64, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.committed >= len(t.chunks) {
		return 0, 0, false
	}
	start := t.chunks[t.committed].StartSample
	end := t.chunks[len(t.chunks)-1].EndSample
	return start, end, true
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
