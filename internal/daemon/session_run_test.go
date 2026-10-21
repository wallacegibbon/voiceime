package daemon

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wallacegibbon/voiceime/internal/audio"
	"github.com/wallacegibbon/voiceime/internal/config"
)

// These tests run a whole session against a stand-in for the resident server. They
// exist because the session is the only place in the project with three goroutines
// touching shared state at once - recognition events, the capture loop and the repair
// worker - and the unit tests around the transcript do not exercise that. Run with
// -race to be worth anything.

// tone is audio loud enough to cross the detector's threshold, at an RMS well above the
// configured 600.
func tone(seconds, amplitude float64) []byte {
	samples := make([]int16, int(seconds*audio.SampleRate))
	frequency := 2 * math.Pi * 220 / audio.SampleRate
	for i := range samples {
		samples[i] = int16(amplitude * math.Sin(frequency*float64(i)))
	}
	return audio.SamplesToBytes(samples)
}

// speechWAV writes silence, then a tone loud enough to be speech, then silence, so the
// endpoint detector opens and closes one utterance.
func speechWAV(t *testing.T) string {
	t.Helper()
	payload := append(append(silence(0.2), tone(0.6, 6000)...), silence(0.5)...)
	path := filepath.Join(t.TempDir(), "speech.wav")
	writeWAV(t, path, payload)
	return path
}

// sessionOptions wires a session to a fake server. The server is the only decode route
// there is now, which is why the harness is a server rather than a script: it answers
// the live endpoint, the repair endpoint and /v1/models on one address, as the real
// service does.
func sessionOptions(t *testing.T, server *fakeASRServer, refine bool) (Options, *recordingSink) {
	t.Helper()
	return sessionOptionsForWAV(t, server, refine, speechWAV(t))
}

func sessionOptionsForWAV(t *testing.T, server *fakeASRServer, refine bool, wav string) (Options, *recordingSink) {
	t.Helper()
	cfg := config.Default()
	cfg.RefineOnSegmentEnd = refine
	cfg.ASRServerURL = server.url

	sink := &recordingSink{}
	return Options{
		Config: cfg,
		Sink:   sink,
		NewCapture: func() (audio.Capture, error) {
			// Real time, deliberately: the fake server emits partials as audio
			// arrives, and the interesting interleavings (a partial arriving before
			// the utterance ends, a repair finishing after it) only happen when audio
			// arrives at the rate it would from a microphone.
			return audio.NewFileReplay(wav, true)
		},
	}, sink
}

func runSession(t *testing.T, options Options) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := options.Run(ctx)
	if err != nil && ctx.Err() == nil {
		t.Fatalf("session failed: %v", err)
	}
	return result
}

// The happy path: live partials shown as a composition, then the repaired sentence
// committed once.
func TestSessionCommitsTheRepairedText(t *testing.T) {
	server := newFakeASRServer(t)
	server.repairText = "修复后的句子。"
	options, sink := sessionOptions(t, server, true)
	result := runSession(t, options)

	if result.Partials == 0 {
		t.Error("no partials were recorded, so the recognition path never ran")
	}
	if server.repairCount() == 0 {
		t.Error("no repair was requested, so the offline pass never ran")
	}
	if len(sink.commits()) == 0 {
		t.Fatalf("nothing was delivered: %v", sink.events)
	}
	last := sink.commits()[len(sink.commits())-1]
	if !strings.Contains(last, "修复后的句子") {
		t.Errorf("committed %q, want the repaired text", last)
	}
	if sink.preeditClears() == 0 {
		t.Error("the composition was never cleared, so an application would show the text twice")
	}
}

// With repair switched off the stream text is what reaches the application, and the
// repair endpoint is never called.
func TestSessionCommitsStreamTextWhenRepairIsOff(t *testing.T) {
	server := newFakeASRServer(t)
	options, sink := sessionOptions(t, server, false)
	runSession(t, options)

	if len(sink.commits()) == 0 {
		t.Fatalf("nothing was delivered: %v", sink.events)
	}
	if got := strings.Join(sink.commits(), ""); !strings.Contains(got, "片段") {
		t.Errorf("committed %q, want the streaming text", got)
	}
	if server.repairCount() != 0 {
		t.Errorf("the repair endpoint was called %d time(s) with repair off", server.repairCount())
	}
}

