package audio

import (
	"errors"
	"io"
	"sync"
	"testing"
)

// gatedReader blocks until released and then reports EOF. It lets a test be sure
// the capture loop is inside Read before it acts: a requested stop has to be a
// request the loop can actually see, not a race against whether the loop has
// started yet.
type gatedReader struct {
	once    sync.Once
	reading chan struct{}
	release chan struct{}
}

func newGatedReader() *gatedReader {
	return &gatedReader{reading: make(chan struct{}), release: make(chan struct{})}
}

func (r *gatedReader) Read([]byte) (int, error) {
	r.once.Do(func() { close(r.reading) })
	<-r.release
	return 0, io.EOF
}

// A stop we asked for must not be reported as a capture failure.
//
// Stop closes the base and then kills the client, so the client dies on a signal
// and the reader sees EOF. wait() therefore reports "signal: killed" rather than
// a clean exit, and taking that at face value surfaced as a spurious
// "capture process exited: signal: killed" on every clean shutdown - the kind of
// message that makes a working session look like a broken device.
func TestRequestedStopIsNotACaptureError(t *testing.T) {
	base := newCaptureBase(SampleRate)
	reader := newGatedReader()
	done := make(chan struct{})
	go func() {
		defer close(done)
		base.readLoop(reader, func() error { return errors.New("signal: killed") })
	}()

	<-reader.reading
	base.Stop()
	close(reader.release)
	<-done

	if err := base.Err(); err != nil {
		t.Fatalf("Err() = %v after a requested stop, want nil", err)
	}
}

// The distinction is the request, not the exit status: a client that died on its
// own, with no stop requested, is still a failure worth surfacing.
func TestUnrequestedClientExitIsACaptureError(t *testing.T) {
	base := newCaptureBase(SampleRate)
	reader := newGatedReader()
	done := make(chan struct{})
	go func() {
		defer close(done)
		base.readLoop(reader, func() error { return errors.New("signal: killed") })
	}()

	<-reader.reading
	close(reader.release)
	<-done

	if err := base.Err(); err == nil {
		t.Fatal("a client that exited with no stop requested was not reported as an error")
	}
}
