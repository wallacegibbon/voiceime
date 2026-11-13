package daemon

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wallacegibbon/voiceime/internal/asr"
	"github.com/wallacegibbon/voiceime/internal/audio"
	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/ibus"
)

// graveKeyval is the key of the default trigger, <Control>grave.
const graveKeyval = 0x0060

// recordingControl stands in for one input context: it records what the handler tried
// to deliver, and which context received it.
type recordingControl struct {
	mu      sync.Mutex
	commits []string
}

func (c *recordingControl) Commit(text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.commits = append(c.commits, text)
	return nil
}

func (c *recordingControl) Preedit(text string, cursor int, visible bool) error { return nil }

func (c *recordingControl) DeleteSurroundingText(offset int32, chars uint32) error { return nil }

func (c *recordingControl) delivered() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.commits...)
}

type fakeCapture struct {
	chunks chan audio.Chunk

	mu      sync.Mutex
	started bool
	stopped bool
	samples int64
}

func newFakeCapture() *fakeCapture {
	return &fakeCapture{chunks: make(chan audio.Chunk, 256)}
}

func (c *fakeCapture) Start() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.started = true
	return nil
}

func (c *fakeCapture) Chunks() <-chan audio.Chunk { return c.chunks }

func (c *fakeCapture) Err() error { return nil }

func (c *fakeCapture) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stopped {
		return
	}
	c.stopped = true
	close(c.chunks)
}

func (c *fakeCapture) isStarted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.started
}

// publish hands over audio in the 20ms blocks a real capture produces, so the collector
// sees the granularity it sees from a microphone.
func (c *fakeCapture) publish(data []byte) {
	block := audio.FrameMS * audio.SampleRate / 1000 * audio.BytesPerSample
	for offset := 0; offset < len(data); offset += block {
		end := min(offset+block, len(data))
		c.mu.Lock()
		if c.stopped {
			c.mu.Unlock()
			return
		}
		chunk := audio.Chunk{Data: append([]byte(nil), data[offset:end]...), StartSample: c.samples}
		c.samples += int64(audio.SampleCount(chunk.Data))
		c.chunks <- chunk
		c.mu.Unlock()
	}
}

// captureSource gives the handler a fresh fake capture per hold and lets the test find
// the one in flight, which is what a microphone would do.
type captureSource struct {
	mu     sync.Mutex
	latest *fakeCapture
	holds  int
}

func (s *captureSource) NewCapture() (audio.Capture, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.holds++
	s.latest = newFakeCapture()
	return s.latest, nil
}

func (s *captureSource) current() *fakeCapture {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.latest
}

func (s *captureSource) started() bool {
	if capture := s.current(); capture != nil {
		return capture.isStarted()
	}
	return false
}

func (s *captureSource) publish(data []byte) {
	if capture := s.current(); capture != nil {
		capture.publish(data)
	}
}

func (s *captureSource) recordings() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.holds
}

// offlineTranscriber answers the offline decode without a model, and keeps what it was
// asked to read.
type offlineTranscriber struct {
	mu   sync.Mutex
	pcm  [][]byte
	text string
	err  error
}

func (f *offlineTranscriber) Transcribe(ctx context.Context, pcm []byte) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pcm = append(f.pcm, append([]byte(nil), pcm...))
	if f.err != nil {
		return "", f.err
	}
	if f.text != "" {
		return f.text, nil
	}
	return "你好。", nil
}

// calls is one entry per decode request.
func (f *offlineTranscriber) calls() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte(nil), f.pcm...)
}

// logCollector keeps what the handler wrote, for the assertions about what a user would
// find in the engine's log.
type logCollector struct {
	mu    sync.Mutex
	lines []string
}

func (l *logCollector) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(format, args...))
}

func (l *logCollector) text() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// newTestPushToTalk builds the handler the way the engine process does, with the model
// replaced by a stand-in. The capture source is the test's: it is what the test drives.
func newTestPushToTalk(t *testing.T) (*PushToTalk, *captureSource, *offlineTranscriber) {
	t.Helper()
	handler, err := NewPushToTalk(config.Default(), nil)
	if err != nil {
		t.Fatalf("NewPushToTalk: %v", err)
	}
	source := &captureSource{}
	transcriber := &offlineTranscriber{}
	handler.NewCapture = source.NewCapture
	handler.NewTranscriber = func() asr.Transcriber { return transcriber }
	return handler, source, transcriber
}