// A repair that fails must still release the utterance: otherwise the text stays
// provisional forever and the user sees it never arrive.
func TestSessionReleasesTextWhenRepairFails(t *testing.T) {
	server := newFakeASRServer(t)
	server.repairStatus = 503
	options, sink := sessionOptions(t, server, true)
	runSession(t, options)

	if len(sink.commits()) == 0 {
		t.Fatalf("a failed repair left the text undelivered: %v", sink.events)
	}
	if got := strings.Join(sink.commits(), ""); !strings.Contains(got, "片段") {
		t.Errorf("committed %q, want the stream text as the fallback", got)
	}
}

// A cancelled session must finish cleanly - and the sentence the cancellation itself
// closed must still be decoded offline.
//
// Cancelling is how dictation ends: the hotkey switches the engine off, Disable
// cancels the session, and the cancellation flushes the utterance in progress and
// queues it for repair *after* the context is already cancelled. The decode used to
// inherit that cancellation, so it never left the process: measured on this test, zero
// repair requests and the window-boundary-damaged draft delivered as if it were final.
// The earlier version of this test used a 1.5s timeout on a 1.3s clip, so it never
// cancelled anything and could not see the bug.
func TestSessionStopsCleanlyWhenCancelled(t *testing.T) {
	server := newFakeASRServer(t)
	server.repairText = "释放后的句子。"
	options, sink := sessionOptions(t, server, true)
	// After the speech ends (0.8s) and before the 800ms hangover would close the
	// utterance on its own, so the boundary comes from the cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()

	if _, err := options.Run(ctx); err != nil && ctx.Err() == nil {
		t.Fatalf("session failed: %v", err)
	}
	if len(sink.commits()) == 0 {
		t.Fatalf("a cancelled session delivered nothing: %v", sink.events)
	}
	if got := server.repairCount(); got == 0 {
		t.Error("no repair was requested: the utterance the stop closed was delivered as the draft")
	}
	if got := strings.Join(sink.commits(), ""); !strings.Contains(got, "释放后的句子") {
		t.Errorf("committed %q, want the text the offline decode produced", got)
	}
}

// One session owns one capture: the options take a factory because a replay is
// consumed by a run and a microphone should be reopened per activation. A second run
// must therefore work - and it must work against the same server, because the server is
// a service rather than something the session starts.
func TestSessionCanRunTwiceWithAFactory(t *testing.T) {
	server := newFakeASRServer(t)
	options, sink := sessionOptions(t, server, true)

	runSession(t, options)
	first := len(sink.commits())
	if first == 0 {
		t.Fatalf("first run delivered nothing: %v", sink.events)
	}
	runSession(t, options)
	if len(sink.commits()) <= first {
		t.Errorf("second run delivered nothing new: %v", sink.events)
	}
	if got := server.liveCount(); got != 2 {
		t.Errorf("opened %d streaming sessions over two dictations, want 2", got)
	}
}

