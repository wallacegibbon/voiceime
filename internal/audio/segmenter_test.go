package audio

import (
	"math"
	"testing"
)

// The endpoint detector decides when text becomes committable, so its behaviour
// is pinned down with synthetic audio rather than left to chance.
func speech(seconds float64, amplitude int16) []byte {
	samples := make([]int16, int(seconds*SampleRate))
	frequency := 2 * math.Pi * 220 / SampleRate
	for i := range samples {
		samples[i] = int16(float64(amplitude) * math.Sin(frequency*float64(i)))
	}
	return SamplesToBytes(samples)
}

func TestSegmenterFindsSpeechBetweensilence(t *testing.T) {
	segmenter := NewSegmenter(SegmenterConfig{
		SilenceMS:   200,
		MinSpeechMS: 100,
		Threshold:   350,
	})

	segmenter.Push(silence(0.5), 0)
	events := segmenter.Push(speech(1.0, 6000), 8000)
	if len(events) != 1 || events[0].Kind != SpeechStart {
		t.Fatalf("events after speech began = %+v, want one speech_start", events)
	}
	if start := events[0].StartSample; start < 7600 || start > 8300 {
		t.Errorf("speech start = %d, want approximately 8000", start)
	}

	// The end fires once the silence hangover has elapsed, and it must report the
	// last speech sample rather than the end of the silence: the repair pass uses
	// that position to know what audio to re-decode.
	events = segmenter.Push(silence(0.5), 24000)
	if len(events) != 1 || events[0].Kind != SpeechEnd {
		t.Fatalf("events after silence = %+v, want one speech_end", events)
	}
	if end := events[0].EndSample; end < 23800 || end > 24400 {
		t.Errorf("speech end = %d, want approximately 24000 (end of speech, not end of silence)", end)
	}
	if events[0].DurationSeconds() < 0.9 {
		t.Errorf("segment duration = %.2fs, want about 1s", events[0].DurationSeconds())
	}
}

func TestSegmenterIgnoresShortBursts(t *testing.T) {
	segmenter := NewSegmenter(SegmenterConfig{
		SilenceMS:   200,
		MinSpeechMS: 500,
		Threshold:   350,
	})
	segmenter.Push(speech(0.1, 8000), 0)
	events := segmenter.Push(silence(0.5), 1600)

	for _, event := range events {
		if event.Kind == SpeechEnd {
			t.Fatalf("a 100ms burst produced a segment: %+v", event)
		}
	}
	// After a rejected burst the detector must be ready for real speech again.
	segmenter.Push(speech(1.0, 8000), 9600)
	if events := segmenter.Push(silence(0.5), 25600); len(events) == 0 || events[0].Kind != SpeechEnd {
		t.Errorf("real speech after a rejected burst produced %+v, want a segment", events)
	}
}

func TestSegmenterFlushEndsAnOpenUtterance(t *testing.T) {
	segmenter := NewSegmenter(SegmenterConfig{SilenceMS: 5000, MinSpeechMS: 100, Threshold: 350})
	segmenter.Push(speech(1.0, 6000), 0)
	events := segmenter.Flush()
	if len(events) != 1 || events[0].Kind != SpeechEnd {
		t.Fatalf("Flush() = %+v, want one speech_end", events)
	}
	if events[0].StartSample != 0 {
		t.Errorf("flushed segment start = %d, want 0", events[0].StartSample)
	}
	// Flushing twice must not invent a second segment.
	if events := segmenter.Flush(); len(events) != 0 {
		t.Errorf("second Flush() = %+v, want nothing", events)
	}
}

func TestSegmenterDoesNotSplitOnAPause(t *testing.T) {
	segmenter := NewSegmenter(SegmenterConfig{SilenceMS: 800, MinSpeechMS: 100, Threshold: 350})
	segmenter.Push(speech(1.0, 6000), 0)
	// A 300ms pause is shorter than the 800ms hangover, so it must not end the
	// utterance: splitting there would cut a sentence in half and force the user
	// to watch two repairs fight over one sentence.
	if events := segmenter.Push(silence(0.3), 16000); len(events) != 0 {
		t.Errorf("a 300ms pause ended the utterance: %+v", events)
	}
	segmenter.Push(speech(1.0, 6000), 20800)
	events := segmenter.Push(silence(1.0), 36800)
	if len(events) != 1 || events[0].Kind != SpeechEnd {
		t.Fatalf("events after the real end = %+v, want one speech_end", events)
	}
	if got := events[0].DurationSeconds(); got < 1.8 || got > 2.4 {
		t.Errorf("segment duration = %.2fs, want the two bursts joined (~2.0s)", got)
	}
}

