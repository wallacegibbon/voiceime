package audio

import (
	"math"
	"time"
)

// SegmentKind names an endpoint event.
type SegmentKind string

const (
	// SpeechStart fires once when the detector decides the user has begun.
	SpeechStart SegmentKind = "speech_start"
	// SpeechEnd fires when the utterance is over: the point at which provisional
	// text can be committed and repaired.
	SpeechEnd SegmentKind = "speech_end"
)

// SegmentEvent is one endpoint decision, positioned in the audio stream.
type SegmentEvent struct {
	Kind SegmentKind `json:"kind"`
	// StartSample and EndSample delimit the speech that just occurred. EndSample
	// is where repair should stop reading, StartSample where it should start.
	StartSample int64 `json:"start_sample"`
	EndSample   int64 `json:"end_sample"`
	// Probability is the speech likelihood of the frame that triggered the event,
	// useful when tuning thresholds from a log.
	Probability float64 `json:"probability,omitempty"`
	// Elapsed is wall time since the first frame, for latency reporting only.
	Elapsed time.Duration `json:"-"`
}

// DurationSeconds is the length of the segment being reported.
func (e SegmentEvent) DurationSeconds() float64 {
	return float64(e.EndSample-e.StartSample) / SampleRate
}

// SegmenterConfig mirrors the endpointing settings.
type SegmenterConfig struct {
	// SilenceMS is how long the user must be quiet before the utterance ends.
	SilenceMS int
	// MinSpeechMS is the shortest speech that counts; shorter bursts are noise.
	MinSpeechMS int
	// Threshold is the RMS amplitude below which a frame is silence.
	Threshold float64
	// MaxUtteranceSeconds forces a boundary so a forgotten hotkey cannot grow
	// the repair buffer without bound.
	MaxUtteranceSeconds float64
	// Adaptive lets the detector raise the threshold above a measured noise
	// floor. A fan, a fanless laptop's coil whine or a noisy room would
	// otherwise keep the detector in the speech state forever.
	Adaptive bool
}

// segmenterState is the detector's position in the speech/silence cycle.
type segmenterState int

const (
	stateQuiet segmenterState = iota
	stateSpeaking
)

// minInterruptMS is how long speech must last before it interrupts the silence
// hangover and extends the utterance.
//
// Without it, a single frame above the threshold resets the hangover counter, and
// an impulsive noise source - a keystroke, a mouse click, a chair - is enough to
// keep the detector in the speaking state indefinitely. Measured in the reference
// room: the noise floor sits at RMS 309 with a 20 ms p99 of 7992 and a peak of
// 23873, so clicks were resetting the 800 ms hangover about once a second and a
// silent room produced 4-10 second "utterances" of hallucinated text. Clicks are
// a few frames long; syllables are not.
const minInterruptMS = 120

// Segmenter turns an audio stream into SpeechStart/SpeechEnd events.
//
// It is energy-based rather than model-based on purpose. The ASR models do run a
// Silero VAD internally, but only to pick offline chunk boundaries: in streaming
// mode they neither accept a VAD caller nor report voice activity, so the
// endpoint decision - the thing that decides when text becomes committable - has
// to be made here. Keeping it in voiceime also means it costs no extra RAM, which
// matters because a 0.6B repair model already needs a gigabyte.
type Segmenter struct {
	cfg SegmenterConfig

	state            segmenterState
	speechStart      int64
	lastSpeechSample int64
	silenceSamples   int64
	speechRun        int64
	pos              int64

	// noiseFloor tracks the quietest frame seen recently, which is what allows
	// the threshold to adapt upward in a noisy room. It follows the level down
	// immediately and upward slowly, so one loud frame cannot desensitise the
	// detector and a genuinely noisy room is still learned in a few seconds.
	noiseFloor float64
	pending    []SegmentEvent
	started    time.Time
}

// NewSegmenter builds a detector from configuration.
func NewSegmenter(cfg SegmenterConfig) *Segmenter {
	if cfg.SilenceMS <= 0 {
		cfg.SilenceMS = 800
	}
	if cfg.MinSpeechMS <= 0 {
		cfg.MinSpeechMS = 300
	}
	if cfg.Threshold <= 0 {
		cfg.Threshold = 350
	}
	if cfg.MaxUtteranceSeconds <= 0 {
		cfg.MaxUtteranceSeconds = 60
	}
	segmenter := &Segmenter{cfg: cfg, started: time.Now()}
	// Seed the noise floor with the configured threshold so a stream that never
	// calibrates still behaves like the plain threshold detector.
	segmenter.noiseFloor = cfg.Threshold
	return segmenter
}

// Calibrate measures the noise floor from a block of known silence, which the
// daemon supplies from the first milliseconds of capture.
//
// Not calling it is the normal case and is not a failure: the floor is tracked
// from the stream itself (see frame), which is safer than trusting the opening
// milliseconds - a user who starts talking immediately would otherwise measure
// speech as their noise floor and the detector would then ignore everything.
// Calibrate exists for a caller that does have a block of known silence.
func (s *Segmenter) Calibrate(silence []byte) {
	if len(silence) == 0 {
		return
	}
	level := RMS(silence)
	if level <= 0 {
		return
	}
	s.noiseFloor = level
}

