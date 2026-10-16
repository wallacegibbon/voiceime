package daemon

import (
	"strings"
	"testing"

	"github.com/wallacegibbon/voiceime/internal/asr"
	"github.com/wallacegibbon/voiceime/internal/audio"
)

// recordingSink captures what the session would have sent to an application.
type recordingSink struct{ events []string }

func (s *recordingSink) Commit(text string) error {
	s.events = append(s.events, "commit:"+text)
	return nil
}

func (s *recordingSink) Preedit(text string, cursor int, visible bool) error {
	s.events = append(s.events, "preedit:"+text)
	return nil
}

func (s *recordingSink) joined() string { return strings.Join(s.events, "|") }

// commits filters the events down to what the application would have inserted.
func (s *recordingSink) commits() []string {
	var out []string
	for _, event := range s.events {
		if text, found := strings.CutPrefix(event, "commit:"); found {
			out = append(out, text)
		}
	}
	return out
}

// preeditClears counts how often the composition was cleared, which must happen
// once per commit and never carry text.
func (s *recordingSink) preeditClears() int {
	count := 0
	for _, event := range s.events {
		if event == "preedit:" {
			count++
		}
	}
	return count
}

func newTestSession(sink Sink) *session {
	return &session{
		options:    Options{Sink: sink},
		transcript: asr.NewTranscript(),
		sink:       sink,
	}
}

// The delta rule is what keeps text from arriving twice: an application appends
// whatever it is given, so a commit that repeated earlier text would duplicate it
// in the document.
func TestCommitDeliversOnlyNewText(t *testing.T) {
	sink := &recordingSink{}
	session := newTestSession(sink)

	session.transcript.AppendStream("你好", 0, 16000)
	session.commitProvisional()
	session.transcript.AppendStream("世界", 16000, 32000)
	session.commitProvisional()

	got := sink.commits()
	if len(got) != 2 || got[0] != "你好" || got[1] != "世界" {
		t.Fatalf("commits = %v, want the new text only, never the whole transcript", got)
	}
}

// Committing again with nothing provisional must not insert an empty string, and
// the composition must be cleared first so the application does not keep showing
// text that has just been committed.
func TestCommitClearsTheCompositionAndIsSilentWhenEmpty(t *testing.T) {
	sink := &recordingSink{}
	session := newTestSession(sink)

	session.transcript.AppendStream("你好", 0, 16000)
	session.commitProvisional()
	session.commitProvisional()

	if got := sink.commits(); len(got) != 1 {
		t.Fatalf("commits = %v, want exactly one", got)
	}
	if got := sink.joined(); got != "preedit:|commit:你好|preedit:" {
		t.Fatalf("events = %q, want the composition cleared before each commit", got)
	}
}

// The preedit/commit split is the reason an application can show text that is
// still being corrected: while text is provisional it is a composition, and only
// at the boundary does it become real.
func TestPreeditShowsProvisionalText(t *testing.T) {
	sink := &recordingSink{}
	session := newTestSession(sink)

	session.transcript.AppendStream("你好", 0, 16000)
	if err := session.publishPreedit(); err != nil {
		t.Fatalf("publishPreedit: %v", err)
	}
	session.commitProvisional()

	want := "preedit:你好|preedit:|commit:你好"
	if got := sink.joined(); got != want {
		t.Fatalf("events = %q, want %q", got, want)
	}
	if clears := sink.preeditClears(); clears != 1 {
		t.Fatalf("composition clears = %d, want 1", clears)
	}
}

// Text that a repair rewrote must be committed in its repaired form, because the
// repair is the only reason a finished utterance is worth more than the stream.
func TestRepairedTextIsWhatGetsCommitted(t *testing.T) {
	sink := &recordingSink{}
	session := newTestSession(sink)

	session.transcript.AppendStream("欢迎大家来体验达摩院。", 15000, 48000)
	session.transcript.AppendStream("推出的语音识别模型。", 48000, 88000)
	session.transcript.Repair(15000, 88000, "欢迎大家来体验达摩院推出的语音识别模型。")
	session.commitProvisional()

	got := sink.commits()
	if len(got) != 1 || got[0] != "欢迎大家来体验达摩院推出的语音识别模型。" {
		t.Fatalf("commits = %v, want the repaired sentence", got)
	}
}

// A stream that produced nothing provisional must not commit anything: an empty
// utterance (a cough, a door) should reach the application as nothing at all.
func TestSilentUtteranceCommitsNothing(t *testing.T) {
	sink := &recordingSink{}
	session := newTestSession(sink)

	session.commitProvisional()

	if got := sink.commits(); len(got) != 0 {
		t.Fatalf("commits = %v, want none", got)
	}
}

