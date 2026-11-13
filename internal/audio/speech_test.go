package audio

import (
	"math"
	"testing"
)

func tone(seconds, amplitude float64) []byte {
	samples := make([]int16, int(seconds*SampleRate))
	frequency := 2 * math.Pi * 220 / SampleRate
	for i := range samples {
		samples[i] = int16(amplitude * math.Sin(frequency*float64(i)))
	}
	return SamplesToBytes(samples)
}

// The gate answers one question - is there speech here - and never edits the audio.
func TestHasSpeech(t *testing.T) {
	silence := make([]byte, SampleRate*2*BytesPerSample)
	if HasSpeech(silence, 600, 300) {
		t.Error("silence was called speech")
	}
	if !HasSpeech(tone(0.5, 6000), 600, 300) {
		t.Error("a loud tone was not called speech")
	}
	// Below the configured minimum duration.
	if HasSpeech(tone(0.1, 6000), 600, 300) {
		t.Error("a 100ms burst was called speech, under the 300ms minimum")
	}
	// A single frame above the threshold is a click, not a word.
	click := make([]byte, SampleRate*BytesPerSample)
	copy(click, tone(0.02, 9000))
	if HasSpeech(click, 600, 300) {
		t.Error("one 20ms frame was called speech")
	}
	// Quiet speech: above the floor, well under a shout.
	if !HasSpeech(tone(0.6, 900), 600, 300) {
		t.Error("speech at RMS 640 was not called speech")
	}
}