// waitFor polls a condition, so a test does not depend on how long a goroutine takes.
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestPushToTalkRecordsOnlyWhileTheKeyIsHeld(t *testing.T) {
	handler, source, transcriber := newTestPushToTalk(t)
	control := &recordingControl{}
	handler.Enable(control)

	if handler.KeyEvent('a', 0, 0) {
		t.Error("an ordinary key was consumed")
	}
	if handler.KeyEvent(graveKeyval, 0, 0) {
		t.Error("a bare key was consumed, but that is ordinary typing")
	}
	if source.recordings() != 0 {
		t.Fatalf("a capture was started before the trigger was pressed (%d)", source.recordings())
	}

	if !handler.KeyEvent(graveKeyval, 0, ibus.ModControl) {
		t.Fatal("the trigger press was not consumed")
	}
	waitFor(t, "the capture to start", source.started)

	source.publish(append(append(silence(0.1), tone(0.6, 6000)...), silence(0.1)...))

	// The daemon reports every repeat of a held key as another press.
	if !handler.KeyEvent(graveKeyval, 0, ibus.ModControl) {
		t.Error("the auto-repeat of the held key was not consumed")
	}
	if got := source.recordings(); got != 1 {
		t.Errorf("the auto-repeat started %d recordings, want one hold to be one recording", got)
	}

	if !handler.KeyEvent(graveKeyval, 0, ibus.ModControl|ibus.ReleaseMask) {
		t.Error("the trigger release was not consumed")
	}
	waitFor(t, "the recording to be decoded", func() bool { return len(transcriber.calls()) == 1 })

	if got := control.delivered(); len(got) != 1 || got[0] != "你好" {
		t.Errorf("the application received %v, want the decoded sentence once, without its trailing period", got)
	}
}

// The request is the recording, byte for byte.
func TestPushToTalkSendsTheRecordingItMade(t *testing.T) {
	handler, source, transcriber := newTestPushToTalk(t)
	control := &recordingControl{}
	handler.Enable(control)

	handler.KeyEvent(graveKeyval, 0, ibus.ModControl)
	waitFor(t, "the capture to start", source.started)
	hold := append(append(silence(0.2), tone(0.6, 6000)...), append(silence(1.2), tone(0.6, 6000)...)...)
	hold = append(hold, silence(0.3)...)
	source.publish(hold)
	handler.KeyEvent(graveKeyval, 0, ibus.ModControl|ibus.ReleaseMask)

	waitFor(t, "the recording to be decoded", func() bool { return len(transcriber.calls()) == 1 })
	sent := transcriber.calls()[0]
	if !bytes.Equal(sent, hold) {
		t.Errorf("the decode was asked to read %d bytes of a %d byte recording: the audio was altered",
			len(sent), len(hold))
	}
	// One hold is one request, whatever its length: a cut costs the model the context
	// across it, so there is nothing to gain by sending the recording in pieces.
	if calls := len(transcriber.calls()); calls != 1 {
		t.Errorf("a %.1fs take was sent as %d requests, want one", audio.DurationSeconds(hold), calls)
	}
	if got := control.delivered(); len(got) != 1 {
		t.Errorf("the application received %v, want one sentence", got)
	}
}

// A long hold is still one request: the recording is never cut up to deliver it sooner.
func TestPushToTalkSendsALongHoldAsOneRequest(t *testing.T) {
	handler, source, transcriber := newTestPushToTalk(t)
	control := &recordingControl{}
	handler.Enable(control)

	handler.KeyEvent(graveKeyval, 0, ibus.ModControl)
	waitFor(t, "the capture to start", source.started)
	var hold []byte
	for phrase := 0; phrase < 6; phrase++ {
		hold = append(hold, tone(2.5, 6000)...)
		hold = append(hold, silence(0.9)...)
	}
	source.publish(hold)
	handler.KeyEvent(graveKeyval, 0, ibus.ModControl|ibus.ReleaseMask)

	waitFor(t, "the recording to be decoded", func() bool { return len(transcriber.calls()) == 1 })
	if calls := len(transcriber.calls()); calls != 1 {
		t.Fatalf("a %.1fs take was sent as %d requests, want one", audio.DurationSeconds(hold), calls)
	}
	if sent := transcriber.calls()[0]; !bytes.Equal(sent, hold) {
		t.Errorf("the request holds %d bytes of a %d byte recording", len(sent), len(hold))
	}
	if got := len(control.delivered()); got != 1 {
		t.Errorf("%d commits for one take", got)
	}
}

// Punctuation is not a transcript: it is what a recogniser emits for audio it could not
// read, and typing it into the application is worse than typing nothing.
func TestPushToTalkCommitsNothingForPunctuation(t *testing.T) {
	handler, source, transcriber := newTestPushToTalk(t)
	transcriber.text = "。"
	logs := &logCollector{}
	handler.Log = logs.logf
	control := &recordingControl{}
	handler.Enable(control)

	handler.KeyEvent(graveKeyval, 0, ibus.ModControl)
	waitFor(t, "the capture to start", source.started)
	source.publish(tone(0.6, 6000))
	handler.KeyEvent(graveKeyval, 0, ibus.ModControl|ibus.ReleaseMask)

	waitFor(t, "the decode to come back", func() bool { return len(transcriber.calls()) == 1 })
	waitFor(t, "the log to say punctuation was dropped", func() bool {
		return strings.Contains(logs.text(), "only punctuation")
	})
	if got := control.delivered(); len(got) != 0 {
		t.Errorf("the application received %v for a punctuation-only answer", got)
	}
}

