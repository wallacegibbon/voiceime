package asr

import (
	"strings"
	"testing"
)

// chunksForTest reads the transcript's chunks directly.
//
// The transcript used to expose a Chunks() accessor "for logs and tests", but no
// shipped code ever called it, which is the pattern that hid a real bug once
// already (Segmenter.Calibrate was the only caller of the adaptive threshold, and
// it too was test-only). Tests live in this package, so they can read the field
// and the production type keeps only the API the pipeline uses.
func chunksForTest(transcript *Transcript) []Chunk {
	transcript.mu.Lock()
	defer transcript.mu.Unlock()
	out := make([]Chunk, len(transcript.chunks))
	copy(out, transcript.chunks)
	return out
}

// The transcript model is where "correct the words already shown" is decided, so
// its rules are pinned down by tests: repair replaces provisional text only, and
// committing is a one-way door.
func TestPlaceLeavesCommittedTextAlone(t *testing.T) {
	transcript := NewTranscript()
	transcript.AppendStream("第一段。", 0, 16000)
	transcript.AppendStream("第二段。", 16000, 32000)
	transcript.CommitProvisional()

	// A repair that arrives after a commit must not touch committed text.
	transcript.Place(0, 32000, "不该出现的修复")
	if got := transcript.Text(); got != "第一段。第二段。" {
		t.Errorf("after commit, Text() = %q, want committed text unchanged", got)
	}
}

func TestPlaceReplacesOverlappingChunks(t *testing.T) {
	transcript := NewTranscript()
	transcript.AppendStream("欢迎大家来体验达摩院。", 15000, 48000)
	transcript.AppendStream("推出的语音识别模型。", 48000, 88000)

	// The endpoint detector reports the whole utterance; both partials fall
	// inside it, so both are replaced by the offline decode.
	transcript.Place(15000, 88000, "欢迎大家来体验达摩院推出的语音识别模型。")

	if got := transcript.ProvisionalText(); got != "欢迎大家来体验达摩院推出的语音识别模型。" {
		t.Errorf("ProvisionalText() = %q", got)
	}
	if chunks := chunksForTest(transcript); len(chunks) != 1 ||
		chunks[0].Text != "欢迎大家来体验达摩院推出的语音识别模型。" {
		t.Errorf("chunks = %+v, want a single chunk holding the offline decode", chunks)
	}
}

func TestPlaceKeepsChunksOutsideTheRange(t *testing.T) {
	transcript := NewTranscript()
	transcript.AppendStream("前一句。", 0, 16000)
	transcript.AppendStream("后一句被替换。", 16000, 48000)

	transcript.Place(16000, 48000, "后一句。")

	if got := transcript.Text(); got != "前一句。后一句。" {
		t.Errorf("Text() = %q, want %q", got, "前一句。后一句。")
	}
}

func TestCommitProvisionalReturnsDeliveredText(t *testing.T) {
	transcript := NewTranscript()
	transcript.AppendStream("第一段。", 0, 16000)
	if got := transcript.CommitProvisional(); got != "第一段。" {
		t.Errorf("CommitProvisional() = %q, want %q", got, "第一段。")
	}
	if got := transcript.ProvisionalText(); got != "" {
		t.Errorf("ProvisionalText() after commit = %q, want empty", got)
	}
	if got := transcript.Text(); got != "第一段。" {
		t.Errorf("Text() = %q, want the committed text to still be the transcript", got)
	}
	// Repair after a commit has nothing to act on.
	transcript.Place(0, 16000, "改写")
	if got := transcript.Text(); got != "第一段。" {
		t.Errorf("Text() = %q, want committed text to survive", got)
	}
}

