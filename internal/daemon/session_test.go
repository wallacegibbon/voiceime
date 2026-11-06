package daemon

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

// preedits lists the compositions that were shown, so a test can assert that a guess never
// reached the caret.
func (s *recordingSink) preedits() []string {
	var out []string
	for _, event := range s.events {
		if text, found := strings.CutPrefix(event, "preedit:"); found && text != "" {
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
	session.transcript.Place(15000, 88000, "欢迎大家来体验达摩院推出的语音识别模型。")
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

// An utterance the streaming layer has not reached must still be decoded: the
// offline pass is the reader for an accepted utterance, not only a corrector of
// streaming text. Leaving it out is what made a short sentence sit in preedit, its
// boundary passing before any window covered it.
//
// The range is the utterance widened by padSamples: the endpointer ends on the last
// loud frame, so its own range can clip a quiet tail. See resolveSegment.
func TestSegmentEndWithNoStreamingTextDecodesItsOwnAudio(t *testing.T) {
	sink := &recordingSink{}
	session := newTestSession(sink)
	session.repairQueue = make(chan repairJob, 4)
	session.options.MinRepairSeconds = 0.1
	session.buffer = audio.NewRollingBuffer(5)
	session.buffer.Append(make([]byte, 48000*audio.BytesPerSample), 0)
	session.padSamples = 1600 // 100 ms at 16 kHz

	event := audio.SegmentEvent{Kind: audio.SpeechEnd, StartSample: 16000, EndSample: 32000}
	session.onSegmentEnd(event)

	select {
	case job := <-session.repairQueue:
		if job.startSample != 16000-1600 || job.endSample != 32000+1600 {
			t.Fatalf("decode span = [%d,%d), want [%d,%d): the utterance widened by padSamples",
				job.startSample, job.endSample, 16000-1600, 32000+1600)
		}
	default:
		t.Fatal("no decode was queued: the utterance would never commit")
	}
	if got := sink.commits(); len(got) != 0 {
		t.Fatalf("commits = %v, want none: the decode has not landed yet", got)
	}
}

// An utterance at the very start of a session must not ask for audio before zero.
func TestSegmentEndClampsTheDecodeRangeAtZero(t *testing.T) {
	session := newTestSession(&recordingSink{})
	session.repairQueue = make(chan repairJob, 4)
	session.options.MinRepairSeconds = 0.1
	session.buffer = audio.NewRollingBuffer(5)
	session.buffer.Append(make([]byte, 48000*audio.BytesPerSample), 0)
	session.padSamples = 1600

	event := audio.SegmentEvent{Kind: audio.SpeechEnd, StartSample: 500, EndSample: 20000}
	session.onSegmentEnd(event)

	select {
	case job := <-session.repairQueue:
		if job.startSample != 0 {
			t.Fatalf("decode span starts at %d, want 0", job.startSample)
		}
	default:
		t.Fatal("no decode was queued")
	}
}

// A session that stops while the user is still speaking has no trailing silence
// for the decode range to widen into: the segmenter's flush closes the last
// utterance on the last loud frame, so the padded range overruns the recording by
// up to padSamples. Refusing to decode there delivered the streaming draft for the
// last sentence of every session that ends mid-speech - the case reproduced on the
// reference machine, where the second of two utterances came back
// "skipped: segment too long to repair" on three runs out of three.
//
// The clip in that reproduction is 4.24s of audio ending in speech: the utterance
// runs to 4.08s, padSamples is 0.4s, and the buffer holds 4.24s.
func TestSegmentEndClipsThePadToTheAudioThatExists(t *testing.T) {
	sink := &recordingSink{}
	session := newTestSession(sink)
	session.repairQueue = make(chan repairJob, 4)
	session.options.MinRepairSeconds = 0.1
	session.buffer = audio.NewRollingBuffer(63)
	session.buffer.Append(make([]byte, 67840*audio.BytesPerSample), 0) // 4.24s fed
	session.padSamples = 6400                                          // 0.4s at 16 kHz

	event := audio.SegmentEvent{Kind: audio.SpeechEnd, StartSample: 47360, EndSample: 65280}
	session.onSegmentEnd(event)

	select {
	case job := <-session.repairQueue:
		// The start pad is inside the buffer (which holds from 0), so only the end
		// is clipped: [47360-6400, 67840) rather than [40960, 71680).
		if job.startSample != 40960 || job.endSample != 67840 {
			t.Fatalf("decode span = [%d,%d), want [40960,67840): the utterance widened by "+
				"as much pad as the audio allows", job.startSample, job.endSample)
		}
		if want := (67840 - 40960) * audio.BytesPerSample; len(job.pcm) != want {
			t.Errorf("decode audio = %d bytes, want %d", len(job.pcm), want)
		}
	default:
		t.Fatalf("no decode was queued, so the last sentence would commit unrepaired: %v", sink.events)
	}
}

// Clipping is only for the widened range. When a provisional chunk is what makes
// the span long, the decode has to re-read everything that chunk's text covers -
// decoding less would delete that text without replacing it - so a span that fell
// out of the buffer is still skipped.
func TestSegmentEndStillSkipsASpanThatFellOutOfTheBuffer(t *testing.T) {
	sink := &recordingSink{}
	session := newTestSession(sink)
	// A window partial covering from the start of the session: the repair span is
	// the union with the utterance, and the window start is no longer held.
	session.transcript.AppendStream("开头的一段话。", 0, 60000)
	session.repairQueue = make(chan repairJob, 4)
	session.options.MinRepairSeconds = 0.1
	session.buffer = audio.NewRollingBuffer(63)
	session.buffer.Append(make([]byte, 40000*audio.BytesPerSample), 20000) // held [20000,60000)
	session.padSamples = 6400

	event := audio.SegmentEvent{Kind: audio.SpeechEnd, StartSample: 40000, EndSample: 55000}
	session.onSegmentEnd(event)

	select {
	case job := <-session.repairQueue:
		t.Fatalf("queued a decode for [%d,%d): the range must cover the chunk's text "+
			"or the repair deletes it", job.startSample, job.endSample)
	default:
	}
	if got := sink.commits(); len(got) != 1 {
		t.Fatalf("commits = %v, want the stream text: the repair cannot run", got)
	}
}

// newMergeTestSession wires a session whose only job is to exercise the coalescer:
// a queue to catch what it dispatches, and a buffer holding the audio a decode would
// read.
func newMergeTestSession(t *testing.T, window time.Duration, maxGroup int64) *session {
	t.Helper()
	session := newTestSession(&recordingSink{})
	session.repairQueue = make(chan repairJob, 4)
	session.buffer = audio.NewRollingBuffer(63)
	session.buffer.Append(make([]byte, 96000*audio.BytesPerSample), 0)
	session.mergeWindow = window
	session.maxGroupSamples = maxGroup
	return session
}

// nextRepairJob waits for one queued decode.
func nextRepairJob(t *testing.T, session *session, within time.Duration) repairJob {
	t.Helper()
	select {
	case job := <-session.repairQueue:
		return job
	case <-time.After(within):
		t.Fatalf("no decode was queued within %v", within)
		return repairJob{}
	}
}

// A pause the endpointer heard is not a sentence end. Two segments that follow each
// other within the merge window must reach the offline model as one request, which is
// the whole point: decoded apart, each half loses the other's context and the text
// comes back as it was cut. This is the reported "it always cuts in the wrong place".
func TestPausesWithinTheMergeWindowDecodeAsOneGroup(t *testing.T) {
	session := newMergeTestSession(t, 60*time.Millisecond, 0)

	session.onSegmentEnd(audio.SegmentEvent{Kind: audio.SpeechEnd, StartSample: 16000, EndSample: 32000})
	session.onSegmentEnd(audio.SegmentEvent{Kind: audio.SpeechEnd, StartSample: 40000, EndSample: 56000})

	// Nothing may be decoded while the speaker may still pick the sentence up.
	select {
	case job := <-session.repairQueue:
		t.Fatalf("decoded [%d,%d) during the window: a pause was taken for a sentence end", job.startSample, job.endSample)
	case <-time.After(30 * time.Millisecond):
	}

	job := nextRepairJob(t, session, 2*time.Second)
	if job.startSample != 16000 || job.endSample != 56000 {
		t.Fatalf("decode span = [%d,%d), want [16000,56000): the group must span both pauses",
			job.startSample, job.endSample)
	}
}

// The window is measured in silence, not in the events that report it.
//
// Measured on a real recording: a one-second pause was followed by a five-second
// sentence; an event-driven countdown kept running through that sentence and split it off
// the moment it ended. Silence is a property of the audio, so the audio is what gets
// measured: while the detector keeps hearing speech, the group stays open however long
// that goes on.
func TestTheMergeWindowIsMeasuredInSilence(t *testing.T) {
	session := newMergeTestSession(t, 60*time.Millisecond, 0)
	var heardTo atomic.Int64
	session.heardSpeech = func() (int64, int64) { return 0, heardTo.Load() }
	session.mergeSamples = int64(session.mergeWindow.Seconds() * audio.SampleRate)

	session.lastSample.Store(32000)
	heardTo.Store(32000) // speech up to the last sample fed
	session.onSegmentEnd(audio.SegmentEvent{Kind: audio.SpeechEnd, StartSample: 0, EndSample: 16000})

	// Past the wall-clock window many times over: the speaker is still going.
	time.Sleep(150 * time.Millisecond)
	select {
	case job := <-session.repairQueue:
		t.Fatalf("decoded [%d,%d) while the speaker was still being heard", job.startSample, job.endSample)
	default:
	}

	// The speaker stops; the audio keeps arriving, so the silence grows with it.
	session.lastSample.Store(32000 + session.mergeSamples)
	job := nextRepairJob(t, session, 2*time.Second)
	if job.startSample != 0 || job.endSample != 16000 {
		t.Fatalf("decode span = [%d,%d), want [0,16000)", job.startSample, job.endSample)
	}
}

// A burst too short to be an utterance must not postpone the group for good.
//
// This is the reported "it often does not finish by itself": a click or a breath opens a
// speech region, the region closes without an end event (it is shorter than
// endpoint_min_speech_ms), and a countdown that waited for the next event never fired
// again. The text stayed in preedit until the key was pressed. Measured in silence, the
// burst only postpones the group by the window it started.
func TestAShortBurstDoesNotPostponeTheGroupForGood(t *testing.T) {
	session := newMergeTestSession(t, 60*time.Millisecond, 0)
	var heardTo atomic.Int64
	session.heardSpeech = func() (int64, int64) { return 0, heardTo.Load() }
	session.mergeSamples = int64(session.mergeWindow.Seconds() * audio.SampleRate)

	session.lastSample.Store(16000)
	heardTo.Store(15000)
	session.onSegmentEnd(audio.SegmentEvent{Kind: audio.SpeechEnd, StartSample: 0, EndSample: 16000})

	// A click 20ms before the end of the audio, and then nothing.
	heardTo.Store(16000)
	time.Sleep(30 * time.Millisecond)

	// The audio keeps arriving with no further speech in it.
	session.lastSample.Store(16000 + session.mergeSamples)
	job := nextRepairJob(t, session, 2*time.Second)
	if job.startSample != 0 || job.endSample != 16000 {
		t.Fatalf("decode span = [%d,%d), want [0,16000)", job.startSample, job.endSample)
	}
}

// A pause longer than the window is a sentence end// A pause longer than the window is a sentence end, and each group is decoded on its
// own - otherwise nothing would ever be delivered before the next thing is said.
func TestPausesBeyondTheMergeWindowDecodeSeparately(t *testing.T) {
	session := newMergeTestSession(t, 30*time.Millisecond, 0)

	session.onSegmentEnd(audio.SegmentEvent{Kind: audio.SpeechEnd, StartSample: 1000, EndSample: 9000})
	first := nextRepairJob(t, session, 2*time.Second)
	if first.startSample != 1000 || first.endSample != 9000 {
		t.Fatalf("first decode = [%d,%d), want [1000,9000)", first.startSample, first.endSample)
	}

	session.onSegmentEnd(audio.SegmentEvent{Kind: audio.SpeechEnd, StartSample: 50000, EndSample: 58000})
	second := nextRepairJob(t, session, 2*time.Second)
	if second.startSample != 50000 || second.endSample != 58000 {
		t.Fatalf("second decode = [%d,%d), want [50000,58000)", second.startSample, second.endSample)
	}
}

// A group cannot grow past what the buffer holds: one decode has to be able to read
// all of it, or the text would have to be delivered from the draft. The cap ends the
// group instead, which is also what bounds how long one refine may take.
func TestMergeGroupIsCappedAtTheLongestUtterance(t *testing.T) {
	session := newMergeTestSession(t, time.Hour, 20000)

	session.onSegmentEnd(audio.SegmentEvent{Kind: audio.SpeechEnd, StartSample: 0, EndSample: 16000})
	session.onSegmentEnd(audio.SegmentEvent{Kind: audio.SpeechEnd, StartSample: 16000, EndSample: 32000})

	// The first group is released even though the window has not elapsed, because
	// adding the second segment would exceed the cap.
	capped := nextRepairJob(t, session, time.Second)
	if capped.startSample != 0 || capped.endSample != 16000 {
		t.Fatalf("capped decode = [%d,%d), want [0,16000)", capped.startSample, capped.endSample)
	}
	select {
	case job := <-session.repairQueue:
		t.Fatalf("a second group was decoded while it was still open: [%d,%d)", job.startSample, job.endSample)
	default:
	}

	// The new group is still pending, and a stop releases it at once.
	session.flushPending()
	rest := nextRepairJob(t, session, time.Second)
	if rest.startSample != 16000 || rest.endSample != 32000 {
		t.Fatalf("released decode = [%d,%d), want [16000,32000)", rest.startSample, rest.endSample)
	}
}

// A session that stops releases whatever the coalescer holds, without waiting out the
// window: the stop is itself the end of the sentence, and the merge window exists only
// to decide whether a *pause* is one.
func TestStoppingFlushesThePendingGroup(t *testing.T) {
	session := newMergeTestSession(t, time.Hour, 0)
	session.onSegmentEnd(audio.SegmentEvent{Kind: audio.SpeechEnd, StartSample: 16000, EndSample: 32000})

	session.flushPending()
	job := nextRepairJob(t, session, time.Second)
	if job.startSample != 16000 || job.endSample != 32000 {
		t.Fatalf("decode = [%d,%d), want the pending group", job.startSample, job.endSample)
	}
}

// A timer that fires after the session has ended must not deliver anything: the
// session has already committed its last text, and a late decode would insert it
// again into whatever the user is doing by then.
func TestLateMergeTimerDoesNotDispatchAfterTheSessionEnds(t *testing.T) {
	session := newMergeTestSession(t, 40*time.Millisecond, 0)
	session.onSegmentEnd(audio.SegmentEvent{Kind: audio.SpeechEnd, StartSample: 16000, EndSample: 32000})
	session.finished.Store(true)
	session.flushPending()

	select {
	case job := <-session.repairQueue:
		t.Fatalf("queued [%d,%d) after the session ended", job.startSample, job.endSample)
	case <-time.After(150 * time.Millisecond):
	}
}

// The wait for in-flight repairs is bounded.
//
// A repair no longer inherits the session's cancellation - that is what lets the
// utterance a stop closes still be decoded - so without a budget the wait would last
// as long as the transcriber's own timeout (30s by default). The engine's Disable is a
// D-Bus call ibus-daemon is waiting on, so an unbounded wait there is a hung input
// method, not a slow one.
func TestFinishRepairsGivesUpAfterItsBudget(t *testing.T) {
	sink := &recordingSink{}
	session := newTestSession(sink)
	session.repairQueue = make(chan repairJob, 1)
	session.repairDone.Add(1)

	release := make(chan struct{})
	go func() {
		defer session.repairDone.Done()
		<-release // a decode that does not answer
	}()
	defer close(release)
	session.repairQueue <- repairJob{}

	started := time.Now()
	err := session.finishRepairs(50 * time.Millisecond)
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("finishRepairs reported success with a decode still running")
	}
	if elapsed > 2*time.Second {
		t.Errorf("finishRepairs waited %v for a 50ms budget", elapsed)
	}
	if !strings.Contains(err.Error(), "draft") {
		t.Errorf("error %q does not say the sentence was delivered from the draft", err)
	}
	// Closing twice must not panic: the normal shutdown path and an early return can
	// both end a session.
	if err := session.finishRepairs(50 * time.Millisecond); err == nil {
		t.Error("a second call reported success while the decode was still running")
	}
}
