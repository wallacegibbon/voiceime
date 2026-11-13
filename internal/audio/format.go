// Package audio owns microphone capture, PCM arithmetic and endpointing.
package audio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
)

// SampleRate is the only rate the pipeline supports. See the package comment.
const SampleRate = 16000

// BytesPerSample is the width of one sample in the interchange format.
const BytesPerSample = 2

// FrameMS is the analysis frame used by the endpoint detector and the capture
// chunk size. 20 ms keeps endpointing responsive without waking up constantly.
const FrameMS = 20

// SamplesToBytes converts 16-bit samples to little-endian bytes.
func SamplesToBytes(samples []int16) []byte {
	out := make([]byte, len(samples)*BytesPerSample)
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(out[i*BytesPerSample:], uint16(sample))
	}
	return out
}

func BytesToSamples(data []byte) []int16 {
	count := len(data) / BytesPerSample
	out := make([]int16, count)
	for i := 0; i < count; i++ {
		out[i] = int16(binary.LittleEndian.Uint16(data[i*BytesPerSample:]))
	}
	return out
}

// SampleCount is the number of whole samples in a byte slice.
func SampleCount(data []byte) int { return len(data) / BytesPerSample }

// DurationSeconds is how long a byte slice of audio lasts.
func DurationSeconds(data []byte) float64 {
	return float64(SampleCount(data)) / SampleRate
}

// RMS returns the root-mean-square amplitude of a block.
func Resample(data []byte, fromRate, toRate int) []byte {
	if fromRate == toRate || len(data) == 0 {
		return data
	}
	source := BytesToSamples(data)
	if len(source) == 0 {
		return nil
	}
	ratio := float64(toRate) / float64(fromRate)
	outLen := int(float64(len(source)) * ratio)
	out := make([]int16, outLen)
	last := len(source) - 1
	for i := 0; i < outLen; i++ {
		position := float64(i) / ratio
		left := int(position)
		if left >= last {
			out[i] = source[last]
			continue
		}
		frac := position - float64(left)
		out[i] = int16(float64(source[left])*(1-frac) + float64(source[left+1])*frac)
	}
	return SamplesToBytes(out)
}

// WAV is a decoded 16-bit PCM WAV file.
type WAV struct {
	Data     []byte
	Rate     int
	Channels int
}

func EncodeWAV(data []byte, rate, channels int) []byte {
	if channels < 1 {
		channels = 1
	}
	frames := len(data) / BytesPerSample / channels
	byteRate := rate * channels * BytesPerSample
	blockAlign := channels * BytesPerSample

	var buffer strings.Builder
	buffer.WriteString("RIFF")
	writeUint32(&buffer, uint32(36+frames*blockAlign))
	buffer.WriteString("WAVE")
	buffer.WriteString("fmt ")
	writeUint32(&buffer, 16)
	writeUint16(&buffer, 1) // PCM
	writeUint16(&buffer, uint16(channels))
	writeUint32(&buffer, uint32(rate))
	writeUint32(&buffer, uint32(byteRate))
	writeUint16(&buffer, uint16(blockAlign))
	writeUint16(&buffer, 16)
	buffer.WriteString("data")
	writeUint32(&buffer, uint32(frames*blockAlign))

	return append([]byte(buffer.String()), data[:frames*blockAlign]...)
}

func writeUint32(buffer *strings.Builder, value uint32) {
	var raw [4]byte
	binary.LittleEndian.PutUint32(raw[:], value)
	buffer.Write(raw[:])
}

func writeUint16(buffer *strings.Builder, value uint16) {
	var raw [2]byte
	binary.LittleEndian.PutUint16(raw[:], value)
	buffer.Write(raw[:])
}

// ReadWAV decodes a 16-bit PCM WAV file, converting to mono 16 kHz as needed.
func ReadWAV(path string) (WAV, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return WAV{}, err
	}
	if len(raw) < 44 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		return WAV{}, fmt.Errorf("%s: not a RIFF/WAVE file", path)
	}

	var (
		rate     int
		channels int
		bits     int
		data     []byte
	)
	for offset := 12; offset+8 <= len(raw); {
		id := string(raw[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(raw[offset+4 : offset+8]))
		body := offset + 8
		if body+size > len(raw) {
			size = len(raw) - body
		}
		switch id {
		case "fmt ":
			if size < 16 {
				return WAV{}, fmt.Errorf("%s: truncated fmt chunk", path)
			}
			format := binary.LittleEndian.Uint16(raw[body : body+2])
			if format != 1 {
				return WAV{}, fmt.Errorf("%s: only uncompressed PCM is supported (format %d)", path, format)
			}
			channels = int(binary.LittleEndian.Uint16(raw[body+2 : body+4]))
			rate = int(binary.LittleEndian.Uint32(raw[body+4 : body+8]))
			bits = int(binary.LittleEndian.Uint16(raw[body+14 : body+16]))
		case "data":
			data = raw[body : body+size]
		}
		offset = body + size + (size & 1)
	}
	if rate == 0 || channels == 0 {
		return WAV{}, fmt.Errorf("%s: missing fmt chunk", path)
	}
	if bits != 16 {
		return WAV{}, fmt.Errorf("%s: only 16-bit PCM is supported, got %d-bit", path, bits)
	}

	if channels > 1 {
		data = downmix(data, channels)
	}
	if rate != SampleRate {
		data = Resample(data, rate, SampleRate)
		rate = SampleRate
	}
	return WAV{Data: data, Rate: rate, Channels: 1}, nil
}

// downmix averages interleaved channels into mono. Averaging rather than taking
// the first channel keeps a stereo recording with one silent side audible.
func downmix(data []byte, channels int) []byte {
	frames := len(data) / BytesPerSample / channels
	out := make([]int16, frames)
	for frame := 0; frame < frames; frame++ {
		sum := 0
		for channel := 0; channel < channels; channel++ {
			index := (frame*channels + channel) * BytesPerSample
			if index+BytesPerSample > len(data) {
				break
			}
			sum += int(int16(binary.LittleEndian.Uint16(data[index:])))
		}
		out[frame] = int16(sum / channels)
	}
	return SamplesToBytes(out)
}

// ErrNoAudio means a capture stream ended without producing anything usable.
var ErrNoAudio = errors.New("capture produced no audio")
