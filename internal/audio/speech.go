package audio

import "math"

// HasSpeech reports whether a recording contains speech: a run of frames at or above
// threshold lasting at least minSpeechMS.
//
// It decides only whether a recording is worth sending. Nothing is trimmed: the audio that
// passes this test goes to the model in full. It exists because a recogniser asked to read
// a room answers with words - measured on this machine, SenseVoice returns "我。" for two
// seconds of digital silence and "。" for two seconds of room - so a mis-pressed trigger
// would otherwise type something.
func HasSpeech(pcm []byte, threshold float64, minSpeechMS int) bool {
	if minSpeechMS <= 0 {
		minSpeechMS = 300
	}
	frameBytes := FrameMS * SampleRate / 1000 * BytesPerSample
	needed := SampleRate * minSpeechMS / 1000
	run := 0
	for offset := 0; offset+frameBytes <= len(pcm); offset += frameBytes {
		if rms(pcm[offset:offset+frameBytes]) >= threshold {
			run += SampleCount(pcm[offset : offset+frameBytes])
			if run >= needed {
				return true
			}
			continue
		}
		run = 0
	}
	return false
}

// rms is the root-mean-square level of one block of 16-bit samples.
func rms(pcm []byte) float64 {
	count := SampleCount(pcm)
	if count == 0 {
		return 0
	}
	total := 0.0
	for index := 0; index < count; index++ {
		sample := float64(int16(pcm[index*2]) | int16(pcm[index*2+1])<<8)
		total += sample * sample
	}
	return math.Sqrt(total / float64(count))
}
