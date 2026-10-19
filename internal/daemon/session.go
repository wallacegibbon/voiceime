// Package daemon owns the dictation session: capture, endpointing, streaming
// recognition, repair, and the decision of when text becomes final.
//
// It exists because two front ends need exactly the same behaviour: `voiceime
// stream`, which prints what it sees for measurement, and the IBus engine, which
// feeds it into an application. Keeping one implementation means a change to the
// repair policy or the endpoint rules cannot apply to only one of them.
package daemon

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wallacegibbon/voiceime/internal/asr"
	"github.com/wallacegibbon/voiceime/internal/audio"
	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/server"
)

// Sink is where text goes. It is deliberately the same shape as ibus.Control, so
// the engine passes its own control object straight in, while `stream` passes a
// printer.
type Sink interface {
	// Commit delivers final text. It is called once per utterance boundary, with
	// only the text that has not been delivered before.
	Commit(text string) error
	// Preedit shows text that is still provisional and may change.
	Preedit(text string, cursor int, visible bool) error
}

// Observer receives everything the session learns, for logging and measurement.
// All callbacks are optional.
//
// They are invoked from three goroutines (recognition events, the capture loop and
// the repair worker), so an implementation must be safe for concurrent use. Text
// delivery is already serialised; these are notifications and are not.
type Observer struct {
	Ready     func(load time.Duration)
	Partial   func(text string, fedSeconds float64)
	Segment   func(startSeconds, endSeconds, speechSeconds float64)
	Repair    func(text string, took time.Duration, speechSeconds float64)
	Skipped   func(reason string)
	Committed func(text string)
	Error     func(error)
}

// Options describes one dictation session.
type Options struct {
	// Config supplies the server address and everything the session reads per run. There
	// is no process to start: the service holds the models, so a session is a connection.
	Config   *config.Config
	Sink     Sink
	Observer Observer
	// NewCapture opens the audio source. A factory rather than an instance
	// because a session consumes its source: a file replay is exhausted after one
	// run, and a microphone should be reopened per activation rather than shared
	// across sessions.
	NewCapture func() (audio.Capture, error)
	// Transcriber overrides the offline decode the repair pass runs, for tests and
	// for a caller that wants a different reader. Nil uses the server.
	Transcriber asr.Transcriber
	// MinRepairSeconds is the shortest segment worth an offline re-decode. A
	// repair costs a process start and a model load, so a cough is not worth one.
	MinRepairSeconds float64
}

// Result summarises a finished session.
type Result struct {
	StreamText string
	FinalText  string
	Partials   int
	AudioFed   time.Duration
	LoadTime   time.Duration
	Metrics    map[string]string
}

