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

// EngineHandler runs the dictation session while the IBus engine is active.
//
// The semantic is the one an input method already teaches: while this engine is
// selected, you are dictating. Activation starts the session, deactivation stops
// it and commits whatever was still provisional. No global hotkey is needed,
// because switching input methods is the hotkey.
type EngineHandler struct {
	Config      *config.Config
	Binary      string
	Model       string
	RepairModel string
	// Transcriber overrides the offline decode the repair pass runs. Nil builds
	// the CLI Repairer; a resident server passes its own implementation.
	Transcriber asr.Transcriber
	// PreferredRepair, when set, brings up the route a repair prefers for one
	// session, with the CLI repairer as its fallback. It is called per activation.
	PreferredRepair func(ctx context.Context) (asr.Transcriber, error)
	// PreferredWait bounds how long a repair waits for PreferredRepair.
	PreferredWait time.Duration
	Log           func(format string, args ...any)

	// AudioFile replays a WAV instead of using the microphone, for the diagnostic
	// that exercises the whole path (engine → application text) without speaking.
	// A fresh replay is opened per session, because a replay is consumed by one.
	AudioFile string
	// MinRepairSeconds is passed through to the session.
	MinRepairSeconds float64

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
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

// Enable starts dictation. It is called again for every new input context, and
// only the first one starts a session: two sessions would capture the same
// microphone twice and interleave their text.
func (h *EngineHandler) Enable(control ibus.Control) {
	h.lifecycle.Lock()
	defer h.lifecycle.Unlock()

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cancel != nil {
		h.logf("engine enabled again; already dictating")
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel
	h.done = make(chan struct{})
	done := h.done

	options := Options{
		Config:           h.Config,
		Binary:           h.Binary,
		Model:            h.Model,
		RepairModel:      h.RepairModel,
		Transcriber:      h.Transcriber,
		PreferredRepair:  h.PreferredRepair,
		PreferredWait:    h.PreferredWait,
		Sink:             control,
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

// Disable stops dictation. The session commits the tail as it shuts down, so a
// deactivation mid-sentence does not lose words.
func (h *EngineHandler) Disable() {
	h.lifecycle.Lock()
	defer h.lifecycle.Unlock()

	h.mu.Lock()
	cancel, done := h.cancel, h.done
	h.cancel, h.done = nil, nil
	h.mu.Unlock()

	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// KeyEvent never consumes a key: voiceime is not a keyboard layout, and taking
// keys would break typing in the application it is serving.
func (h *EngineHandler) KeyEvent(keyval, keycode, state uint32) bool { return false }
