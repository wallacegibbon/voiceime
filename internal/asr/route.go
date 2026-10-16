package asr

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Starter is a Transcriber that has to be brought up before it can be used.
//
// A resident server is the reason this exists: it has to be spawned and it has to
// load a model, which takes about a second and must not happen on the audio path.
// The session starts one in the background as dictation begins and stops it when
// dictation ends, so an on-demand process's lifetime is the session's rather than
// the machine's.
type Starter interface {
	// Start brings the route up. It returns immediately, and must be safe to call
	// once.
	Start(ctx context.Context)
	// Stop releases it. It runs once, when the session ends.
	Stop()
}

// Stopper is a Transcriber that owns something to release, such as a process it
// spawned. Routed calls it when the session ends, which is how a preferred route's
// lifetime is bounded without the session knowing what the route is.
type Stopper interface {
	Stop()
}

// Routed prefers one repair route and falls back to another.
//
// It exists for a route that is worth waiting for but cannot be relied on: a
// server that saves seconds per utterance when it is up, and that must never cost
// the user their words when it is not. So a repair goes to the preferred route
// once it is ready, and to the fallback while it is still being brought up, if it
// never arrives, and whenever it fails.
//
// The session owns the fallback (it is the only place that knows the engine
// settings both routes have to share) and fills in Fallback before Start.
type Routed struct {
	// BringUp prepares the preferred route and returns it. It runs once, in the
	// background, when the session starts.
	BringUp func(ctx context.Context) (Transcriber, error)
	// Teardown releases whatever BringUp created. It runs once, when the session
	// ends; nil means there is nothing to release.
	Teardown func()
	// Fallback decodes a repair until the preferred route is ready and whenever it
	// fails. Nil means a failure is a failure, with nothing to fall back to.
	Fallback Transcriber
	// Wait bounds how long a repair waits for the preferred route. A repair that
	// waits longer than the utterance is worth is worse than one that falls back.
	Wait time.Duration
	// Report, when set, is told about a preferred route that could not be brought
	// up or that failed a decode. It is called once per session for each kind: a
	// route that fails has failed, and one line per utterance would be noise.
	Report func(error)

	started   atomic.Bool
	stopped   atomic.Bool
	startOnce sync.Once
	initOnce  sync.Once
	stopOnce  sync.Once
	report    sync.Once
	ready     chan struct{}
	primary   Transcriber
}

// Start brings the preferred route up in the background.
//
// Safe to call more than once - a second call does nothing - because the safe
// failure mode matters more than the honest one here: a route is held by a handler
// that serves every activation, and closing a readiness channel twice panics inside
// the dictation path. The session builds one route per session in any case; this is
// the guard for the mistake being made anyway.
func (r *Routed) Start(ctx context.Context) {
	r.startOnce.Do(func() {
		if r.stopped.Load() {
			// Started after being retired: the route it would have brought up is
			// already stopped, so pretending otherwise would send repairs to a
			// server that is gone.
			close(r.channel())
			return
		}
		r.started.Store(true)
		ready := r.channel()
		if r.BringUp == nil {
			close(ready)
			return
		}
		go func() {
			// primary is written before ready closes, which is what makes it safe to
			// read after receiving from it without a lock.
			defer close(ready)
			transcriber, err := r.BringUp(ctx)
			if err != nil {
				r.reportOnce(err)
				return
			}
			if transcriber == nil {
				return
			}
			r.primary = transcriber
		}()
	})
}

// Stop releases the preferred route.
//
// It waits for a bring-up that is still running, because stopping halfway through
// one would leak the very process this exists to own: the bring-up spawns it, and
// only the finished bring-up knows it exists.
func (r *Routed) Stop() {
	r.stopOnce.Do(func() {
		// Retired before the wait below, so a repair that runs while Stop is waiting
		// for a bring-up goes to the fallback rather than to a route being torn down.
		r.stopped.Store(true)
		defer func() {
			if r.Teardown != nil {
				r.Teardown()
			}
		}()
		if !r.started.Load() {
			return
		}
		select {
		case <-r.channel():
		case <-time.After(stopGrace):
		}
		if stopper, ok := r.primary.(Stopper); ok {
			stopper.Stop()
		}
	})
}

// stopGrace bounds how long Stop waits for a bring-up in flight. A model load is
// about a second; this is the outer bound for a machine that is thrashing.
const stopGrace = 10 * time.Second

// Transcribe routes one repair.
func (r *Routed) Transcribe(ctx context.Context, pcm []byte) (string, error) {
	preferred := r.await(ctx)
	if preferred == nil {
		return r.fallback(ctx, pcm, nil)
	}
	text, err := preferred.Transcribe(ctx, pcm)
	if err == nil {
		return text, nil
	}
	r.reportOnce(err)
	return r.fallback(ctx, pcm, err)
}

// await returns the preferred route once it is ready, waiting at most Wait for it.
func (r *Routed) await(ctx context.Context) Transcriber {
	if r.stopped.Load() || !r.started.Load() {
		// Never started, so there is nothing to wait for: a caller that uses a
		// route without a session gets the fallback rather than a stall.
		return nil
	}
	ready := r.channel()
	select {
	case <-ready:
		return r.primary
	default:
	}
	if r.Wait <= 0 {
		return nil
	}
	timer := time.NewTimer(r.Wait)
	defer timer.Stop()
	select {
	case <-ready:
		return r.primary
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return nil
	}
}

func (r *Routed) fallback(ctx context.Context, pcm []byte, preferredErr error) (string, error) {
	if r.Fallback == nil {
		if preferredErr != nil {
			return "", preferredErr
		}
		return "", fmt.Errorf("no repair route is available")
	}
	text, err := r.Fallback.Transcribe(ctx, pcm)
	if err != nil && preferredErr != nil {
		return "", fmt.Errorf("%v; the fallback repair failed too: %w", preferredErr, err)
	}
	return text, err
}

// channel returns the readiness channel, creating it on first use so a Routed
// value needs no constructor.
func (r *Routed) channel() chan struct{} {
	r.initOnce.Do(func() { r.ready = make(chan struct{}) })
	return r.ready
}

func (r *Routed) reportOnce(err error) {
	if r.Report == nil {
		return
	}
	r.report.Do(func() { r.Report(err) })
}

var _ Starter = (*Routed)(nil)
var _ Transcriber = (*Routed)(nil)
var _ Stopper = (*Routed)(nil)