// Session runs one dictation session. Run blocks until the context ends or the
// capture stream finishes.
func (o Options) Run(ctx context.Context) (Result, error) {
	cfg := o.Config
	if o.Config == nil {
		return Result{}, fmt.Errorf("session needs a configuration")
	}
	if o.Sink == nil {
		return Result{}, fmt.Errorf("session needs a sink to deliver text to")
	}
	if o.MinRepairSeconds == 0 {
		o.MinRepairSeconds = 0.4
	}
	notify := o.Observer
	report := func(fn func(error), err error) {
		if fn != nil && err != nil {
			fn(err)
		}
	}

	capture := audio.Capture(nil)
	if o.NewCapture != nil {
		var err error
		capture, err = o.NewCapture()
		if err != nil {
			return Result{}, err
		}
	} else {
		var err error
		capture, err = audio.NewMicrophone(cfg)
		if err != nil {
			return Result{}, err
		}
	}

	segmenter := audio.NewSegmenter(audio.SegmenterConfig{
		SilenceMS:           cfg.EndpointSilenceMS,
		MinSpeechMS:         cfg.EndpointMinSpeechMS,
		Threshold:           float64(cfg.SilenceRMSThreshold),
		MaxUtteranceSeconds: cfg.MaxUtteranceSeconds,
		Adaptive:            true,
	})
	// The buffer holds enough audio for one repair: the longest utterance plus one
	// streaming window, since a segment can end a window late.
	buffer := audio.NewRollingBuffer(cfg.MaxUtteranceSeconds + cfg.ChunkSeconds)

	pipeline := &session{
		options:    o,
		transcript: asr.NewTranscript(),
		sink:       o.Sink,
		notify:     notify,
		buffer:     buffer,
		segmenter:  segmenter,
		padSamples: int64(cfg.EndpointSilenceMS) * int64(audio.SampleRate) / 2000,
	}
	// The repair route is brought up before the streaming model is loaded, not after.
	// A route that has to load its own model starts doing so in the background, and
	// it is the first sentence that waits for it - so the ~330ms the streaming load
	// used to hold it back is time the user spends waiting after they stop talking.
	//
	// Decoding deliberately outlives the session's context. Stopping dictation is
	// itself an endpoint: the utterance in progress is flushed and queued for repair
	// *after* ctx is cancelled, and a decode that inherits the cancellation never
	// leaves the process - measured on the hotkey-release path: 0 repair requests for
	// the sentence that was just spoken, so the unrepaired draft was delivered. What
	// bounds the wait instead is repairDrainBudget, and the transcriber's own timeout.
	decodeCtx, stopDecoding := context.WithCancel(context.WithoutCancel(ctx))
	defer stopDecoding()
	pipeline.startRepairWorker(decodeCtx, o)

	engine, err := asr.OpenStream(ctx, asr.StreamConfig{
		BaseURL:  cfg.ASRServerURL,
		Model:    server.DefaultStreamID,
		Language: cfg.Language,
	})
	if err != nil {
		report(notify.Error, pipeline.finishRepairs(repairDrainBudget))
		return Result{}, err
	}
	defer engine.Close()
	if notify.Ready != nil {
		notify.Ready(engine.LoadTime())
	}

	// Recognition events run on their own goroutine so the capture loop is never
	// blocked by publishing text.
	engineDone := make(chan struct{})
	go func() {
		defer close(engineDone)
		var previousPartial int64
		for event := range engine.Events() {
			switch event.Kind {
			case asr.EventPartial:
				pipeline.partials.Add(1)
				end := int64(event.FedSeconds * float64(audio.SampleRate))
				if end < previousPartial {
					end = previousPartial
				}
				start := previousPartial
				previousPartial = end
				if !pipeline.acceptPartial(event.Text, start, end, notify.Skipped) {
					continue
				}
				accepted := pipeline.transcript.AppendStream(event.Text, start, end)
				if !accepted && notify.Skipped != nil {
					// The window started inside text that was already delivered.
					// Saying so is better than looking like a lost partial.
					notify.Skipped("a window overlapping already-delivered text was dropped")
				}
				if notify.Partial != nil {
					// Only text that reached the transcript: a listener printing every
					// decode would show the silence hallucination as if it were a draft.
					notify.Partial(event.Text, event.FedSeconds)
				}
				report(notify.Error, pipeline.publishPreedit())
			case asr.EventSnapshot:
				pipeline.transcript.SetSnapshot(event.Text, 0, engine.FedSamples())
				previousPartial = engine.FedSamples()
				if notify.Partial != nil {
					notify.Partial(event.Text, event.FedSeconds)
				}
				report(notify.Error, pipeline.publishPreedit())
			}
		}
	}()

	if err := capture.Start(); err != nil {
		// The worker goroutines are already running, and this process is long
		// lived: a failed activation (a busy device) must not leave a queue and two
		// goroutines behind per attempt - measured at two goroutines per failure
		// before this cleanup existed.
		report(notify.Error, pipeline.finishRepairs(repairDrainBudget))
		return Result{}, err
	}
	defer capture.Stop()

	for chunk := range capture.Chunks() {
		if err := engine.Write(chunk.Data); err != nil {
			// A cancelled session closes the socket under the writer - that is how a
			// Write waiting on a slow server is released (see OpenStream's watcher) - so
			// this failure is the stop working, not a failure. Reporting it put "write
			// audio to the streaming server: use of closed network connection" on stderr
			// for every Ctrl-C and in the engine's trace for every hotkey release, which
			// is the shape of a bug report for a feature that worked.
			if ctx.Err() == nil {
				report(notify.Error, err)
			}
			break
		}
		// A dropped block is the one way audio the user spoke never reaches the
		// recogniser at all, and the capture layer can only count it. Saying so is
		// the difference between "the model misheard me" and "the machine could not
		// keep up", and the transcript has a gap at that position either way.
		if chunk.DroppedSamples > 0 && notify.Skipped != nil {
			notify.Skipped(fmt.Sprintf(
				"capture dropped %.2fs of audio because the engine was behind; that speech was not transcribed",
				float64(chunk.DroppedSamples)/float64(audio.SampleRate)))
		}
		pipeline.audioFed.Store(int64(time.Duration(engine.FedSamples()) * time.Second / audio.SampleRate))
		buffer.Append(chunk.Data, chunk.StartSample)

		for _, event := range segmenter.Push(chunk.Data, chunk.StartSample) {
			if event.Kind != audio.SpeechEnd {
				continue
			}
			pipeline.onSegmentEnd(event)
		}
		if ctx.Err() != nil {
			break
		}
	}
	capture.Stop()

	// A session that stops is itself an endpoint: whatever was being said is now
	// finished, so it is committed and repaired like any other utterance.
	for _, event := range segmenter.Flush() {
		pipeline.onSegmentEnd(event)
	}

	result, engineErr := engine.Close()
	<-engineDone
	if engineErr == nil {
		// Result carries the same failure for a caller that reads what the session
		// produced rather than the error Close returned. The two must not diverge:
		// a session that lost its server mid-transcript is a failure, not a short
		// dictation.
		engineErr = result.ProcessErr
	}

	// Every in-flight repair must land before the final commit, or text would be
	// delivered twice and out of order. The wait is bounded: the decodes no longer
	// inherit this session's cancellation (see startRepairWorker's caller), so a
	// wedged server would otherwise hold up the stop for as long as its repair
	// timeout.
	report(notify.Error, pipeline.finishRepairs(repairDrainBudget))

	if notify.Skipped != nil && capture.Err() != nil {
		notify.Skipped(capture.Err().Error())
	}
	// Goes through commitProvisional so the "already delivered" bookkeeping
	// applies here too: the last utterance must not be sent twice.
	//
	// But only if the endpointer heard speech at some point. Streaming ASR
	// produces plausible text for almost any input - measured: twenty seconds of
	// an empty room committed "被。。。。。。嗯。" - so committing the provisional text
	// of a session that never contained speech types hallucinated words into the
	// application for no reason. A user whose speech is too quiet to cross the
	// threshold sees nothing rather than nonsense; `voiceime doctor` and
	// `stream --events` show which of the two happened.
	//
	// And not at all if the stream failed. A failed session is not an utterance the
	// user finished: its provisional text is a draft that no offline decode ever
	// reached, and the audio behind it may be truncated, so committing it would type
	// words the user has no way to know were guessed. Utterances that did close are
	// unaffected - each was decoded and committed by the repair worker before this
	// point - so what is withheld here is exactly the text the failure stranded.
	//
	// Discarding is not the same as discarding *silently*, though: that text was
	// visible at the user's caret as preedit, so how much of it is being dropped is
	// reported. The count is what lets "the detector never heard me" (this message,
	// with a number) be told apart from "the session was lost" (an error), and it
	// is the actionable direction - it is the size of what the room measurement was
	// wrong about.
	if engineErr != nil {
		if withheld := pipeline.transcript.ProvisionalText(); withheld != "" && notify.Skipped != nil {
			notify.Skipped(fmt.Sprintf(
				"streaming session failed; withheld %d characters it never decoded offline (%v)",
				len([]rune(withheld)), engineErr))
		}
	} else if pipeline.segments.Load() > 0 {
		pipeline.commitProvisional()
	} else if notify.Skipped != nil {
		if withheld := pipeline.transcript.ProvisionalText(); withheld != "" {
			notify.Skipped(fmt.Sprintf(
				"no speech detected; withheld %d characters the recogniser produced for audio the detector never called speech (check silence_rms_threshold and endpoint_min_speech_ms)",
				len([]rune(withheld))))
		} else {
			notify.Skipped("no speech detected; nothing committed")
		}
	}

	summary := Result{
		StreamText: result.Text,
		FinalText:  pipeline.transcript.Text(),
		Partials:   int(pipeline.partials.Load()),
		AudioFed:   time.Duration(pipeline.audioFed.Load()),
		LoadTime:   result.LoadTime,
		Metrics:    result.Metrics,
	}
	if engineErr != nil {
		return summary, engineErr
	}
	return summary, ctx.Err()
}

