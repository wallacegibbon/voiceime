package daemon

import (
	"context"
	"sync"
	"time"

	"github.com/wallacegibbon/voiceime/internal/asr"
	"github.com/wallacegibbon/voiceime/internal/audio"
	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/ibus"
)

// switchableSink is where the session delivers text, with the target it delivers to
// swappable while a session runs.
//
// The engine process serves every input context in the session, and IBus calls Enable
// once per context. A session whose sink was fixed at the first activation kept
// delivering there after the user moved to another window: the text went to the context
// that started dictation, not the one being typed in. Delivering to the newest
// activation is the only reading of "dictate into the focused application" that survives
// more than one window.
type switchableSink struct {
	mu      sync.Mutex
	control ibus.Control
}

// use points the sink at an input context.
func (s *switchableSink) use(control ibus.Control) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.control = control
}

// target returns where text would go now.
func (s *switchableSink) target() ibus.Control {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.control
}

func (s *switchableSink) Commit(text string) error {
	if control := s.target(); control != nil {
		return control.Commit(text)
	}
	return nil
}

func (s *switchableSink) Preedit(text string, cursor int, visible bool) error {
	if control := s.target(); control != nil {
		return control.Preedit(text, cursor, visible)
	}
	return nil
}

// EngineHandler runs the dictation session while the IBus engine is active.
//
// The semantic is the one an input method already teaches: while this engine is
// selected, you are dictating. Activation starts the session, deactivation stops it and
// commits whatever was still provisional. No global hotkey is needed, because switching
// input methods is the hotkey.
type EngineHandler struct {
	Config *config.Config
	// ServerURL, StreamModel and RepairModel name the resident server and the two
	// entries a session uses.
	ServerURL   string
	StreamModel string
	RepairModel string
	// Transcriber overrides the offline decode the repair pass runs, for tests.
	Transcriber asr.Transcriber
	Log         func(format string, args ...any)

	// AudioFile replays a WAV instead of using the microphone, for the diagnostic
	// that exercises the whole path (engine → application text) without speaking.
	// A fresh replay is opened per session, because a replay is consumed by one.
	AudioFile string
	// MinRepairSeconds is passed through to the session.
	MinRepairSeconds float64

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	// contexts counts the input contexts this engine is active in. Dictation starts with
	// the first and stops when the last one goes away: stopping on the first deactivation
	// ended the session mid-sentence whenever the user moved between two windows.
	contexts int
	// sink is where the session delivers text. It outlives a session so the target can
	// follow the input context the user is in (see switchableSink).
	sink *switchableSink
	// lifecycle serialises Enable against Disable *including* the wait for the
	// previous session to finish. Without it a rapid deactivate-then-activate -
	// which the daemon can do while the first session is still committing its tail
	// - would start a second session while the first is still stopping: two
	// captures of the same microphone, and two text streams into one application.
	lifecycle sync.Mutex
}

func (h *EngineHandler) logf(format string, args ...any) {
	if h.Log != nil {
		h.Log(format, args...)
	}
}

// Enable starts dictation, or moves it to a new input context.
//
// It is called once per context the engine is active in. The first one starts the
// session; later ones only move the sink, because two sessions would capture the same
// microphone twice and interleave their text.
func (h *EngineHandler) Enable(control ibus.Control) {
	h.lifecycle.Lock()
	defer h.lifecycle.Unlock()

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sink == nil {
		h.sink = &switchableSink{}
	}
	h.sink.use(control)
	h.contexts++
	if h.cancel != nil {
		h.logf("engine enabled in another input context; dictating into the newest of %d", h.contexts)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan struct{})
	done := h.done

	options := Options{
		Config:           h.Config,
		ServerURL:        h.ServerURL,
		StreamModel:      h.StreamModel,
		RepairModel:      h.RepairModel,
		Transcriber:      h.Transcriber,
		Sink:             h.sink,
		MinRepairSeconds: h.MinRepairSeconds,
		Observer: Observer{
			Ready: func(load time.Duration) {
				h.logf("listening (model loaded in %v)", load)
			},
			Partial: func(text string, fedSeconds float64) {
				h.logf("partial at %.2fs: %s", fedSeconds, text)
			},
			Segment: func(start, end, speech float64) {
				h.logf("utterance %.2f-%.2fs (%.2fs of speech)", start, end, speech)
			},
			Repair: func(text string, took time.Duration, speech float64) {
				h.logf("repaired %.2fs of speech in %v: %s", speech, took, text)
			},
			Committed: func(text string) {
				// The line that says text left this process. Without it the log could
				// not tell "the session delivered nothing" from "the application did
				// not show it", which is the difference between a recognition problem
				// and a delivery one.
				h.logf("committed %d character(s): %s", len([]rune(text)), firstRunes(text, 120))
			},
			Skipped: func(reason string) { h.logf("skipped: %s", reason) },
			Error:   func(err error) { h.logf("error: %v", err) },
		},
	}

	if h.AudioFile != "" {
		path := h.AudioFile
		options.NewCapture = func() (audio.Capture, error) {
			return audio.NewFileReplay(path, true)
		}
	}

	go func() {
		defer close(done)
		result, err := options.Run(ctx)
		if err != nil && ctx.Err() == nil {
			h.logf("session ended: %v", err)
		}
		h.logf("session finished: %.2fs of audio, %d partial(s), text=%q",
			result.AudioFed.Seconds(), result.Partials, result.FinalText)
	}()
}

// Disable stops dictation, once the last input context has gone.
//
// The session commits the tail as it shuts down, so a deactivation mid-sentence does not
// lose words - and while another context is still active the session is left alone,
// because the user switched windows rather than stopped speaking.
func (h *EngineHandler) Disable() {
	h.lifecycle.Lock()
	defer h.lifecycle.Unlock()

	h.mu.Lock()
	if h.contexts > 0 {
		h.contexts--
	}
	if h.contexts > 0 {
		remaining := h.contexts
		h.mu.Unlock()
		h.logf("engine disabled in one input context; still dictating in %d", remaining)
		return
	}
	cancel, done := h.cancel, h.done
	h.cancel, h.done = nil, nil
	h.mu.Unlock()

	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// firstRunes shortens text for a log line without splitting a character in half.
func firstRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "…"
}

// KeyEvent never consumes a key: voiceime is not a keyboard layout, and taking
// keys would break typing in the application it is serving.
func (h *EngineHandler) KeyEvent(keyval, keycode, state uint32) bool { return false }
