package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/wallacegibbon/voiceime/internal/asr"
	"github.com/wallacegibbon/voiceime/internal/audio"
	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/ibus"
	"github.com/wallacegibbon/voiceime/internal/textnorm"
)

type PushToTalk struct {
	// Config is read per hold, so a changed setting applies to the next dictation
	// without restarting the engine. The exception is the trigger itself, which is
	// parsed once (see NewPushToTalk).
	Config *config.Config
	// Log is where this handler accounts for itself; nil discards.
	Log func(format string, args ...any)
	// AudioFile, when set, dictates that file on activation instead of waiting for
	// the key. It is the diagnostic that proves recording, decoding and delivery
	// without a microphone or a keyboard.
	AudioFile string
	// NewTranscriber overrides the offline decode; nil uses the resident server.
	// Tests use it to answer without a model.
	NewTranscriber func() asr.Transcriber
	// NewCapture overrides the source; nil captures from the configured input.
	// Tests use it to publish audio on demand.
	NewCapture func() (audio.Capture, error)

	// key is the parsed trigger, and spec is what the user wrote for it: the log
	// quotes the setting, not a re-serialisation of it.
	key        ibus.Key
	spec       string
	normalizer *textnorm.Normalizer

	// jobs is the decode queue: bounded, drained in order by one worker, so two quick
	// holds cannot deliver their text out of order (which would be invisible until the
	// application had two sentences swapped).
	jobs chan utterance
	once sync.Once

	mu sync.Mutex
	// sink is where text is delivered. It outlives a hold so the target can follow the
	// input context the user is in.
	sink     *switchableSink
	contexts int
	// holding is the recording in flight, or nil. It is what a press starts and a
	// release ends; a second press while it exists is auto-repeat.
	holding *hold
	// consuming says the trigger key's press was consumed, so its release has to be
	// consumed too - the application must not receive half a keystroke.
	consuming bool
}

// hold is one recording in flight.
type hold struct {
	capture audio.Capture
	// discard is set when the engine is deselected while the key is still down. The
	// text would have no caret to go to: the context it would be delivered into is the
	// one being torn down.
	discard atomic.Bool
}

// detach clears the hold this goroutine owns, unless something already replaced it.
func (r *hold) detach(h *PushToTalk) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.holding == r {
		h.holding = nil
	}
}

func NewPushToTalk(cfg *config.Config, logf func(format string, args ...any)) (*PushToTalk, error) {
	if cfg == nil {
		return nil, fmt.Errorf("push to talk needs a configuration")
	}
	spec := strings.TrimSpace(cfg.PushToTalk)
	if spec == "" {
		return nil, fmt.Errorf("no push_to_talk combination is configured")
	}
	key, err := ibus.ParseShortcut(spec)
	if err != nil {
		return nil, fmt.Errorf("push_to_talk %q: %w", spec, err)
	}
	if key.State&ibus.ModifierMask == 0 {
		return nil, fmt.Errorf("push_to_talk %q has no modifier: hold a combination such as <Control>apostrophe", spec)
	}
	return &PushToTalk{
		Config:     cfg,
		Log:        logf,
		key:        key,
		spec:       spec,
		normalizer: textnorm.New(cfg.Language, textnorm.Options{Script: cfg.TextScript}),
		// One slot per delivery slice of a long take. The slices of one hold share the
		// recording's buffer, so a deep queue costs headers, not audio.
		jobs: make(chan utterance, 64),
	}, nil
}

// TriggerSpec is the combination as the user wrote it, for a caller that reports what
// the engine is waiting for.
func (h *PushToTalk) TriggerSpec() string { return h.spec }

func (h *PushToTalk) logf(format string, args ...any) {
	if h.Log != nil {
		h.Log(format, args...)
	}
}

// Enable records where text would go. It deliberately starts nothing: the engine being
// selected only says that the user's typing is routed here.
func (h *PushToTalk) Enable(control ibus.Control) {
	h.mu.Lock()
	if h.sink == nil {
		h.sink = &switchableSink{}
	}
	h.sink.use(control)
	h.contexts++
	first := h.contexts == 1
	file := h.AudioFile
	h.mu.Unlock()

	if !first {
		return
	}
	h.logf("ready: hold %s to dictate, release to transcribe", h.spec)
	if file != "" {
		h.logf("diagnostic: dictating %s, with no key and no microphone", file)
		h.play(file)
	}
}