// session is the mutable state of one run, kept off the Options value so a caller
// can reuse its options.
type session struct {
	options    Options
	transcript *asr.Transcript
	sink       Sink
	notify     Observer

	partials atomic.Int64
	audioFed atomic.Int64
	// segments counts utterances the endpointer accepted. A session that ends
	// without one contains no speech the detector could find, and committing its
	// provisional text would deliver the recogniser's hallucination.
	segments atomic.Int64

	repairQueue chan repairJob
	repairDone  sync.WaitGroup
	// repairClose makes closing the queue idempotent: the normal shutdown path and
	// an early return both end the session, and closing twice would panic.
	repairClose sync.Once

	// buffer is the recent audio an offline decode re-reads.
	buffer *audio.RollingBuffer
	// padSamples widens an utterance's decode range on each side, by half the
	// silence the detector needs to close one. The detector stops on the last loud
	// frame, so it can clip a quiet tail; half the hangover covers that and still
	// stops well short of the next utterance. See resolveSegment.
	padSamples int64

	// publishMu serialises sink calls. Three goroutines publish (recognition
	// events, segment boundaries and repair results), and the order of "clear the
	// composition, then commit" matters to the application receiving it.
	publishMu sync.Mutex

	// segmenter is the endpointer the capture loop drives. The recognition goroutine asks it
	// which audio it heard speech in, to decide whether decoded text is speech or the
	// recogniser's answer to silence (see acceptPartial). Segmenter.Speech is safe to call
	// from another goroutine.
	segmenter *audio.Segmenter

	// droppedSilence keeps the "this window was silence" report to once per pause rather
	// than once per window; it is touched only by the recognition goroutine.
	droppedSilence bool
}

