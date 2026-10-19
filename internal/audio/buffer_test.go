package audio

import "testing"

func TestRollingBufferSlice(t *testing.T) {
	buffer := NewRollingBuffer(1) // one second
	buffer.Append(silence(0.5), 0)
	buffer.Append(silence(0.5), 8000)

	if _, ok := buffer.Slice(0, 16000); !ok {
		t.Error("Slice(0, 16000) failed for a range that is fully held")
	}
	if _, ok := buffer.Slice(0, 32000); ok {
		t.Error("Slice beyond the held range succeeded")
	}
	if _, ok := buffer.Slice(-1000, 1000); ok {
		t.Error("Slice before the held range succeeded")
	}

	// Appending past the limit drops the oldest audio, and the reported start
	// moves with it, so a later slice of the newest audio still works.
	buffer.Append(silence(0.5), 16000)
	if held := DurationSeconds(buffer.data); held > 1.0001 {
		t.Errorf("held audio = %v, want at most 1", held)
	}
	if _, ok := buffer.Slice(16000, 24000); !ok {
		t.Error("Slice of the newest audio failed after the buffer wrapped")
	}
}

func TestRollingBufferRestartsAfterAGap(t *testing.T) {
	buffer := NewRollingBuffer(5)
	buffer.Append(silence(0.1), 0)
	// A dropped chunk means the buffer can no longer describe a contiguous
	// range; it must restart rather than silently splice unrelated audio.
	buffer.Append(silence(0.1), 16000)
	if _, ok := buffer.Slice(0, 8000); ok {
		t.Error("Slice spanning a dropped block succeeded, want failure")
	}
	if _, ok := buffer.Slice(16000, 17600); !ok {
		t.Error("Slice of post-gap audio failed")
	}
}

// repeated returns a block of one repeated byte, so a test can tell which append a
// byte came from.
func repeated(value byte, bytes int) []byte {
	out := make([]byte, bytes)
	for i := range out {
		out[i] = value
	}
	return out
}

// A slice handed to the repair worker must not change while it is being decoded.
//
// The capture loop keeps filling the buffer after the slice is taken, and the gap
// path restarts it at offset zero of the same array. A live view therefore had every
// byte replaced under the reader - measured before this test: a queued repair would
// have decoded the next block instead of its utterance and had that text committed
// as the corrected sentence.
func TestSliceDoesNotAliasTheLiveBuffer(t *testing.T) {
	buffer := NewRollingBuffer(10)
	buffer.Append(repeated(0x11, 32000), 0)

	pcm, ok := buffer.Slice(0, 16000)
	if !ok {
		t.Fatal("Slice failed for a range that is fully held")
	}

	// A dropped block: the buffer restarts at offset zero of the same array.
	buffer.Append(repeated(0x22, 32000), 9_000_000)
	for i, got := range pcm {
		if got != 0x11 {
			t.Fatalf("byte %d of the handed-out PCM became %#x: the buffer aliased it", i, got)
		}
	}

	// SliceClamped is the other way audio leaves the buffer, so it must not alias
	// either - including when the next block is another restart.
	start := buffer.startSample
	clamped, _, _, ok := buffer.SliceClamped(start, start+8000)
	if !ok {
		t.Fatal("SliceClamped failed for a range that is held")
	}
	buffer.Append(repeated(0x33, 32000), 18_000_000)
	for i, got := range clamped {
		if got != 0x22 {
			t.Fatalf("byte %d of the clamped range became %#x: the buffer aliased it", i, got)
		}
	}
}

// Keeping the buffer full must not allocate: it is full for the whole of a long
// session, so an allocation per block is a megabyte of garbage every second on the
// real-time audio path. The window is bounded, so the shift can happen in place.
func TestRollingBufferAppendIsAllocationFreeWhenFull(t *testing.T) {
	buffer := NewRollingBuffer(60)
	frame := repeated(0, 640) // 20ms at 16kHz mono
	for i := 0; i < 3000; i++ {
		buffer.Append(frame, int64(i*320))
	}
	if held := len(buffer.data); held != buffer.limitBytes {
		t.Fatalf("held %d bytes, want the buffer full at %d", held, buffer.limitBytes)
	}

	next := int64(buffer.startSample) + int64(SampleCount(buffer.data))
	allocs := testing.AllocsPerRun(100, func() {
		buffer.Append(frame, next)
		next += int64(SampleCount(frame))
	})
	if allocs > 0 {
		t.Errorf("%v allocations per append with the buffer full, want 0", allocs)
	}
}
