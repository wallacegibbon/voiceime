package audio

import (
	"errors"
	"io"
	"sync"
	"testing"
)

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
