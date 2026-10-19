package audio

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestSampleRoundTrip(t *testing.T) {
	samples := []int16{0, 1, -1, 32767, -32768, 1234}
	data := SamplesToBytes(samples)
	if got := len(data); got != len(samples)*2 {
		t.Fatalf("SamplesToBytes length = %d, want %d", got, len(samples)*2)
	}
	back := BytesToSamples(data)
	for i := range samples {
		if back[i] != samples[i] {
			t.Fatalf("sample %d round-tripped to %d, want %d", i, back[i], samples[i])
		}
	}
	// A truncated read must not panic or invent a sample.
	if got := len(BytesToSamples(data[:len(data)-1])); got != len(samples)-1 {
		t.Errorf("odd trailing byte produced %d samples, want %d", got, len(samples)-1)
	}
}

func TestRMSAndPeak(t *testing.T) {
	quiet := silence(0.1)
	if level := RMS(quiet); level != 0 {
		t.Errorf("RMS(silence) = %v, want 0", level)
	}
	tone := SamplesToBytes([]int16{1000, -1000, 1000, -1000})
	if level := RMS(tone); math.Abs(level-1000) > 0.001 {
		t.Errorf("RMS(square) = %v, want 1000", level)
	}
	if peak := peak(tone); peak != 1000 {
		t.Errorf("Peak = %d, want 1000", peak)
	}
	if got := DurationSeconds(silence(1)); math.Abs(got-1) > 1e-9 {
		t.Errorf("DurationSeconds(1s of silence) = %v", got)
	}
}

func TestResampleHalvesLength(t *testing.T) {
	// A 1 kHz tone sampled at 32 kHz, downsampled to 16 kHz, must come back with
	// half the samples at the pipeline rate.
	const sourceRate, targetRate = 32000, 16000
	count := sourceRate / 10
	samples := make([]int16, count)
	frequency := 2 * math.Pi * 1000 / sourceRate
	for i := range samples {
		samples[i] = int16(10000 * math.Sin(frequency*float64(i)))
	}
	out := Resample(SamplesToBytes(samples), sourceRate, targetRate)
	if got, want := SampleCount(out), count/2; got != want {
		t.Errorf("resampled sample count = %d, want %d", got, want)
	}
	// The result must be a recognisable tone, not garbage: check its RMS is in
	// the same ballpark as the input's.
	if in, out := RMS(SamplesToBytes(samples)), RMS(out); math.Abs(in-out)/in > 0.1 {
		t.Errorf("resample changed the level: in %v out %v", in, out)
	}
}

func TestWAVRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sample.wav")
	original := SamplesToBytes([]int16{0, 500, -500, 1000, -1000})
	if err := os.WriteFile(path, EncodeWAV(original, SampleRate, 1), 0o644); err != nil {
		t.Fatalf("write wav: %v", err)
	}
	wav, err := ReadWAV(path)
	if err != nil {
		t.Fatalf("ReadWAV: %v", err)
	}
	if wav.Rate != SampleRate || wav.Channels != 1 {
		t.Errorf("ReadWAV = rate %d channels %d, want %d/1", wav.Rate, wav.Channels, SampleRate)
	}
	if len(wav.Data) != len(original) {
		t.Fatalf("round-tripped %d bytes, want %d", len(wav.Data), len(original))
	}
	for i := range original {
		if wav.Data[i] != original[i] {
			t.Fatalf("byte %d = %d, want %d", i, wav.Data[i], original[i])
		}
	}
}

func TestReadWAVRejectsNonPCM(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bogus.wav")
	if err := WriteFileForTest(path, []byte("not a wav file at all, but long enough to look like one")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadWAV(path); err == nil {
		t.Error("ReadWAV accepted a file that is not a WAV")
	}
}

// silence builds a block of digital silence, and peak measures the loudest
// sample in one. Both live here rather than in format.go: they are fixtures, and
// a production package should not carry functions that only its tests call - that
// is how the adaptive noise floor came to be dead code for months.
func silence(seconds float64) []byte {
	return make([]byte, int(seconds*SampleRate)*BytesPerSample)
}

func peak(data []byte) int {
	best := 0
	for _, sample := range BytesToSamples(data) {
		value := int(sample)
		if value < 0 {
			value = -value
		}
		if value > best {
			best = value
		}
	}
	return best
}