// A window that starts inside delivered audio is dropped. Reproduced on the wire:
// after a segment was committed at 4.50s, the next window covered [3.14s, 6.14s]
// and its text re-entered the transcript, so the committed sentence appeared
// underlined again as preedit and was then delivered a second time. A window is
// decoded as a unit, so its text cannot be split at the delivered boundary.
func TestAppendStreamDropsAWindowThatOverlapsDeliveredText(t *testing.T) {
	transcript := NewTranscript()
	transcript.AppendStream("第一句。", 0, 48000) // window 1: [0, 3.0s]
	transcript.Place(0, 45000, "第一句，修好了。")    // segment re-decoded: [0, 2.8s]
	if got := transcript.CommitProvisional(); got != "第一句，修好了。" {
		t.Fatalf("committed %q, want the repaired text", got)
	}

	// Window 2 covers [1.96s, 3.84s] and so straddles the delivered end at 2.8s.
	if transcript.AppendStream("又出现了一遍。", 31400, 61400) {
		t.Error("a window overlapping delivered text was accepted")
	}
	if got := transcript.ProvisionalText(); got != "" {
		t.Errorf("ProvisionalText() = %q, want nothing: delivered words must not return as preedit", got)
	}

	// Window 3 starts after the delivered end: kept, it is new speech.
	if !transcript.AppendStream("第二句。", 61400, 91400) {
		t.Error("a window after the delivered text was dropped")
	}
	if got := transcript.ProvisionalText(); got != "第二句。" {
		t.Errorf("ProvisionalText() = %q, want the new window", got)
	}
}

func TestSetSnapshotReplacesProvisional(t *testing.T) {
	transcript := NewTranscript()
	transcript.AppendStream("committed", 0, 16000)
	transcript.CommitProvisional()
	transcript.AppendStream("provisional one", 16000, 32000)
	transcript.AppendStream("provisional two", 32000, 48000)

	// A revision-capable model reports its whole hypothesis for the provisional
	// region; only that region is replaced.
	transcript.SetSnapshot("whole hypothesis", 16000, 48000)

	if got := transcript.Text(); got != "committedwhole hypothesis" {
		t.Errorf("Text() = %q, want %q", got, "committedwhole hypothesis")
	}
}

func TestEmptyDecodeKeepsStreamText(t *testing.T) {
	transcript := NewTranscript()
	transcript.AppendStream("", 0, 16000)
	if chunks := chunksForTest(transcript); len(chunks) != 0 {
		t.Errorf("chunks = %+v, want no chunks for empty text", chunks)
	}

	// An empty decode is indistinguishable from "nothing there", and dropping
	// text the user has already seen is the worse failure, so the stream text
	// stays. This is a deliberate limitation, not an oversight.
	transcript.AppendStream("real", 0, 16000)
	transcript.Place(0, 16000, "")
	if got := transcript.Text(); got != "real" {
		t.Errorf("Text() = %q, want the stream text kept when a repair returns nothing", got)
	}
	if chunks := chunksForTest(transcript); len(chunks) != 1 || chunks[0].Text != "real" {
		t.Errorf("chunks = %+v, want the stream chunk untouched", chunks)
	}
}

// A decode whose range the application already has must not be inserted again: a
// decode can land after the text was committed, and appending it would deliver the
// same words twice.
func TestPlaceDoesNotRedeliverDeliveredAudio(t *testing.T) {
	transcript := NewTranscript()
	transcript.AppendStream("delivered", 0, 16000)
	transcript.CommitProvisional()

	transcript.Place(0, 16000, "delivered, decoded offline")
	if got := transcript.Text(); got != "delivered" {
		t.Errorf("Text() = %q, want %q", got, "delivered")
	}
	if chunks := chunksForTest(transcript); len(chunks) != 1 {
		t.Errorf("chunks = %+v, want the delivered chunk alone", chunks)
	}
}

// A decode for a span no streaming chunk covers is *inserted*: the utterance closed
// before a window reached it, so the offline decode is the only reading of that
// audio rather than a correction of one.
func TestPlaceInsertsADecodeThatReplacesNothing(t *testing.T) {
	transcript := NewTranscript()

	transcript.Place(0, 16000, "短句。")

	if got := transcript.Text(); got != "短句。" {
		t.Fatalf("Text() = %q, want the inserted decode", got)
	}
	chunks := chunksForTest(transcript)
	if len(chunks) != 1 || chunks[0].Text != "短句。" {
		t.Fatalf("chunks = %+v, want one inserted decode chunk", chunks)
	}

	// A later decode lands after it, in sample order.
	transcript.Place(16000, 32000, "第二句。")
	if got := transcript.Text(); got != "短句。第二句。" {
		t.Fatalf("Text() = %q, want both decodes in order", got)
	}
}