type repairJob struct {
	pcm         []byte
	startSample int64
	endSample   int64
}

func (s *session) startRepairWorker(ctx context.Context, options Options) {
	if !options.Config.RefineOnSegmentEnd {
		return
	}
	// ctx is the decoding context, not the session's: it outlives a stop so that the
	// utterance the stop itself closed is still decoded offline (see the caller).
	// One route, not two. The server is a service on the machine, so there is nothing
	// to bring up and nothing to fall back to: if it is not running the session never
	// starts (the streaming side fails first, with a message naming the unit), and if
	// it dies mid-session the repair reports that rather than quietly decoding the
	// same audio somewhere else at several times the cost.
	transcriber := options.Transcriber
	if transcriber == nil {
		transcriber = asr.ServerTranscriber{
			BaseURL:  options.Config.ASRServerURL,
			Model:    server.DefaultID,
			Language: options.Config.Language,
			Timeout:  time.Duration(options.Config.ASRServerTimeoutMS) * time.Millisecond,
		}
	}

	// One worker, buffered: repairs run off the audio path but stay ordered, so the
	// transcript cannot be reassembled out of order. It decodes and publishes in the same
	// goroutine - publishing is a few sink calls, and a second goroutine to overlap them
	// with the next decode bought nothing while the decodes are what take the time.
	s.repairQueue = make(chan repairJob, 8)
	s.repairDone.Add(1)
	go func() {
		defer s.repairDone.Done()
		for job := range s.repairQueue {
			started := time.Now()
			text, err := transcriber.Transcribe(ctx, job.pcm)
			took := time.Since(started)
			if err != nil {
				if s.notify.Error != nil {
					s.notify.Error(err)
				}
				// A failed repair must still release the utterance, or the text would sit
				// in provisional limbo forever.
				s.commitProvisional()
				continue
			}
			s.transcript.Place(job.startSample, job.endSample, text)
			if s.notify.Repair != nil {
				s.notify.Repair(text, took, float64(job.endSample-job.startSample)/audio.SampleRate)
			}
			if err := s.publishPreedit(); err != nil && s.notify.Error != nil {
				s.notify.Error(err)
			}
			// The repair is what makes the text worth committing: delivering before it
			// lands would deliver the window-boundary damage.
			s.commitProvisional()
		}
	}()
}