// adaptiveFactor is how far above the measured noise floor speech must be. Two is
// the smallest factor that separates a room's noise from normal speech in the
// measurements recorded in docs/measurements.md.
const adaptiveFactor = 2

// Threshold is the RMS level currently treated as the speech boundary: twice the
// measured noise floor, but never below the configured value. Adapting *downward*
// would start treating quiet speech as silence, which loses words.
func (s *Segmenter) Threshold() float64 {
	if !s.cfg.Adaptive {
		return s.cfg.Threshold
	}
	if adaptive := s.noiseFloor * adaptiveFactor; adaptive > s.cfg.Threshold {
		return adaptive
	}
	return s.cfg.Threshold
}

// Push feeds one block of audio and returns any events it produced. startSample
// is the block's position in the stream, so events carry absolute positions
// whether the audio came from a microphone or a file.
func (s *Segmenter) Push(data []byte, startSample int64) []SegmentEvent {
	s.pos = startSample
	s.pending = s.pending[:0]

	frameBytes := FrameMS * SampleRate / 1000 * BytesPerSample
	for offset := 0; offset+frameBytes <= len(data); offset += frameBytes {
		sample := startSample + int64(offset/BytesPerSample)
		s.frame(data[offset:offset+frameBytes], sample)
	}
	return s.pending
}

// Flush ends an utterance in progress, which is what a hotkey release or an
// explicit stop should do: the user has finished speaking, so the provisional
// text is committable even though no silence was captured.
//
// It applies the same minimum-speech rule as a normal boundary. Without it, a
// single keystroke in the last frames of a session looked like an utterance and
// committed whatever the recogniser hallucinated from it - measured as a 0.02s
// "segment" and a committed "。".
func (s *Segmenter) Flush() []SegmentEvent {
	s.pending = s.pending[:0]
	if s.state == stateSpeaking && s.lastSpeechSample > s.speechStart && s.speechLongEnough() {
		s.pending = append(s.pending, s.endEvent(s.lastSpeechSample, 0))
	}
	return s.pending
}

func (s *Segmenter) frame(frame []byte, sample int64) {
	level := RMS(frame)
	// Track the floor only while quiet, and only downward quickly / upward
	// slowly, so a single loud frame cannot desensitise the detector. While
	// speaking the floor is frozen: a long utterance would otherwise drag it up.
	if s.state == stateQuiet {
		if level < s.noiseFloor {
			s.noiseFloor = level
		} else {
			s.noiseFloor = s.noiseFloor*0.995 + level*0.005
		}
	}
	speech := level >= s.Threshold()
	frameSamples := int64(SampleCount(frame))

	switch s.state {
	case stateQuiet:
		if speech {
			s.state = stateSpeaking
			s.speechStart = sample
			s.lastSpeechSample = sample + frameSamples
			s.silenceSamples = 0
			s.speechRun = frameSamples
			s.pending = append(s.pending, SegmentEvent{
				Kind:        SpeechStart,
				StartSample: sample,
				EndSample:   sample + frameSamples,
				Probability: level,
				Elapsed:     time.Since(s.started),
			})
		}
	case stateSpeaking:
		if speech {
			s.speechRun += frameSamples
			// Only sustained speech counts as "the user is still talking": see
			// minInterruptMS. Until it is sustained the frame is treated as
			// neither speech nor silence, so the hangover keeps counting.
			if s.speechRun >= minInterruptSamples() {
				s.lastSpeechSample = sample + frameSamples
				s.silenceSamples = 0
			}
		} else {
			s.speechRun = 0
			s.silenceSamples += frameSamples
		}

		silentFor := time.Duration(s.silenceSamples) * time.Second / SampleRate
		if silentFor >= time.Duration(s.cfg.SilenceMS)*time.Millisecond {
			// Only report a segment that contained enough speech to be words.
			if s.speechLongEnough() {
				s.pending = append(s.pending, s.endEvent(s.lastSpeechSample, level))
			} else {
				s.state = stateQuiet
			}
			return
		}

		if float64(s.lastSpeechSample-s.speechStart)/SampleRate >= s.cfg.MaxUtteranceSeconds {
			s.pending = append(s.pending, s.endEvent(s.lastSpeechSample, level))
		}
	}
}

func (s *Segmenter) speechLongEnough() bool {
	minutes := s.cfg.MinSpeechMS
	spoken := float64(s.lastSpeechSample-s.speechStart) / SampleRate * 1000
	return spoken >= float64(minutes)
}

func (s *Segmenter) endEvent(endSample int64, probability float64) SegmentEvent {
	event := SegmentEvent{
		Kind:        SpeechEnd,
		StartSample: s.speechStart,
		EndSample:   endSample,
		Probability: probability,
		Elapsed:     time.Since(s.started),
	}
	s.state = stateQuiet
	s.silenceSamples = 0
	s.speechRun = 0
	return event
}

// minInterruptSamples is minInterruptMS as a sample count.
func minInterruptSamples() int64 { return int64(SampleRate) * minInterruptMS / 1000 }

// SpeechProbability is a convenience for logs: how far above the threshold the
// current frame is, mapped through a soft saturating curve.
func (s *Segmenter) SpeechProbability(level float64) float64 {
	threshold := s.Threshold()
	if threshold <= 0 {
		return math.NaN()
	}
	return 1 - math.Exp(-level/threshold)
}