// The production path never calls Calibrate: the daemon has no block of known
// silence to hand over, and trusting the opening milliseconds would measure
// speech as the noise floor for a user who starts talking immediately. The floor
// is therefore learned from the stream itself, and this test pins that - the
// previous version of this test called Calibrate, so it verified a path that
// nothing in the pipeline ran, and the detector was left on the static threshold.
// The helper's second argument is a sine *amplitude*, and the detector compares
// RMS, which for a sine is amplitude/√2. The numbers below are chosen with that
// in mind: the room tone has an RMS of ~212, the burst ~389 (above the configured
// 350, which is what makes it a discriminator) and the speech ~4243.
func TestSegmenterLearnsTheNoiseFloorFromTheStream(t *testing.T) {
	segmenter := NewSegmenter(SegmenterConfig{
		SilenceMS:   300,
		MinSpeechMS: 100,
		Threshold:   350,
		Adaptive:    true,
	})

	// A room tone below the configured threshold but well above half of it: the
	// threshold must follow it up, because a noise floor that close to the
	// threshold is what makes a detector flap and never close an utterance.
	roomTone := speech(1.0, 300)
	segmenter.Push(roomTone, 0)
	threshold := segmenter.Threshold()
	if threshold <= 350 {
		t.Fatalf("Threshold() = %.0f, want the measured room tone to raise it above the configured 350", threshold)
	}

	// A burst that the static threshold would have called speech is silence once
	// the floor is known. Without this, a noisy room produces segments out of
	// nothing and the text never gets committed.
	events := segmenter.Push(speech(1.0, 550), 16000)
	for _, event := range events {
		if event.Kind == SpeechStart {
			t.Fatalf("noise below the adapted threshold was detected as speech: %+v", event)
		}
	}

	// Real speech still gets through, and still ends: adapting upward must not
	// turn the detector deaf.
	segmenter.Push(speech(1.0, 6000), 32000)
	events = segmenter.Push(silence(0.5), 48000)
	if len(events) != 1 || events[0].Kind != SpeechEnd {
		t.Fatalf("events after real speech = %+v, want one speech_end", events)
	}
	if got := events[0].DurationSeconds(); got < 0.9 {
		t.Errorf("segment duration = %.2fs, want about 1s", got)
	}
}

// Adaptation must never lower the threshold: quiet speech below the configured
// value has to stay speech, or the feature would lose words.
func TestSegmenterNeverAdaptsBelowTheConfiguredThreshold(t *testing.T) {
	segmenter := NewSegmenter(SegmenterConfig{
		SilenceMS:   300,
		MinSpeechMS: 100,
		Threshold:   350,
		Adaptive:    true,
	})
	segmenter.Push(silence(1.0), 0)
	if threshold := segmenter.Threshold(); threshold != 350 {
		t.Fatalf("Threshold() = %.0f after a silent second, want the configured 350", threshold)
	}
	// Amplitude 700 is an RMS of ~495: above the configured threshold, so it must
	// be heard even with adaptation enabled.
	if events := segmenter.Push(speech(1.0, 700), 16000); len(events) != 1 || events[0].Kind != SpeechStart {
		t.Fatalf("events for speech above the configured threshold = %+v, want one speech_start", events)
	}
}

// An impulsive noise source must not extend an utterance. Before this rule, one
// frame above the threshold reset the silence hangover, so a keystroke could hold
// the detector in the speaking state and a silent room produced multi-second
// utterances of hallucinated text (measured: 9.9s).
func TestSegmenterIgnoresAClickInsideUtterance(t *testing.T) {
	segmenter := NewSegmenter(SegmenterConfig{SilenceMS: 200, MinSpeechMS: 100, Threshold: 350})
	segmenter.Push(speech(1.0, 6000), 0) // 16000 samples of speech
	segmenter.Push(silence(0.15), 16000)
	// A 20ms click: loud, but far shorter than minInterruptMS.
	segmenter.Push(speech(0.02, 20000), 18400)
	events := segmenter.Push(silence(0.15), 18720)

	if len(events) != 1 || events[0].Kind != SpeechEnd {
		t.Fatalf("events = %+v, want one speech_end: the click must not reset the hangover", events)
	}
	if events[0].EndSample != 16000 {
		t.Errorf("speech end = %d, want 16000: the click is not speech and must not be repaired", events[0].EndSample)
	}
}

// Flush ends an open utterance, but not one that never contained speech: the last
// frames of a session are as likely to hold a keystroke as a word, and repairing
// one commits whatever the recogniser hallucinated from it.
func TestSegmenterFlushIgnoresAMereClick(t *testing.T) {
	segmenter := NewSegmenter(SegmenterConfig{SilenceMS: 5000, MinSpeechMS: 300, Threshold: 350})
	segmenter.Push(speech(0.02, 8000), 0)
	if events := segmenter.Flush(); len(events) != 0 {
		t.Fatalf("Flush() after a single click = %+v, want nothing", events)
	}
}
