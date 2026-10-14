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

func TestSegmenterFindsSpeechBetweenSilence(t *testing.T) {
	segmenter := NewSegmenter(SegmenterConfig{
		SilenceMS:   200,
		MinSpeechMS: 100,
		Threshold:   350,
	})

	segmenter.Push(Silence(0.5), 0)
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
	events = segmenter.Push(Silence(0.5), 24000)
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
	events := segmenter.Push(Silence(0.5), 1600)

	for _, event := range events {
		if event.Kind == SpeechEnd {
			t.Fatalf("a 100ms burst produced a segment: %+v", event)
		}
	}
	// After a rejected burst the detector must be ready for real speech again.
	segmenter.Push(speech(1.0, 8000), 9600)
	if events := segmenter.Push(Silence(0.5), 25600); len(events) == 0 || events[0].Kind != SpeechEnd {
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
	if events := segmenter.Push(Silence(0.3), 16000); len(events) != 0 {
		t.Errorf("a 300ms pause ended the utterance: %+v", events)
	}
	segmenter.Push(speech(1.0, 6000), 20800)
	events := segmenter.Push(Silence(1.0), 36800)
	if len(events) != 1 || events[0].Kind != SpeechEnd {
		t.Fatalf("events after the real end = %+v, want one speech_end", events)
	}
	if got := events[0].DurationSeconds(); got < 1.8 || got > 2.4 {
		t.Errorf("segment duration = %.2fs, want the two bursts joined (~2.0s)", got)
	}
}

func TestSegmenterAdaptsToANoisyRoom(t *testing.T) {
	segmenter := NewSegmenter(SegmenterConfig{
		SilenceMS:   300,
		MinSpeechMS: 100,
		Threshold:   350,
		Adaptive:    true,
	})
	// A room tone above the configured threshold: without adaptation this would
	// be heard as continuous speech and no endpoint would ever fire.
	roomTone := speech(1.0, 800)
	segmenter.Calibrate(roomTone)
	segmenter.Push(roomTone, 0)
	events := segmenter.Push(roomTone, 16000)
	for _, event := range events {
		if event.Kind == SpeechStart {
			t.Fatalf("room tone above the raw threshold was detected as speech: %+v", event)
		}
	}
	if threshold := segmenter.Threshold(); threshold <= 800 {
		t.Errorf("Threshold() = %.0f, want it raised above the measured room tone", threshold)
	}
}