// The repair decodes every chunk it will replace, not just the endpointer's range,
// so a word the detector was unsure about is re-read instead of dropped.
//
// The endpointer is the weakest part of the pipeline: an energy detector misses a
// quiet onset and stops at the last loud frame, so its range is narrower than the
// streaming chunks that overlap it. Repair drops those chunks whole, so decoding
// only the range deletes their text for the overhang - the reported "only part of
// what I said was recorded".
func TestSegmentEndRepairsEverythingItWillReplace(t *testing.T) {
	sink := &recordingSink{}
	session := newTestSession(sink)
	session.transcript.AppendStream("第一段", 0, 16000)
	session.transcript.AppendStream("第二段", 16000, 48000)

	// Speech the detector is sure about, inside chunks that reach half a second
	// before it and a second and a half after it.
	event := audio.SegmentEvent{Kind: audio.SpeechEnd, StartSample: 8000, EndSample: 20000}
	buffer := audio.NewRollingBuffer(5)
	buffer.Append(make([]byte, 48000*audio.BytesPerSample), 0)

	// A queue means repair is on, so onSegmentEnd takes the repair path.
	session.repairQueue = make(chan repairJob, 1)
	session.options.MinRepairSeconds = 0.1
	session.buffer = buffer
	session.onSegmentEnd(event)

	select {
	case job := <-session.repairQueue:
		if job.startSample != 0 || job.endSample != 48000 {
			t.Errorf("repair span = [%d,%d), want [0,48000): the decode must cover every chunk it replaces",
				job.startSample, job.endSample)
		}
		if want := 48000 * audio.BytesPerSample; len(job.pcm) != want {
			t.Errorf("repair audio = %d bytes, want %d", len(job.pcm), want)
		}
	default:
		t.Fatal("no repair was queued, so the utterance would commit unrepaired")
	}
	if got := sink.commits(); len(got) != 0 {
		t.Errorf("commits = %v, want none: the utterance is still being repaired", got)
	}
}

// An utterance whose text the streaming layer has not produced yet must NOT be
// committed at its boundary.
//
// Committing there delivers nothing and forgets the segment, which is the reported
// "the text stays preedit and never commits": the text arrives later, and by then
// no boundary is left to release it. The segment is held instead, and released
// when a covering window arrives.
func TestSegmentEndWithoutTextIsHeldForLater(t *testing.T) {
	sink := &recordingSink{}
	session := newTestSession(sink)
	session.repairQueue = make(chan repairJob, 4)
	session.options.MinRepairSeconds = 0.1
	session.buffer = audio.NewRollingBuffer(5)
	session.buffer.Append(make([]byte, 48000*audio.BytesPerSample), 0)

	event := audio.SegmentEvent{Kind: audio.SpeechEnd, StartSample: 8000, EndSample: 20000}
	session.onSegmentEnd(event)

	if len(session.pending) != 1 {
		t.Fatalf("pending = %v, want the segment held for its text", session.pending)
	}
	select {
	case job := <-session.repairQueue:
		t.Fatalf("queued a repair for an utterance with no text: %+v", job)
	default:
	}
	if got := sink.commits(); len(got) != 0 {
		t.Fatalf("commits = %v, want none: the segment has no text to deliver yet", got)
	}
}

// The held segment is released as soon as a window covering it arrives. This is
// the fix itself: a short utterance closes before the recogniser has decoded it,
// and used to be forgotten at that point.
func TestHeldSegmentIsReleasedWhenItsWindowArrives(t *testing.T) {
	sink := &recordingSink{}
	session := newTestSession(sink)
	session.repairQueue = make(chan repairJob, 4)
	session.options.MinRepairSeconds = 0.1
	session.buffer = audio.NewRollingBuffer(5)
	session.buffer.Append(make([]byte, 48000*audio.BytesPerSample), 0)

	event := audio.SegmentEvent{Kind: audio.SpeechEnd, StartSample: 8000, EndSample: 20000}
	session.onSegmentEnd(event)
	if len(session.pending) != 1 {
		t.Fatalf("pending = %v, want the segment held", session.pending)
	}

	// Its window arrives: a chunk that covers the utterance.
	session.transcript.AppendStream("你好", 0, 32000)
	session.resolvePending()

	select {
	case job := <-session.repairQueue:
		if job.startSample > 8000 || job.endSample < 20000 {
			t.Errorf("repair span = [%d,%d), want it to cover the utterance [8000,20000)",
				job.startSample, job.endSample)
		}
	default:
		t.Fatal("the held segment was not released when its window arrived")
	}
	if len(session.pending) != 0 {
		t.Errorf("pending = %v, want the held segment released", session.pending)
	}
}

// A held segment whose audio a later union already delivered is dropped, not
// decoded again: a later segment's repair decodes every chunk it replaces, so it
// can carry the earlier utterance out with it, and re-decoding it would deliver
// the same words twice.
func TestHeldSegmentAlreadyDeliveredIsDropped(t *testing.T) {
	sink := &recordingSink{}
	session := newTestSession(sink)
	session.repairQueue = make(chan repairJob, 4)
	session.options.MinRepairSeconds = 0.1
	session.buffer = audio.NewRollingBuffer(5)
	session.buffer.Append(make([]byte, 48000*audio.BytesPerSample), 0)

	session.pending = append(session.pending,
		audio.SegmentEvent{Kind: audio.SpeechEnd, StartSample: 0, EndSample: 16000})
	session.transcript.AppendStream("你好", 0, 16000)
	session.commitProvisional()

	session.resolvePending()

	if len(session.pending) != 0 {
		t.Fatalf("pending = %v, want the already-delivered segment dropped", session.pending)
	}
	select {
	case job := <-session.repairQueue:
		t.Fatalf("queued a repair for audio that was already delivered: %+v", job)
	default:
	}
}
