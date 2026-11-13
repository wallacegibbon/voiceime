package daemon

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/wallacegibbon/voiceime/internal/audio"
)

// tone is audio loud enough for a speech detector to call it speech.
func tone(seconds, amplitude float64) []byte {
	samples := make([]int16, int(seconds*audio.SampleRate))
	frequency := 2 * math.Pi * 220 / audio.SampleRate
	for i := range samples {
		samples[i] = int16(amplitude * math.Sin(frequency*float64(i)))
	}
	return audio.SamplesToBytes(samples)
}

func silence(seconds float64) []byte {
	return make([]byte, int(seconds*audio.SampleRate)*audio.BytesPerSample)
}

// writeWAV writes 16 kHz mono s16le PCM as a WAV file.
func writeWAV(t *testing.T, path string, pcm []byte) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer file.Close()

	const channels, rate, bits = 1, audio.SampleRate, 16
	data := uint32(len(pcm))
	header := make([]byte, 44)
	copy(header[0:], "RIFF")
	binary.LittleEndian.PutUint32(header[4:], 36+data)
	copy(header[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(header[16:], 16)
	binary.LittleEndian.PutUint16(header[20:], 1)
	binary.LittleEndian.PutUint16(header[22:], channels)
	binary.LittleEndian.PutUint32(header[24:], rate)
	binary.LittleEndian.PutUint32(header[28:], rate*channels*bits/8)
	binary.LittleEndian.PutUint16(header[32:], channels*bits/8)
	binary.LittleEndian.PutUint16(header[34:], bits)
	copy(header[36:], "data")
	binary.LittleEndian.PutUint32(header[40:], data)
	if _, err := file.Write(append(header, pcm...)); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// speechWAV is a recording with a phrase in the middle, for the file diagnostic.
func speechWAV(t *testing.T) string {
	t.Helper()
	payload := append(append(silence(0.2), tone(0.6, 6000)...), silence(0.5)...)
	path := filepath.Join(t.TempDir(), "speech.wav")
	writeWAV(t, path, payload)
	return path
}