// A recording the model finds no words in commits nothing, and says so.
func TestPushToTalkCommitsNothingWhenTheModelHearsNothing(t *testing.T) {
	handler, source, transcriber := newTestPushToTalk(t)
	transcriber.text = "   "
	logs := &logCollector{}
	handler.Log = logs.logf
	control := &recordingControl{}
	handler.Enable(control)

	handler.KeyEvent(graveKeyval, 0, ibus.ModControl)
	waitFor(t, "the capture to start", source.started)
	// Speech, so the recording passes the gate and the model is the one answering blank.
	source.publish(tone(0.6, 6000))
	handler.KeyEvent(graveKeyval, 0, ibus.ModControl|ibus.ReleaseMask)

	waitFor(t, "the decode to come back empty", func() bool { return len(transcriber.calls()) == 1 })
	waitFor(t, "the log to say no words were heard", func() bool {
		return strings.Contains(logs.text(), "heard no words")
	})
	if got := control.delivered(); len(got) != 0 {
		t.Errorf("the application received %v for an empty answer", got)
	}
}

// A modifier coming up before the key ends the hold, and its release is not consumed.
func TestPushToTalkStopsWhenTheModifierComesUpFirst(t *testing.T) {
	handler, source, transcriber := newTestPushToTalk(t)
	control := &recordingControl{}
	handler.Enable(control)

	handler.KeyEvent(graveKeyval, 0, ibus.ModControl)
	waitFor(t, "the capture to start", source.started)
	source.publish(tone(0.5, 6000))

	const controlL = 0xffe3
	if handler.KeyEvent(controlL, 0, ibus.ModControl|ibus.ReleaseMask) {
		t.Error("the modifier release was consumed, which leaves the modifier latched")
	}
	waitFor(t, "the recording to be decoded", func() bool { return len(transcriber.calls()) == 1 })

	// The space release that follows has no press behind it, so it passes through and
	// must not start a second recording.
	if handler.KeyEvent(graveKeyval, 0, ibus.ReleaseMask) {
		t.Error("the trailing space release was consumed")
	}
	if calls := transcriber.calls(); len(calls) != 1 {
		t.Errorf("%d decodes were requested, want one", len(calls))
	}
}

// A forgotten key cannot grow a recording without bound, and the audio up to the limit is
// still decoded.
func TestPushToTalkCapsAHeldRecording(t *testing.T) {
	handler, source, transcriber := newTestPushToTalk(t)
	handler.Config.MaxUtteranceSeconds = 0.5
	control := &recordingControl{}
	handler.Enable(control)

	handler.KeyEvent(graveKeyval, 0, ibus.ModControl)
	waitFor(t, "the capture to start", source.started)
	source.publish(tone(0.8, 6000))

	waitFor(t, "the capped recording to be decoded", func() bool { return len(transcriber.calls()) == 1 })
	if seconds := audio.DurationSeconds(transcriber.calls()[0]); seconds > 0.65 {
		t.Errorf("the decode was asked to read %.2fs, want at most max_utterance_seconds", seconds)
	}

	// The release, and the repeats before it, must not produce a second decode.
	handler.KeyEvent(graveKeyval, 0, ibus.ModControl)
	handler.KeyEvent(graveKeyval, 0, ibus.ModControl|ibus.ReleaseMask)
	time.Sleep(100 * time.Millisecond)
	if calls := transcriber.calls(); len(calls) != 1 {
		t.Errorf("%d decodes were requested, want one per hold", len(calls))
	}
	if got := control.delivered(); len(got) != 1 {
		t.Errorf("the application received %v, want one sentence", got)
	}
}

// Leaving the engine while the key is down drops the recording: the caret it was dictated
// for is being torn down.
func TestPushToTalkDiscardsTheRecordingWhenDeselected(t *testing.T) {
	handler, source, transcriber := newTestPushToTalk(t)
	control := &recordingControl{}
	handler.Enable(control)

	handler.KeyEvent(graveKeyval, 0, ibus.ModControl)
	waitFor(t, "the capture to start", source.started)
	source.publish(tone(0.5, 6000))
	handler.Disable()

	time.Sleep(150 * time.Millisecond)
	if calls := transcriber.calls(); len(calls) != 0 {
		t.Errorf("%d decodes were requested after the engine was deselected", len(calls))
	}
	if got := control.delivered(); len(got) != 0 {
		t.Errorf("the application received %v after the engine was deselected", got)
	}
}

