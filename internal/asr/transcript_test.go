package asr

import "testing"

// The transcript model is where "correct the words already shown" is decided, so
// its rules are pinned down by tests: repair replaces provisional text only, and
// committing is a one-way door.
func TestRepairReplacesProvisionalOnly(t *testing.T) {
	transcript := NewTranscript()
	transcript.AppendStream("第一段。", 0, 16000)
	transcript.AppendStream("第二段。", 16000, 32000)
	transcript.Commit()

	// A repair that arrives after a commit must not touch committed text.
	transcript.Repair(0, 32000, "不该出现的修复")
	if got := transcript.Text(); got != "第一段。第二段。" {
		t.Errorf("after commit, Text() = %q, want committed text unchanged", got)
	}
}

func TestRepairReplacesOverlappingChunks(t *testing.T) {
	transcript := NewTranscript()
	transcript.AppendStream("欢迎大家来体验达摩院。", 15000, 48000)
	transcript.AppendStream("推出的语音识别模型。", 48000, 88000)

	// The endpoint detector reports the whole utterance; both partials fall
	// inside it, so both are replaced by the offline decode.
	transcript.Repair(15000, 88000, "欢迎大家来体验达摩院推出的语音识别模型。")

	if got := transcript.ProvisionalText(); got != "欢迎大家来体验达摩院推出的语音识别模型。" {
		t.Errorf("ProvisionalText() = %q", got)
	}
	if chunks := transcript.Chunks(); len(chunks) != 1 || chunks[0].Source != "repair" {
		t.Errorf("Chunks() = %+v, want a single repair chunk", chunks)
	}
}

func TestRepairKeepsChunksOutsideTheRange(t *testing.T) {
	transcript := NewTranscript()
	transcript.AppendStream("前一句。", 0, 16000)
	transcript.AppendStream("后一句被替换。", 16000, 48000)

	transcript.Repair(16000, 48000, "后一句。")

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
	if got := transcript.CommittedText(); got != "第一段。" {
		t.Errorf("CommittedText() = %q", got)
	}
	// Repair after a commit has nothing to act on.
	transcript.Repair(0, 16000, "改写")
	if got := transcript.Text(); got != "第一段。" {
		t.Errorf("Text() = %q, want committed text to survive", got)
	}
}

func TestSetSnapshotReplacesProvisional(t *testing.T) {
	transcript := NewTranscript()
	transcript.AppendStream("committed", 0, 16000)
	transcript.Commit()
	transcript.AppendStream("provisional one", 16000, 32000)
	transcript.AppendStream("provisional two", 32000, 48000)

	// A revision-capable model reports its whole hypothesis for the provisional
	// region; only that region is replaced.
	transcript.SetSnapshot("whole hypothesis", 16000, 48000)

	if got := transcript.Text(); got != "committedwhole hypothesis" {
		t.Errorf("Text() = %q, want %q", got, "committedwhole hypothesis")
	}
}

func TestProvisionalRange(t *testing.T) {
	transcript := NewTranscript()
	transcript.AppendStream("done", 0, 16000)
	transcript.Commit()
	if _, _, ok := transcript.ProvisionalRange(); ok {
		t.Error("ProvisionalRange() reported a range with nothing provisional")
	}
	transcript.AppendStream("pending", 16000, 64000)
	start, end, ok := transcript.ProvisionalRange()
	if !ok || start != 16000 || end != 64000 {
		t.Errorf("ProvisionalRange() = %d, %d, %v; want 16000, 64000, true", start, end, ok)
	}
}

func TestEmptyRepairKeepsStreamText(t *testing.T) {
	transcript := NewTranscript()
	transcript.AppendStream("", 0, 16000)
	if len(transcript.Chunks()) != 0 {
		t.Errorf("Chunks() = %+v, want no chunks for empty text", transcript.Chunks())
	}

	// An empty decode is indistinguishable from "nothing there", and dropping
	// text the user has already seen is the worse failure, so the stream text
	// stays. This is a deliberate limitation, not an oversight.
	transcript.AppendStream("real", 0, 16000)
	transcript.Repair(0, 16000, "")
	if got := transcript.Text(); got != "real" {
		t.Errorf("Text() = %q, want the stream text kept when a repair returns nothing", got)
	}
	if chunks := transcript.Chunks(); len(chunks) != 1 || chunks[0].Source != "stream" {
		t.Errorf("Chunks() = %+v, want the stream chunk untouched", chunks)
	}
}

func TestRepairWithNoProvisionalTextIsANoOp(t *testing.T) {
	transcript := NewTranscript()
	transcript.AppendStream("delivered", 0, 16000)
	transcript.Commit()

	// A repair that finishes after the user paused and the text was delivered
	// must not append its text a second time.
	transcript.Repair(0, 16000, "delivered, decoded offline")
	if got := transcript.Text(); got != "delivered" {
		t.Errorf("Text() = %q, want %q", got, "delivered")
	}
}

func TestRepairKeepsOrderWhenNewerTextExists(t *testing.T) {
	transcript := NewTranscript()
	transcript.AppendStream("older partial", 0, 16000)
	transcript.AppendStream("newer partial", 32000, 48000)

	// Repairing the earlier segment must insert in place, not append after newer
	// provisional text that arrived while the decode was running.
	transcript.Repair(0, 16000, "corrected older")

	chunks := transcript.Chunks()
	if len(chunks) != 2 {
		t.Fatalf("Chunks() = %+v, want two chunks", chunks)
	}
	if chunks[0].Source != "repair" || chunks[0].Text != "corrected older" {
		t.Errorf("first chunk = %+v, want the repair result", chunks[0])
	}
	if chunks[1].Text != "newer partial" {
		t.Errorf("second chunk = %+v, want the newer partial after it", chunks[1])
	}
}
