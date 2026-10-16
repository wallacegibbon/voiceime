package asr

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// stubTranscriber answers with fixed text and counts its calls, so a test can say
// which route served a repair rather than only what came back.
type stubTranscriber struct {
	text  string
	err   error
	mu    sync.Mutex
	calls int
	stop  int
}

func (s *stubTranscriber) Transcribe(context.Context, []byte) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.text, s.err
}

func (s *stubTranscriber) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stop++
}

func (s *stubTranscriber) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *stubTranscriber) stopCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stop
}

// A route that is ready immediately is used immediately: this is the steady state
// of a server that is up, and the fallback must not be touched at all.
func TestRoutedPrefersTheRouteThatIsReady(t *testing.T) {
	preferred := &stubTranscriber{text: "from the server"}
	fallback := &stubTranscriber{text: "from a process"}
	route := &Routed{
		BringUp:  func(context.Context) (Transcriber, error) { return preferred, nil },
		Fallback: fallback,
		Wait:     time.Second,
	}
	route.Start(context.Background())
	defer route.Stop()

	text, err := route.Transcribe(context.Background(), []byte{1, 2, 3})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if text != "from the server" {
		t.Errorf("text = %q, want the preferred route's", text)
	}
	if fallback.callCount() != 0 {
		t.Errorf("the fallback ran %d time(s), want 0: the preferred route was ready",
			fallback.callCount())
	}
}

// Waiting is bounded: a repair that waits longer than the utterance is worth is
// worse than one that falls back. This is what keeps a server that never comes up
// from costing the user their words.
func TestRoutedFallsBackWhenThePreferredRouteNeverArrives(t *testing.T) {
	release := make(chan struct{})
	preferred := &stubTranscriber{text: "from the server"}
	fallback := &stubTranscriber{text: "from a process"}
	route := &Routed{
		BringUp: func(context.Context) (Transcriber, error) {
			<-release
			return preferred, nil
		},
		Fallback: fallback,
		Wait:     50 * time.Millisecond,
	}
	route.Start(context.Background())

	text, err := route.Transcribe(context.Background(), []byte{1})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if text != "from a process" {
		t.Errorf("text = %q, want the fallback's: the preferred route did not arrive in time", text)
	}
	if fallback.callCount() != 1 {
		t.Errorf("fallback ran %d time(s), want 1", fallback.callCount())
	}

	// Let the bring-up finish before stopping: Stop waits for one in flight, by
	// design, because a spawn happens inside it.
	close(release)
	route.Stop()
}

// The other half of the bound: a route that arrives inside it is waited for rather
// than skipped. Without this a slow start would silently disable the route it was
// started early for.
func TestRoutedWaitsForARouteThatArrivesInTime(t *testing.T) {
	preferred := &stubTranscriber{text: "from the server"}
	fallback := &stubTranscriber{text: "from a process"}
	route := &Routed{
		BringUp: func(context.Context) (Transcriber, error) {
			time.Sleep(80 * time.Millisecond)
			return preferred, nil
		},
		Fallback: fallback,
		Wait:     2 * time.Second,
	}
	route.Start(context.Background())
	defer route.Stop()

	text, err := route.Transcribe(context.Background(), []byte{1})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if text != "from the server" {
		t.Errorf("text = %q, want the preferred route's: it arrived inside the wait", text)
	}
	if fallback.callCount() != 0 {
		t.Errorf("fallback ran %d time(s), want 0", fallback.callCount())
	}
}

// A preferred route that fails mid-session must not lose the utterance: the
// fallback is a working decode of the same audio.
func TestRoutedFallsBackWhenThePreferredRouteFails(t *testing.T) {
	preferred := &stubTranscriber{err: errors.New("server repair: 503 Service Unavailable")}
	fallback := &stubTranscriber{text: "from a process"}
	var reported []error
	route := &Routed{
		BringUp:  func(context.Context) (Transcriber, error) { return preferred, nil },
		Fallback: fallback,
		Wait:     time.Second,
		Report:   func(err error) { reported = append(reported, err) },
	}
	route.Start(context.Background())
	defer route.Stop()

	text, err := route.Transcribe(context.Background(), []byte{1})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if text != "from a process" {
		t.Errorf("text = %q, want the fallback's", text)
	}
	if len(reported) != 1 {
		t.Fatalf("reported %d failure(s), want 1", len(reported))
	}
	if !contains(reported[0].Error(), "503") {
		t.Errorf("reported %v, want the server's reason", reported[0])
	}

	// A route that failed has failed: the second repair must not report it again,
	// or a broken server would write one line per utterance.
	if _, err := route.Transcribe(context.Background(), []byte{1}); err != nil {
		t.Fatalf("second Transcribe: %v", err)
	}
	if len(reported) != 1 {
		t.Errorf("reported %d failure(s) after two repairs, want 1", len(reported))
	}
}