// A second window does not restart anything, and a context leaving does not stop the
// engine from dictating into the one that remains.
func TestPushToTalkDeliversToTheNewestInputContext(t *testing.T) {
	handler, source, transcriber := newTestPushToTalk(t)
	first := &recordingControl{}
	handler.Enable(first)
	second := &recordingControl{}
	handler.Enable(second)

	handler.KeyEvent(graveKeyval, 0, ibus.ModControl)
	waitFor(t, "the capture to start", source.started)
	source.publish(tone(0.5, 6000))
	handler.KeyEvent(graveKeyval, 0, ibus.ModControl|ibus.ReleaseMask)

	waitFor(t, "the sentence to be delivered", func() bool { return len(second.delivered()) == 1 })
	if got := first.delivered(); len(got) != 0 {
		t.Errorf("the context the user left received %v", got)
	}

	// One context going away is the user moving windows, not leaving dictation.
	handler.Disable()
	handler.KeyEvent(graveKeyval, 0, ibus.ModControl)
	waitFor(t, "the next recording", func() bool { return len(transcriber.calls()) == 1 && source.recordings() == 2 })
	handler.KeyEvent(graveKeyval, 0, ibus.ModControl|ibus.ReleaseMask)
	if got := len(second.delivered()) + len(first.delivered()); got == 0 {
		t.Error("nothing was delivered")
	}
}

// The engine's log has to say that text left it.
func TestPushToTalkLogsWhatItDelivers(t *testing.T) {
	handler, _, _ := newTestPushToTalk(t)
	handler.AudioFile = speechWAV(t)
	logs := &logCollector{}
	handler.Log = logs.logf
	handler.NewTranscriber = func() asr.Transcriber { return &offlineTranscriber{text: "记录下来的句子。"} }

	control := &recordingControl{}
	handler.Enable(control)
	waitFor(t, "a commit", func() bool { return len(control.delivered()) > 0 })

	logged := logs.text()
	for _, want := range []string{"ready: hold", "diagnostic", "recording", "decoded", "committed", "记录下来的句子"} {
		if !strings.Contains(logged, want) {
			t.Errorf("the log does not record %q:\n%s", want, logged)
		}
	}
	if got := control.delivered(); len(got) != 1 || !strings.Contains(got[0], "记录下来的句子") {
		t.Errorf("the application received %v, want the decoded sentence", got)
	}
}

// A trigger this program cannot honour is refused where the message can name the setting.
func TestPushToTalkRefusesATriggerItCannotUse(t *testing.T) {
	for name, spec := range map[string]string{
		"empty":       "",
		"unknown key": "<Control>nosuchkey",
		"no modifier": "<F9>",
	} {
		t.Run(name, func(t *testing.T) {
			cfg := config.Default()
			cfg.PushToTalk = spec
			if _, err := NewPushToTalk(cfg, nil); err == nil {
				t.Errorf("NewPushToTalk accepted push_to_talk %q", spec)
			}
		})
	}
	if _, err := NewPushToTalk(config.Default(), nil); err != nil {
		t.Errorf("NewPushToTalk rejected the default configuration: %v", err)
	}
}

// The sink must reach whichever context is current, including text committed from the
// decoder's own goroutine after a rebind.
func TestSwitchableSinkFollowsItsTarget(t *testing.T) {
	sink := &switchableSink{}
	if err := sink.Commit("ignored"); err != nil {
		t.Fatalf("Commit with no target: %v", err)
	}
	if err := sink.Preedit("ignored", 0, false); err != nil {
		t.Fatalf("Preedit with no target: %v", err)
	}

	first, second := &recordingControl{}, &recordingControl{}
	sink.use(first)
	if err := sink.Commit("一"); err != nil {
		t.Fatal(err)
	}
	sink.use(second)
	if err := sink.Commit("二"); err != nil {
		t.Fatal(err)
	}
	if got := first.delivered(); len(got) != 1 || got[0] != "一" {
		t.Errorf("first context received %v, want only its own text", got)
	}
	if got := second.delivered(); len(got) != 1 || got[0] != "二" {
		t.Errorf("second context received %v, want only its own text", got)
	}
}

// A long sentence is shortened in the log, and never split mid-character.
func TestFirstRunesShortensWithoutSplittingCharacters(t *testing.T) {
	if got := firstRunes("你好世界", 10); got != "你好世界" {
		t.Errorf("firstRunes kept %q, want it unchanged", got)
	}
	got := firstRunes("一二三四五", 3)
	if got != "一二三…" {
		t.Errorf("firstRunes = %q, want 一二三…", got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("%q is not valid UTF-8", got)
	}
}