// writeWAV lays down a 16 kHz mono file the replay can read.
func writeWAV(t *testing.T, path string, pcm []byte) {
	t.Helper()
	if err := os.WriteFile(path, audio.EncodeWAV(pcm, audio.SampleRate, 1), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// silence builds a block of digital silence for the synthetic sessions.
func silence(seconds float64) []byte {
	return make([]byte, int(seconds*audio.SampleRate)*audio.BytesPerSample)
}

// A session whose audio is only silence commits nothing, and never shows the
// recogniser's guess at all.
//
// Streaming ASR produces plausible words for almost any input - twenty seconds of an
// empty room once committed 被。。。。。。嗯。 into the focused application, and a quiet pause
// used to leave one "。" per window sitting under the caret until the next sentence's
// commit flushed it into the document. Text for audio the detector never called speech is
// dropped when it arrives (see acceptPartial), so the strongest form of the rule is the
// one asserted here: it never reaches the composition, and nothing is committed.
func TestSessionWithholdsTextWhenNoSpeechIsDetected(t *testing.T) {
	server := newFakeASRServer(t)
	dir := t.TempDir()
	wav := filepath.Join(dir, "silence.wav")
	writeWAV(t, wav, silence(1.5))

	var mu sync.Mutex
	var skipped []string
	options, sink := sessionOptionsForWAV(t, server, true, wav)
	options.Observer = Observer{Skipped: func(reason string) {
		mu.Lock()
		defer mu.Unlock()
		skipped = append(skipped, reason)
	}}
	// The fake server emits partials regardless of what the audio contains, which is
	// exactly the hallucination the rule is about.
	result := runSession(t, options)

	if got := sink.commits(); len(got) != 0 {
		t.Fatalf("a session with no detected speech committed %v", got)
	}
	if result.Partials == 0 {
		t.Fatal("the fake recogniser produced no partials, so this proves nothing")
	}
	if result.FinalText != "" {
		t.Errorf("transcript = %q: the guess reached the composition", result.FinalText)
	}
	if got := sink.preedits(); len(got) != 0 {
		t.Errorf("the composition showed %v: a caret would have displayed the guess", got)
	}
	mu.Lock()
	reasons := strings.Join(skipped, " | ")
	mu.Unlock()
	if !strings.Contains(reasons, "never called speech") {
		t.Fatalf("skipped reasons = %q, want the drop reported", reasons)
	}
	if !strings.Contains(reasons, "no speech detected") {
		t.Fatalf("skipped reasons = %q, want the end-of-session report too", reasons)
	}
}

// A session whose audio begins with speech - no quiet frame to measure first - must
// still find the utterance and commit it, including when the recording simply ends and
// the boundary comes from Flush rather than from silence.
//
// This is the end-to-end form of the endpointer bug. The detector compared every frame
// against twice the configured threshold until it had measured the room, so a user who
// started speaking the instant dictation was activated was never heard: the text
// appeared as preedit, no segment was ever accepted, and stopping the session discarded
// it. The tone here has an RMS of ~850: above the configured 600, below the 1200 that
// was in force.
func TestSessionCommitsWhenSpeechStartsImmediately(t *testing.T) {
	server := newFakeASRServer(t)
	spoken := make([]int16, int(0.8*audio.SampleRate))
	frequency := 2 * math.Pi * 220 / audio.SampleRate
	for i := range spoken {
		spoken[i] = int16(1200 * math.Sin(frequency*float64(i)))
	}
	// Less trailing silence than the 800ms hangover, so the utterance is closed by
	// Flush: the session ends with the user still in it, which is the reported case.
	payload := append(audio.SamplesToBytes(spoken), silence(0.6)...)
	wav := filepath.Join(t.TempDir(), "immediate.wav")
	writeWAV(t, wav, payload)

	options, sink := sessionOptionsForWAV(t, server, true, wav)
	runSession(t, options)
	if len(sink.commits()) == 0 {
		t.Fatalf("a session that began with speech committed nothing: %v", sink.events)
	}
}

// A streaming session the server fails is a failure, and the draft it stranded is
// not delivered as though the user had finished the sentence.
//
// Two failures are being pinned at once, and both were invisible before:
//   - the reason arrived nested under "error", so the parser found no message, and
//     the session ended as if the speaker had simply stopped;
//   - Result.ProcessErr had no reader, so even the placeholder never reached a
//     caller, and `voiceime stream` exited 0 with a truncated transcript.
func TestSessionReportsAStreamFailureAndWithholdsTheDraft(t *testing.T) {
	server := newFakeASRServer(t)
	server.liveFail = "model died: cuda OOM"
	// Speech the detector notices - so the streaming text is accepted into the composition
	// - but shorter than endpoint_min_speech_ms, so no utterance closes and nothing reaches
	// the offline decode. That is the draft D24 says a failed session must withhold.
	wav := filepath.Join(t.TempDir(), "short.wav")
	writeWAV(t, wav, append(tone(0.2, 6000), silence(1.0)...))

	var mu sync.Mutex
	var skipped []string
	options, sink := sessionOptionsForWAV(t, server, true, wav)
	options.Observer = Observer{Skipped: func(reason string) {
		mu.Lock()
		defer mu.Unlock()
		skipped = append(skipped, reason)
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	result, err := options.Run(ctx)
	if err == nil {
		t.Fatal("a session whose stream failed reported success")
	}
	if !strings.Contains(err.Error(), "cuda OOM") {
		t.Errorf("failure %q does not carry the server's reason", err)
	}
	if result.Partials == 0 {
		t.Fatal("the fake produced no partials, so there was no draft to withhold")
	}
	if got := sink.commits(); len(got) != 0 {
		t.Fatalf("a failed session committed the unrepaired draft: %v", got)
	}
	mu.Lock()
	reasons := strings.Join(skipped, " | ")
	mu.Unlock()
	want := fmt.Sprintf("withheld %d characters", len([]rune(result.FinalText)))
	if !strings.Contains(reasons, want) {
		t.Errorf("skipped reasons = %q, want the size of what was dropped (%q)", reasons, want)
	}
}

// A session that showed a composition and then ends without delivering it must
// remove it: that session is the only thing that could have finished the text, and
// a composition left at the caret outlives it.
//
// The audio is the shape that produces the case - a burst shorter than
// endpoint_min_speech_ms, which opens a speech region (so a window's text is
// accepted and shown as preedit) but never earns an utterance. Five sessions in the
// reference machine's log ended this way (`no speech detected; withheld N
// characters`, one of them 35 characters) and each left its text on screen with
// nothing left to clear it.
func TestSessionClearsACompositionItDoesNotDeliver(t *testing.T) {
	server := newFakeASRServer(t)
	wav := filepath.Join(t.TempDir(), "short.wav")
	writeWAV(t, wav, append(append(silence(0.2), tone(0.2, 6000)...), silence(4)...))

	options, sink := sessionOptionsForWAV(t, server, false, wav)
	runSession(t, options)

	if len(sink.preedits()) == 0 {
		t.Fatal("no composition was shown, so this audio does not exercise the case")
	}
	if got := sink.commits(); len(got) != 0 {
		t.Fatalf("committed %v, want nothing: no utterance was ever accepted", got)
	}
	if sink.preeditClears() == 0 {
		t.Fatalf("the composition was left on screen: %v", sink.events)
	}
}

// fakeTranscriber stands in for an injected decode: it is handed the utterance's PCM
// and answers with text of the test's choosing.
type fakeTranscriber struct {
	text  string
	err   error
	calls int
	audio []byte
}

func (f *fakeTranscriber) Transcribe(_ context.Context, pcm []byte) (string, error) {
	f.calls++
	f.audio = append([]byte(nil), pcm...)
	return f.text, f.err
}

// The offline decode belongs to whatever Transcriber the session is handed. It is how
// a test can pin the repair contract without a server, and the seam a future reader
// would use.
func TestSessionRepairsWithTheInjectedTranscriber(t *testing.T) {
	server := newFakeASRServer(t)
	options, sink := sessionOptions(t, server, true)
	injected := &fakeTranscriber{text: "注入的解码结果。"}
	options.Transcriber = injected
	runSession(t, options)

	got := strings.Join(sink.commits(), "")
	if !strings.Contains(got, "注入的解码结果") {
		t.Fatalf("committed %q, want the injected transcriber's text", got)
	}
	if injected.calls == 0 {
		t.Fatal("the injected transcriber was never called, so the repair pass did not run")
	}
	if len(injected.audio) == 0 {
		t.Error("the transcriber was handed no audio: the utterance's PCM must reach it")
	}
	if server.repairCount() != 0 {
		t.Errorf("the server was asked to repair %d time(s) despite an injected transcriber", server.repairCount())
	}
}

// A transcriber that fails must release the utterance exactly as a failed server
// repair does, or the text stays provisional and the user sees it never arrive.
func TestSessionReleasesTextWhenTheInjectedTranscriberFails(t *testing.T) {
	server := newFakeASRServer(t)
	options, sink := sessionOptions(t, server, true)
	options.Transcriber = &fakeTranscriber{err: fmt.Errorf("server repair: 503 Service Unavailable")}
	errors := make(chan error, 4)
	options.Observer = Observer{Error: func(err error) { errors <- err }}
	runSession(t, options)

	if len(sink.commits()) == 0 {
		t.Fatalf("a failed server repair left the text undelivered: %v", sink.events)
	}
	if got := strings.Join(sink.commits(), ""); !strings.Contains(got, "片段") {
		t.Errorf("committed %q, want the stream text as the fallback", got)
	}
	select {
	case err := <-errors:
		if !strings.Contains(err.Error(), "503") {
			t.Errorf("reported %v, want the server's reason", err)
		}
	default:
		t.Error("the failure was not reported to the user")
	}
}

// A session with no server configured cannot run at all now: there is no process to
// fall back to. It has to say so instead of ending silently.
func TestSessionWithoutAServerFailsWithAReason(t *testing.T) {
	cfg := config.Default()
	cfg.ASRServerURL = ""
	options := Options{
		Config: cfg,
		Sink:   &recordingSink{},
		NewCapture: func() (audio.Capture, error) {
			return audio.NewFileReplay(speechWAV(t), true)
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := options.Run(ctx); err == nil {
		t.Fatal("a session with no server ran anyway")
	} else if !strings.Contains(err.Error(), "server") {
		t.Errorf("error %q does not say what is missing", err)
	}
}

// failingCapture is a source that cannot be opened: a busy device, or a sound server
// this process may not reach.
type failingCapture struct{}

func (failingCapture) Start() error               { return errors.New("capture device unavailable") }
func (failingCapture) Chunks() <-chan audio.Chunk { return nil }
func (failingCapture) Stop()                      {}
func (failingCapture) Err() error                 { return nil }

// A failed activation must not leave the repair workers behind.
//
// The workers start before the capture source is opened, so a device that will not
// open used to leak a queue and two goroutines per activation - in a process that lives
// as long as the login session. Measured before the cleanup: 100 goroutines after 50
// failed activations.
func TestFailedActivationLeavesNoRepairWorkers(t *testing.T) {
	server := newFakeASRServer(t)
	options, _ := sessionOptions(t, server, true)
	options.NewCapture = func() (audio.Capture, error) { return failingCapture{}, nil }

	// One run first, so the baseline includes whatever the harness itself keeps.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	if _, err := options.Run(ctx); err == nil {
		t.Fatal("a session whose capture could not start reported success")
	}
	cancel()
	baseline := runtime.NumGoroutine()
	if got := runtime.NumGoroutine(); got > baseline {
		t.Fatalf("baseline = %d goroutines, want %d", got, baseline)
	}

	for i := 0; i < 20; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if _, err := options.Run(ctx); err == nil {
			t.Fatal("a session whose capture could not start reported success")
		}
		cancel()
	}

	// Shutdown is asynchronous by design (the workers close their own channels), so
	// the count is allowed to settle rather than read once.
	deadline := time.Now().Add(3 * time.Second)
	for {
		time.Sleep(20 * time.Millisecond)
		if got := runtime.NumGoroutine(); got <= baseline {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("goroutines = %d after 20 failed activations, want at most the baseline %d: the repair workers outlived the session",
				runtime.NumGoroutine(), baseline)
		}
	}
}

// A stop we asked for must not be reported as a write failure.
//
// Cancelling closes the streaming socket under the capture loop - it is what releases a
// Write waiting on a slow server - so the next block fails to write. That is the stop
// working: it used to reach the observer, which put "write audio to the streaming
// server: use of closed network connection" in the engine's trace and on
// `voiceime stream`'s stderr for every Ctrl-C (reproduced against the live server).
func TestCancelledSessionIsNotReportedAsAWriteFailure(t *testing.T) {
	server := newFakeASRServer(t)
	options, sink := sessionOptions(t, server, true)

	var mu sync.Mutex
	var reported []string
	partials := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	options.Observer = Observer{
		// Stop on the second partial: the utterance has been speaking long enough to be
		// accepted, and audio is still arriving, so the loop has another block to write.
		Partial: func(string, float64) {
			mu.Lock()
			defer mu.Unlock()
			partials++
			if partials == 2 {
				cancel()
			}
		},
		Error: func(err error) {
			mu.Lock()
			defer mu.Unlock()
			reported = append(reported, err.Error())
		},
	}

	if _, err := options.Run(ctx); err != nil && ctx.Err() == nil {
		t.Fatalf("session failed: %v", err)
	}
	mu.Lock()
	messages := strings.Join(reported, " | ")
	mu.Unlock()
	if strings.Contains(messages, "write audio") {
		t.Errorf("a requested stop was reported as a failure: %s", messages)
	}
	if len(sink.commits()) == 0 {
		t.Errorf("a cancelled session delivered nothing: %v", sink.events)
	}
}

// A session stopped on request, which heard nothing, still says so.
//
// The stop closes the streaming socket, and the reader's resulting error used to be
// recorded as a session failure. Since D24 a failed session takes the "withhold the
// draft" branch - so the end-of-session rule never ran, and `voiceime stream --seconds 3`
// in a quiet room printed a summary with no line explaining why nothing was committed
// (measured on the installed build before this test existed).
func TestStoppedSessionWithNoSpeechExplainsItself(t *testing.T) {
	server := newFakeASRServer(t)
	wav := filepath.Join(t.TempDir(), "silence.wav")
	writeWAV(t, wav, silence(1.2))

	var mu sync.Mutex
	var skipped []string
	options, sink := sessionOptionsForWAV(t, server, true, wav)
	options.Observer = Observer{Skipped: func(reason string) {
		mu.Lock()
		defer mu.Unlock()
		skipped = append(skipped, reason)
	}}

	// Stop while the stream is still open, which is what a hotkey release does.
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if _, err := options.Run(ctx); err != nil && ctx.Err() == nil {
		t.Fatalf("session failed: %v", err)
	}

	if len(sink.commits()) != 0 {
		t.Fatalf("a silent session committed %v", sink.commits())
	}
	mu.Lock()
	reasons := strings.Join(skipped, " | ")
	mu.Unlock()
	if !strings.Contains(reasons, "no speech detected") {
		t.Errorf("skipped reasons = %q, want the session to say it heard no speech", reasons)
	}
}

// Text decoded from a pause never reaches the composition, and never rides into the next
// sentence's commit.
//
// This is the reported "the last sentence will not stop by itself": the endpointer had
// correctly closed the utterance, but the live model kept answering with one "。" per
// window of room silence, the session kept it as provisional text, and the composition sat
// under the caret until the *next* sentence committed it along with itself - measured in
// the engine log at 01:37: five "。" windows, then a 40-character commit that began with
// them.
func TestSessionDropsStreamingTextFromSilenceBetweenSentences(t *testing.T) {
	server := newFakeASRServer(t)
	server.repairText = "修复后的句子" // no punctuation: the assertion below looks for "。。"
	server.silencePartial = "。。"

	payload := append(tone(0.6, 6000), silence(3.0)...)
	payload = append(payload, tone(0.6, 6000)...)
	payload = append(payload, silence(0.5)...)
	wav := filepath.Join(t.TempDir(), "two-sentences.wav")
	writeWAV(t, wav, payload)

	options, sink := sessionOptionsForWAV(t, server, true, wav)
	result := runSession(t, options)

	commits := sink.commits()
	if len(commits) < 2 {
		t.Fatalf("commits = %v, want one per sentence: the pause swallowed the boundary", commits)
	}
	for _, text := range commits {
		if strings.Contains(text, "。。") {
			t.Errorf("committed %q: text decoded from the pause reached the application", text)
		}
	}
	for _, text := range sink.preedits() {
		if strings.Contains(text, "。。") {
			t.Errorf("the composition showed %q: it would sit under the caret until the next sentence", text)
		}
	}
	if result.FinalText == "" {
		t.Error("nothing was transcribed at all, so this proves nothing")
	}
}

// The repair model has no ITN and may answer in Traditional characters. What is
// delivered has to be the form the user configured, not the model's form: this is
// the end-to-end version of internal/textnorm's unit tests.
func TestSessionNormalizesTheRepairedText(t *testing.T) {
	server := newFakeASRServer(t)
	server.repairText = "臺灣，二零二五年十月二十一号，一千二百三十四個蘋果。"
	options, sink := sessionOptions(t, server, true)
	runSession(t, options)

	last := sink.commits()[len(sink.commits())-1]
	want := "台湾，2025年10月21号，1234个苹果。"
	if last != want {
		t.Errorf("committed %q, want %q", last, want)
	}
}

// With repair off the streaming model's text is committed directly, so the
// transcript's own normalizer is the only chance to clean it.
func TestSessionNormalizesStreamingTextWhenRepairIsOff(t *testing.T) {
	server := newFakeASRServer(t)
	server.partial = "臺灣，二零二五年。"
	options, sink := sessionOptions(t, server, false)
	runSession(t, options)

	got := strings.Join(sink.commits(), "")
	if !strings.Contains(got, "台湾，2025年。") {
		t.Errorf("committed %q, want the normalized streaming text", got)
	}
	if strings.ContainsAny(got, "臺二零") {
		t.Errorf("committed %q still carries Traditional characters or Chinese numerals", got)
	}
}

// A repair is normalized exactly once. OpenCC's t2s data is not idempotent
// (乾圖 -> 乾图 -> 干图), so normalizing the decode before Place and again when
// the transcript is read would deliver a different string than the repair event
// reported - and a worse one.
func TestSessionAppliesTheNormalizerOnceToARepair(t *testing.T) {
	server := newFakeASRServer(t)
	server.repairText = "乾圖和乾斷。"
	options, sink := sessionOptions(t, server, true)
	repaired := make(chan string, 1)
	options.Observer = Observer{Repair: func(text string, _ time.Duration, _ float64) {
		select {
		case repaired <- text:
		default:
		}
	}}
	runSession(t, options)

	commits := sink.commits()
	if len(commits) == 0 {
		t.Fatal("nothing was committed")
	}
	committed := commits[len(commits)-1]
	if want := "乾图和乾断。"; committed != want {
		t.Errorf("committed %q, want %q (the single pass over the decode)", committed, want)
	}
	select {
	case event := <-repaired:
		if event != committed {
			t.Errorf("repair event = %q, committed %q: they must be the same single pass", event, committed)
		}
	default:
		t.Error("the repair event was never reported")
	}
}