// Disable stops watching, and drops a recording whose text would have nowhere to go.
func (h *PushToTalk) Disable() {
	h.mu.Lock()
	if h.contexts > 0 {
		h.contexts--
	}
	last := h.contexts == 0
	recording := h.holding
	if last {
		h.holding, h.consuming = nil, false
	}
	h.mu.Unlock()

	if !last || recording == nil {
		return
	}
	recording.discard.Store(true)
	h.logf("discarded the recording in progress: the engine was deselected before the key was released")
	go recording.capture.Stop()
}

func (h *PushToTalk) KeyEvent(keyval, keycode, state uint32) bool {
	if h.AudioFile != "" {
		// The diagnostic has no keyboard; its audio starts with the engine.
		return false
	}
	if state&ibus.ReleaseMask != 0 {
		return h.keyUp(keyval, h.key)
	}
	return h.keyDown(keyval, state&ibus.ModifierMask, h.key)
}

func (h *PushToTalk) keyDown(keyval, mods uint32, trigger ibus.Key) bool {
	if keyval != trigger.Keyval || !trigger.Held(mods) {
		return false
	}
	h.mu.Lock()
	if h.consuming {
		// Auto-repeat: the daemon reports every repeat of a held key as another press
		// (roughly 200 per second of hold), and one recording per tick is nonsense.
		h.mu.Unlock()
		return true
	}
	h.consuming = true
	h.mu.Unlock()

	h.record()
	return true
}

func (h *PushToTalk) keyUp(keyval uint32, trigger ibus.Key) bool {
	h.mu.Lock()
	consuming := h.consuming
	h.mu.Unlock()

	switch {
	case consuming && keyval == trigger.Keyval:
		h.stop()
		return true
	case consuming && trigger.State&ibus.ModifierMaskOf(keyval) != 0:
		h.stop()
	}
	return false
}

// record opens the configured source and starts collecting it.
func (h *PushToTalk) record() {
	capture, err := h.newCapture()
	if err != nil {
		h.logf("cannot record: %v", err)
		return
	}
	h.begin(capture, true)
}

// play is record's file counterpart, for the diagnostic.
func (h *PushToTalk) play(path string) {
	capture, err := audio.NewFileReplay(path, true)
	if err != nil {
		h.logf("cannot dictate %s: %v", path, err)
		return
	}
	h.begin(capture, false)
}

// begin starts a capture and the goroutine that collects it.
func (h *PushToTalk) begin(capture audio.Capture, keyed bool) {
	if err := capture.Start(); err != nil {
		h.logf("cannot record: %v", err)
		return
	}
	recording := &hold{capture: capture}
	h.mu.Lock()
	switch {
	case h.contexts == 0:
		h.mu.Unlock()
		h.logf("discarded the recording: the engine is no longer selected")
		go capture.Stop()
		return
	case keyed && !h.consuming:
		h.mu.Unlock()
		h.logf("discarded the recording: the key was released before it started")
		go capture.Stop()
		return
	}
	h.holding = recording
	h.mu.Unlock()

	h.logf("recording")
	go h.collect(recording)
}

// stop ends the hold in flight.
func (h *PushToTalk) stop() {
	h.mu.Lock()
	recording := h.holding
	h.holding, h.consuming = nil, false
	h.mu.Unlock()
	if recording == nil {
		return
	}
	go recording.capture.Stop()
}