// A bring-up that fails is the single most useful thing to report: it is the
// difference between "the server is slow" and "the server could not start", and it
// happens before any repair has run.
func TestRoutedReportsABringUpFailureOnce(t *testing.T) {
	fallback := &stubTranscriber{text: "from a process"}
	var reported []error
	route := &Routed{
		BringUp: func(context.Context) (Transcriber, error) {
			return nil, errors.New("no server answered at http://127.0.0.1:18080")
		},
		Fallback: fallback,
		Wait:     time.Second,
		Report:   func(err error) { reported = append(reported, err) },
	}
	route.Start(context.Background())
	defer route.Stop()

	for i := 0; i < 3; i++ {
		if _, err := route.Transcribe(context.Background(), []byte{1}); err != nil {
			t.Fatalf("Transcribe: %v", err)
		}
	}
	if len(reported) != 1 {
		t.Fatalf("reported %d time(s), want 1", len(reported))
	}
	if !contains(reported[0].Error(), "no server answered") {
		t.Errorf("reported %v, want the bring-up reason", reported[0])
	}
	if fallback.callCount() != 3 {
		t.Errorf("fallback ran %d time(s), want 3", fallback.callCount())
	}
}

// The session owns the route's lifetime, and the route owns the process. Stop has
// to reach a bring-up that is still in flight, or the process it spawns leaks: the
// spawn happens inside BringUp, so nothing else would ever hear about it.
func TestRoutedStopsWhatItBroughtUpEvenIfStopArrivesFirst(t *testing.T) {
	spawned := &stubTranscriber{text: "from the server"}
	route := &Routed{
		BringUp: func(context.Context) (Transcriber, error) {
			time.Sleep(80 * time.Millisecond)
			return spawned, nil
		},
		Fallback: &stubTranscriber{text: "from a process"},
		Wait:     time.Second,
	}
	route.Start(context.Background())
	route.Stop()

	if spawned.stopCount() != 1 {
		t.Errorf("stopped the brought-up route %d time(s), want 1", spawned.stopCount())
	}
}

// A caller that never started the route gets the fallback immediately rather than
// stalling on a bring-up that was never requested.
func TestRoutedWithoutStartUsesTheFallback(t *testing.T) {
	fallback := &stubTranscriber{text: "from a process"}
	route := &Routed{
		BringUp:  func(context.Context) (Transcriber, error) { return &stubTranscriber{text: "x"}, nil },
		Fallback: fallback,
		Wait:     30 * time.Second,
	}
	started := time.Now()
	text, err := route.Transcribe(context.Background(), []byte{1})
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if text != "from a process" {
		t.Errorf("text = %q, want the fallback's", text)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Errorf("Transcribe took %v without a Start, want no wait", elapsed)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// A route is held by a handler that serves every activation, so Start has to be
// safe to call twice. The failure it used to have was a panic - close of closed
// channel - in the dictation path, on the second activation of an engine that had
// already dictated once.
func TestRoutedStartIsSafeToCallTwice(t *testing.T) {
	bringUps := 0
	preferred := &stubTranscriber{text: "from the server"}
	route := &Routed{
		BringUp: func(context.Context) (Transcriber, error) {
			bringUps++
			return preferred, nil
		},
		Fallback: &stubTranscriber{text: "from a process"},
		Wait:     time.Second,
	}
	route.Start(context.Background())
	route.Stop()

	// The second activation. It must not panic, and it must not pretend the route
	// that was stopped is live again.
	route.Start(context.Background())
	time.Sleep(100 * time.Millisecond)
	text, err := route.Transcribe(context.Background(), []byte{1})
	if err != nil {
		t.Fatalf("Transcribe after a second Start: %v", err)
	}
	if text != "from a process" {
		t.Errorf("text = %q, want the fallback's: the brought-up route is gone", text)
	}
	if bringUps != 1 {
		t.Errorf("bring-up ran %d time(s), want 1", bringUps)
	}
}