// A repair must re-decode every chunk it is about to drop, not only the range the
// endpointer reported.
//
// This is the "only part of what I said was recorded" case. Repair drops a chunk
// that overlaps the range at all, and a streaming window is decoded as one unit,
// so the dropped chunk reaches past the range at both ends. Decoding only the
// range deleted the chunk's text for the overhang - the words the energy detector
// is least sure of, because it misses a quiet onset and stops at the last loud
// frame. RepairSpan is what makes the decode cover them.
func TestRepairSpanCoversTheChunksThatWillBeReplaced(t *testing.T) {
	transcript := NewTranscript()
	transcript.AppendStream("第一段", 0, 16000)
	transcript.AppendStream("第二段", 16000, 48000)

	// The detector heard speech only in [8000, 20000): it missed the quiet onset
	// and ended on the last loud frame.
	start, end, ok := transcript.RepairSpan(8000, 20000)
	if !ok {
		t.Fatal("RepairSpan found nothing to repair, but two chunks overlap the range")
	}
	if start != 0 || end != 48000 {
		t.Fatalf("RepairSpan = [%d,%d), want [0,48000): the union of the chunks the repair will replace", start, end)
	}

	// Repairing that span keeps every word: the decode covers exactly the audio it
	// replaces, so no text is silently discarded.
	transcript.Place(start, end, "第一段第二段，离线重读")
	if got := transcript.ProvisionalText(); got != "第一段第二段，离线重读" {
		t.Errorf("ProvisionalText() = %q, want the offline reading", got)
	}
	if chunks := chunksForTest(transcript); len(chunks) != 1 ||
		chunks[0].StartSample != 0 || chunks[0].EndSample != 48000 {
		t.Errorf("chunks = %+v, want one chunk spanning the whole replaced span", chunks)
	}
}

// A range that no provisional chunk covers needs no decode: the repair would have
// nothing to replace, and a process start plus a model load is worth avoiding.
func TestRepairSpanIsEmptyWhenNoProvisionalTextCoversTheRange(t *testing.T) {
	transcript := NewTranscript()
	transcript.AppendStream("delivered", 0, 16000)
	transcript.CommitProvisional()
	transcript.AppendStream("still provisional", 48000, 64000)

	if _, _, ok := transcript.RepairSpan(0, 16000); ok {
		t.Error("RepairSpan found a repair for a committed range")
	}
	if _, _, ok := transcript.RepairSpan(16000, 32000); ok {
		t.Error("RepairSpan found a repair for a range between chunks")
	}
	if _, _, ok := transcript.RepairSpan(48000, 64000); !ok {
		t.Error("RepairSpan found nothing for a range that is entirely provisional")
	}
}

func TestPlaceKeepsOrderWhenNewerTextExists(t *testing.T) {
	transcript := NewTranscript()
	transcript.AppendStream("older partial", 0, 16000)
	transcript.AppendStream("newer partial", 32000, 48000)

	// Repairing the earlier segment must insert in place, not append after newer
	// provisional text that arrived while the decode was running.
	transcript.Place(0, 16000, "corrected older")

	chunks := chunksForTest(transcript)
	if len(chunks) != 2 {
		t.Fatalf("chunks = %+v, want two chunks", chunks)
	}
	if chunks[0].Text != "corrected older" {
		t.Errorf("first chunk = %+v, want the repair result", chunks[0])
	}
	if chunks[1].Text != "newer partial" {
		t.Errorf("second chunk = %+v, want the newer partial after it", chunks[1])
	}
}

// The normalizer runs over the assembled text, not per chunk: a number can be
// split across two streaming windows, and only the joined form is a number.
func TestTranscriptNormalizesTheAssembledText(t *testing.T) {
	transcript := NewTranscript()
	transcript.SetNormalizer(func(text string) string {
		return strings.NewReplacer("一千", "1", "二百", "2", "臺灣", "台湾").Replace(text)
	})
	transcript.AppendStream("臺灣", 0, 16000)
	transcript.AppendStream("一千", 16000, 32000)
	transcript.AppendStream("二百", 32000, 48000)

	if got := transcript.ProvisionalText(); got != "台湾12" {
		t.Errorf("ProvisionalText() = %q, want the normalized joined text", got)
	}
	if got := transcript.CommitProvisional(); got != "台湾12" {
		t.Errorf("CommitProvisional() = %q, want the normalized joined text", got)
	}
	if got := transcript.Text(); got != "台湾12" {
		t.Errorf("Text() = %q, want the normalized committed text", got)
	}
	// The chunks stay as the model produced them, which is what logs and
	// measurement read.
	if chunks := chunksForTest(transcript); len(chunks) != 3 || chunks[1].Text != "一千" {
		t.Errorf("chunks = %+v, want the model's raw text kept", chunks)
	}
}
