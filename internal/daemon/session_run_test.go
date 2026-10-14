package daemon

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/wallacegibbon/voiceime/internal/audio"
	"github.com/wallacegibbon/voiceime/internal/config"
)

// These tests run a whole session against a stand-in for audiocpp_cli. They exist
// because the session is the only place in the project with three goroutines
// touching shared state at once - recognition events, the capture loop and the
// repair worker - and the unit tests around the transcript do not exercise that.
// Run with -race to be worth anything.

// The two invocations are told apart by their audio argument: streaming reads
// "-" from stdin, the repair pass is given a file. Getting this wrong made every
// call take the offline branch, which is how the first version of this test failed.
const fakeEngineTemplate = `#!/bin/sh
case "$*" in
  *"--audio -"*)
    # Streaming: report readiness, then emit partials while audio arrives.
    echo "audio_input=stdin format=s16le rate=16000 channels=1"
    ( i=0; while [ $i -lt 200 ]; do sleep 0.05; echo "partial_text=片段"; i=$((i+1)); done ) &
    bg=$!
    cat > /dev/null
    kill $bg 2>/dev/null
    echo "text_output=片段片段"
    ;;
  *)
    # Offline (repair): no stdin, just the report.
%s
    ;;
esac
`

func writeFakeEngine(t *testing.T, dir, repairBehaviour string) string {
	t.Helper()
	path := filepath.Join(dir, "fake-audiocpp")
	script := strings.Replace(fakeEngineTemplate, "%s", repairBehaviour, 1)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake engine: %v", err)
	}
	return path
}

// speechWAV writes silence, then a tone loud enough to be speech, then silence,
// so the endpoint detector opens and closes one utterance.
func speechWAV(t *testing.T, dir string) string {
	t.Helper()
	room := audio.Silence(0.2)
	spoken := make([]int16, int(0.6*audio.SampleRate))
	frequency := 2 * math.Pi * 220 / audio.SampleRate
	for i := range spoken {
		spoken[i] = int16(6000 * math.Sin(frequency*float64(i)))
	}
	payload := append(append(room, audio.SamplesToBytes(spoken)...), audio.Silence(0.5)...)
	path := filepath.Join(dir, "speech.wav")
	if err := audio.WriteWAV(path, payload, audio.SampleRate, 1); err != nil {
		t.Fatalf("write wav: %v", err)
	}
	return path
}

func sessionOptions(t *testing.T, dir, repairBehaviour string, refine bool) (Options, *recordingSink) {
	t.Helper()
	binary := writeFakeEngine(t, dir, repairBehaviour)
	wav := speechWAV(t, dir)

	cfg := config.Default()
	// The CLI runs with this directory as its working directory; it has to exist.
	cfg.AudioCPPDir = dir
	cfg.Threads = 1
	cfg.RefineOnSegmentEnd = refine

	sink := &recordingSink{}
	return Options{
		Config: cfg,
		Binary: binary,
		Model:  "not-a-real-model",
		Sink:   sink,
		NewCapture: func() (audio.Capture, error) {
			// Real time, deliberately: the fake engine emits partials on a timer,
			// and the interesting interleavings (a partial arriving before the
			// utterance ends, a repair finishing after it) only happen when audio
			// arrives at the rate it would from a microphone. A fast replay would
			// end the utterance before the first partial existed.
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
	options, sink := sessionOptions(t, t.TempDir(), `echo "task=asr"; echo "text_output=修复后的句子。"`, true)
	result := runSession(t, options)

	if result.Partials == 0 {
		t.Error("no partials were recorded, so the recognition path never ran")
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

// With repair switched off the stream text is what reaches the application.
func TestSessionCommitsStreamTextWhenRepairIsOff(t *testing.T) {
	options, sink := sessionOptions(t, t.TempDir(), `echo "should not run"`, false)
	runSession(t, options)

	if len(sink.commits()) == 0 {
		t.Fatalf("nothing was delivered: %v", sink.events)
	}
	if got := strings.Join(sink.commits(), ""); !strings.Contains(got, "片段") {
		t.Errorf("committed %q, want the streaming text", got)
	}
}

// A repair that fails must still release the utterance: otherwise the text stays
// provisional forever and the user sees it never arrive.
func TestSessionReleasesTextWhenRepairFails(t *testing.T) {
	options, sink := sessionOptions(t, t.TempDir(), `echo "repair exploded" >&2; exit 1`, true)
	runSession(t, options)

	if len(sink.commits()) == 0 {
		t.Fatalf("a failed repair left the text undelivered: %v", sink.events)
	}
	if got := strings.Join(sink.commits(), ""); !strings.Contains(got, "片段") {
		t.Errorf("committed %q, want the stream text as the fallback", got)
	}
}

// A session that is cancelled must still finish cleanly and deliver what it had.
func TestSessionStopsCleanlyWhenCancelled(t *testing.T) {
	options, sink := sessionOptions(t, t.TempDir(), `echo "task=asr"; echo "text_output=。"; echo "done"`, true)
	// Long enough for the utterance to be recognised, short enough to land in the
	// middle of the trailing silence.
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	if _, err := options.Run(ctx); err != nil && ctx.Err() == nil {
		t.Fatalf("session failed: %v", err)
	}
	if len(sink.commits()) == 0 {
		t.Errorf("a cancelled session delivered nothing: %v", sink.events)
	}
}

// One session owns one capture: the options take a factory because a replay is
// consumed by a run and a microphone should be reopened per activation. A second
// run must therefore work.
func TestSessionCanRunTwiceWithAFactory(t *testing.T) {
	dir := t.TempDir()
	options, sink := sessionOptions(t, dir, `echo "task=asr"; echo "text_output=。"`, true)

	runSession(t, options)
	first := len(sink.commits())
	if first == 0 {
		t.Fatalf("first run delivered nothing: %v", sink.events)
	}
	runSession(t, options)
	if len(sink.commits()) <= first {
		t.Errorf("second run delivered nothing new: %v", sink.events)
	}
}
