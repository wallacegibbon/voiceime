package audio

// RollingBuffer keeps the most recent audio of a bounded length so the repair
// pass can re-read a finished segment.
//
// Bounded is the point: a repair needs only the utterance that just ended, and an
// unbounded buffer would grow with session length until the machine ran out of
// memory. Old audio is dropped from the front, and Slice reports when a requested
// range has already fallen off so the caller can skip the repair rather than
// decode a truncated segment.
type RollingBuffer struct {
	data        []byte
	startSample int64
	limitBytes  int
}

// NewRollingBuffer returns a buffer holding at most seconds of audio.
func NewRollingBuffer(seconds float64) *RollingBuffer {
	return &RollingBuffer{limitBytes: int(seconds*SampleRate) * BytesPerSample}
}

// Append adds a block that starts at startSample. Blocks must arrive in order.
func (b *RollingBuffer) Append(data []byte, startSample int64) {
	if len(b.data) == 0 {
		b.startSample = startSample
	}
	if expected := b.startSample + int64(SampleCount(b.data)); startSample != expected {
		// A gap means a dropped chunk: the buffer can no longer describe a
		// contiguous range, so it restarts at the new block. Slicing then fails
		// for ranges that span the gap, which is the honest outcome.
		b.data = b.data[:0]
		b.startSample = startSample
	}
	b.data = append(b.data, data...)
	if b.limitBytes > 0 && len(b.data) > b.limitBytes {
		drop := len(b.data) - b.limitBytes
		drop -= drop % BytesPerSample
		b.data = append([]byte(nil), b.data[drop:]...)
		b.startSample += int64(drop / BytesPerSample)
	}
}

// Slice returns the audio covering [startSample, endSample). ok is false when the
// range is not fully held, which happens after a long session or a dropped block.
func (b *RollingBuffer) Slice(startSample, endSample int64) ([]byte, bool) {
	if endSample <= startSample {
		return nil, false
	}
	heldEnd := b.startSample + int64(SampleCount(b.data))
	if startSample < b.startSample || endSample > heldEnd {
		return nil, false
	}
	from := int(startSample-b.startSample) * BytesPerSample
	to := int(endSample-b.startSample) * BytesPerSample
	return b.data[from:to], true
}

// Reset forgets everything.
func (b *RollingBuffer) Reset() {
	b.data = b.data[:0]
	b.startSample = 0
}

// HeldSeconds is how much audio the buffer currently holds.
func (b *RollingBuffer) HeldSeconds() float64 { return DurationSeconds(b.data) }