// collect reads the capture to its end and hands the audio to the decoder.
func (h *PushToTalk) collect(recording *hold) {
	cfg := h.Config
	// The cap always exists, whatever the setting says: it is what bounds a forgotten
	// key, and an unbounded recording is a memory leak in a process that outlives every
	// dictation. A zero or negative setting falls back to the detector's own default.
	maxSeconds := cfg.MaxUtteranceSeconds
	if maxSeconds <= 0 {
		maxSeconds = 60
	}
	limit := int64(maxSeconds * audio.SampleRate)

	var (
		pcm     []byte
		dropped int64
		capped  bool
	)
	for chunk := range recording.capture.Chunks() {
		pcm = append(pcm, chunk.Data...)
		dropped += chunk.DroppedSamples
		// A forgotten key must not grow the recording without bound. The cap ends the
		// capture here rather than at the key, because the key may not come up for
		// another minute.
		if limit > 0 && int64(audio.SampleCount(pcm)) >= limit {
			capped = true
			break
		}
	}
	recording.detach(h)
	if capped {
		go recording.capture.Stop()
		h.logf("recording stopped at the %.0fs limit (max_utterance_seconds)", maxSeconds)
	}
	if err := recording.capture.Err(); err != nil {
		h.logf("capture ended: %v", err)
	}
	if recording.discard.Load() {
		return
	}
	if dropped > 0 {
		// The one way audio the user spoke never reaches the model at all: it is the
		// difference between "the model misheard me" and "the machine could not keep up".
		h.logf("capture dropped %.2fs of audio; that speech is missing from the recording",
			float64(dropped)/audio.SampleRate)
	}
	if len(pcm) == 0 {
		h.logf("no audio was recorded; nothing was sent")
		return
	}
	h.decode(utterance{
		pcm:     pcm,
		seconds: float64(audio.SampleCount(pcm)) / audio.SampleRate,
	})
}

// utterance is one hold's audio, waiting for the decoder: the whole recording, exactly as
// the capture produced it.
type utterance struct {
	pcm     []byte
	seconds float64
}

// decode queues one recording for the worker.
func (h *PushToTalk) decode(recorded utterance) {
	h.once.Do(func() { go h.work() })
	select {
	case h.jobs <- recorded:
	default:
		h.logf("transcription queue full; %.2fs of audio was dropped", recorded.seconds)
	}
}

// work decodes recordings one at a time and delivers their text in order.
func (h *PushToTalk) work() {
	transcriber := h.transcriber()
	for job := range h.jobs {
		started := time.Now()
		text, err := transcriber.Transcribe(context.Background(), job.pcm)
		if err != nil {
			h.logf("transcribe %.2fs of audio: %v", job.seconds, err)
			continue
		}
		text = strings.TrimSpace(text)
		if h.normalizer != nil {
			text = strings.TrimSpace(h.normalizer.Normalize(text))
		}
		if text == "" {
			h.logf("the model heard no words in %.2fs of audio", job.seconds)
			continue
		}
		if !hasContent(text) {
			h.logf("the model answered only punctuation (%q); nothing was committed", text)
			continue
		}
		// A decode is worth reporting even when delivery fails: the two failures look
		// identical from the application ("nothing appeared") and have different fixes.
		h.logf("decoded %.2fs of audio in %v: %s", job.seconds, time.Since(started).Round(time.Millisecond), text)
		if err := h.commit(text); err != nil {
			h.logf("deliver %q: %v", firstRunes(text, 40), err)
		}
	}
}

// commit delivers text to the input context the user is in.
func (h *PushToTalk) commit(text string) error {
	h.mu.Lock()
	sink := h.sink
	h.mu.Unlock()
	if sink == nil {
		return fmt.Errorf("no input context is attached to the engine")
	}
	if err := sink.Commit(text); err != nil {
		return err
	}
	// The line that says text left this process. Without it the log could not tell
	// "nothing was decoded" from "the application did not show it".
	h.logf("committed %d character(s): %s", len([]rune(text)), firstRunes(text, 120))
	return nil
}

func (h *PushToTalk) newCapture() (audio.Capture, error) {
	if h.NewCapture != nil {
		return h.NewCapture()
	}
	return audio.NewMicrophone(h.Config)
}

// transcriber is the offline route: one reading of the whole recording.
func (h *PushToTalk) transcriber() asr.Transcriber {
	if h.NewTranscriber != nil {
		return h.NewTranscriber()
	}
	return asr.ServerTranscriber{
		BaseURL:  h.Config.ASRServerURL,
		Language: h.Config.Language,
		Timeout:  time.Duration(h.Config.ASRServerTimeoutMS) * time.Millisecond,
	}
}

// hasContent reports whether text carries a letter or a digit, which is what separates a
// transcript from the punctuation a recogniser emits for audio it could not read.
func hasContent(text string) bool {
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// firstRunes shortens text for a log line without splitting a character in half.
func firstRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}