func (s *session) closeRepairs() {
	if s.repairQueue != nil {
		s.repairClose.Do(func() { close(s.repairQueue) })
	}
}

// repairDrainBudget is how long a stop waits for the decodes already queued.
//
// It exists because a repair no longer inherits the session's cancellation: without a
// bound, a stop would last as long as the transcriber's own timeout (30s by default)
// on a server that stopped answering - and the engine's Disable is a D-Bus call that
// ibus-daemon is waiting on. Measured repairs run 0.5-0.8s, so this is an order of
// magnitude of headroom.
const repairDrainBudget = 5 * time.Second

// finishRepairs ends the queue and waits for the decodes already in it, giving up
// after budget. The error, when there is one, is what the user is owed: it says a
// sentence was delivered from the draft because the decode did not land in time.
func (s *session) finishRepairs(budget time.Duration) error {
	s.closeRepairs()
	if s.repairQueue == nil {
		return nil
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.repairDone.Wait()
	}()
	select {
	case <-done:
		return nil
	case <-time.After(budget):
		return fmt.Errorf("gave up waiting for the last repair after %v: its sentence was committed from the draft", budget)
	}
}

// speechCovered reports whether [start, end) of the audio stream contains speech the
// detector heard. It is deliberately unpadded.
//
// The repair decode pads by half the hangover, because it has to recover a quiet tail that
// the detector's last loud sample clips. A streaming window cannot do that: its text cannot
// be split at the speech boundary, so padding the test admits windows whose audio is
// entirely room - measured on this fixture, a 400ms pad let two windows of pure silence in,
// and their "。" was committed with the sentence they followed. The draft may miss a quiet
// tail; the repair is where the tail is read.
func (s *session) speechCovered(start, end int64) bool {
	heardFrom, heardTo := s.segmenter.Speech()
	if heardTo == 0 {
		// Nothing has been called speech in this session yet.
		return false
	}
	return start < heardTo && end > heardFrom
}

// acceptPartial decides whether streaming text belongs in the transcript.
//
// A live model decodes every window, including windows of silence, and answers with
// something - measured on the reference machine: one "。" per three-second window in a quiet
// room. Keeping that as provisional text is what made the composition hang: the endpointer
// had correctly decided nobody was speaking, so no boundary would ever finalise it, and the
// user watched "。。。" sit under the caret until the *next* sentence's commit flushed it into
// their application (01:37 in the engine log). Text for audio the detector never called
// speech is the recogniser's guess, and the end-of-session rule already says so; this is the
// same rule applied while the session runs.
func (s *session) acceptPartial(text string, start, end int64, report func(string)) bool {
	if !s.speechCovered(start, end) {
		if report != nil && !s.droppedSilence {
			s.droppedSilence = true
			report("dropped streaming text for audio the detector never called speech")
		}
		return false
	}
	s.droppedSilence = false
	return true
}

// onSegmentEnd closes an utterance: queue its offline decode, or commit straight
// away when repair is off.
func (s *session) onSegmentEnd(event audio.SegmentEvent) {
	s.segments.Add(1)
	if s.notify.Segment != nil {
		s.notify.Segment(float64(event.StartSample)/audio.SampleRate,
			float64(event.EndSample)/audio.SampleRate, event.DurationSeconds())
	}
	s.resolveSegment(event)
}

// resolveSegment queues the offline decode of a finished utterance, or commits it
// straight away when repair is off.
//
// The decode is what makes the text final - the streaming layer only ever produces
// a draft - so whether streaming text happens to cover this utterance changes
// nothing about what is delivered. It does change *what range to decode*, because a
// decode that stands in for streaming text must re-read everything that text
// covered:
//
//   - Text covers the utterance: decode the union of its range and every
//     overlapping chunk. A chunk is decoded as one unit and reaches past the
//     range, so decoding only the range would delete that text without re-reading
//     it. See Transcript.RepairSpan.
//
//   - Nothing covers it: the decode is the *only* reading of this audio, so decode
//     the utterance itself, widened by padSamples. A short utterance always lands
//     here - it closes about endpoint_silence_ms after the speech ends, while the
//     recogniser emits a partial only every chunk_seconds - and treating that as
//     "nothing to correct" is what used to commit empty and leave the text with no
//     boundary left to release it.
func (s *session) resolveSegment(event audio.SegmentEvent) {
	if s.repairQueue == nil || event.DurationSeconds() < s.options.MinRepairSeconds {
		s.commitProvisional()
		return
	}
	startSample, endSample, found := s.transcript.RepairSpan(event.StartSample, event.EndSample)
	padded := false
	if !found {
		// Widen by half the silence the detector needed to close the utterance:
		// it ends on the last loud frame, so its own range can clip a quiet tail,
		// and half the hangover covers that without reaching the next utterance -
		// the pause before one is at least the whole hangover. See padSamples.
		startSample = event.StartSample - s.padSamples
		if startSample < 0 {
			startSample = 0
		}
		endSample = event.EndSample + s.padSamples
		padded = true
	}
	pcm, ok := s.buffer.Slice(startSample, endSample)
	if !ok && padded {
		// The widening can reach past the end of the audio that exists. A session
		// that stops while the user is still speaking has no trailing silence left
		// to read, and the segmenter's flush closes the last utterance on the last
		// loud frame - so the padded range overruns the recording by up to
		// padSamples on exactly the sentence a push-to-talk stop ends on. Refusing
		// to decode there delivered the streaming draft instead, which is the
		// window-boundary damage the repair exists to fix.
		//
		// Clipping is safe here and only here: with no provisional text covering the
		// utterance, the decode replaces nothing, so a shorter range means a smaller
		// reading of the same speech rather than text deleted without being re-read.
		// The utterance itself still has to be present - if it is not, the repair
		// really has fallen out of the buffer and skipping is the honest outcome.
		clipped, clippedStart, clippedEnd, overlap := s.buffer.SliceClamped(startSample, endSample)
		if overlap && clippedStart <= event.StartSample && event.EndSample <= clippedEnd {
			pcm, ok = clipped, true
			startSample, endSample = clippedStart, clippedEnd
		}
	}
	if !ok {
		// The segment fell out of the rolling buffer (a very long utterance).
		// Committing the stream text is better than decoding a truncated segment.
		if s.notify.Skipped != nil {
			s.notify.Skipped("segment too long to repair; committing stream text")
		}
		s.commitProvisional()
		return
	}
	select {
	case s.repairQueue <- repairJob{pcm: pcm, startSample: startSample, endSample: endSample}:
	default:
		if s.notify.Skipped != nil {
			s.notify.Skipped("repair queue full; committing stream text")
		}
		s.commitProvisional()
	}
}

// publishPreedit shows the provisional text as an in-progress composition.
func (s *session) publishPreedit() error {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	return s.publishPreeditLocked()
}

func (s *session) publishPreeditLocked() error {
	provisional := s.transcript.ProvisionalText()
	if provisional == "" {
		return s.sink.Preedit("", 0, false)
	}
	return s.sink.Preedit(provisional, len([]rune(provisional)), true)
}

// commitProvisional delivers the text that has become final and clears the
// composition.
//
// The transcript's own bookkeeping decides what is new: CommitProvisional returns
// exactly the text that was provisional until now, and marks it committed, so
// each chunk is released once. Tracking a second "delivered" counter here was a
// bug - it compared the length of one delta against another, and silently dropped
// any sentence that happened to have the same length as its predecessor.
func (s *session) commitProvisional() {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	s.commitProvisionalLocked()
}

func (s *session) commitProvisionalLocked() {
	pending := s.transcript.CommitProvisional()
	// Clear the composition first: an application that kept showing the text after
	// inserting it would display the utterance twice.
	if err := s.sink.Preedit("", 0, false); err != nil && s.notify.Error != nil {
		s.notify.Error(err)
	}
	if pending == "" {
		return
	}
	if err := s.sink.Commit(pending); err != nil {
		if s.notify.Error != nil {
			s.notify.Error(err)
		}
		return
	}
	if s.notify.Committed != nil {
		s.notify.Committed(pending)
	}
}
