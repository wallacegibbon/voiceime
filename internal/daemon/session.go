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
	Config      *config.Config
	Binary      string // audiocpp_cli
	Model       string // streaming model
	RepairModel string // offline model; empty means reuse the streaming one
	Sink        Sink
	Observer    Observer
	// NewCapture opens the audio source. A factory rather than an instance
	// because a session consumes its source: a file replay is exhausted after one
	// run, and a microphone should be reopened per activation rather than shared
	// across sessions.
	NewCapture func() (audio.Capture, error)
	// Transcriber overrides the offline decode the repair pass runs. Nil builds
	// the CLI Repairer from the fields above; a resident server passes its own
	// implementation. The session treats them identically: both take one
	// utterance's PCM and return its text.
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

	engineConfig := asr.EngineConfig{
		Binary:         o.Binary,
		WorkDir:        cfg.AudioCPPDirPath(),
		Model:          o.Model,
		Backend:        cfg.Backend,
		Threads:        cfg.Threads,
		Language:       cfg.Language,
		ChunkSeconds:   cfg.ChunkSeconds,
		SessionOptions: cfg.SessionOptions,
		RequestOptions: cfg.RequestOptions,
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

	engine, err := asr.Start(ctx, engineConfig)
	if err != nil {
		return Result{}, err
	}
	defer engine.Close()
	if notify.Ready != nil {
		notify.Ready(engine.LoadTime())
	}

	pipeline := &session{
		options:    o,
		transcript: asr.NewTranscript(),
		sink:       o.Sink,
		notify:     notify,
		buffer:     buffer,
		padSamples: int64(cfg.EndpointSilenceMS) * int64(audio.SampleRate) / 2000,
	}
	pipeline.startRepairWorker(ctx, o, engineConfig)

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
				accepted := pipeline.transcript.AppendStream(event.Text, previousPartial, end)
				previousPartial = end
				if !accepted && notify.Skipped != nil {
					// The window started inside text that was already delivered.
					// Saying so is better than looking like a lost partial.
					notify.Skipped("a window overlapping already-delivered text was dropped")
				}
				if notify.Partial != nil {
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
		return Result{}, err
	}
	defer capture.Stop()

	for chunk := range capture.Chunks() {
		if err := engine.Write(chunk.Data); err != nil {
			report(notify.Error, err)
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

	// Every in-flight repair must land before the final commit, or text would be
	// delivered twice and out of order.
	pipeline.closeRepairs()
	pipeline.waitForRepairs()

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
	// Discarding is not the same as discarding *silently*, though: that text was
	// visible at the user's caret as preedit, so how much of it is being dropped is
	// reported. The count is what lets "the detector never heard me" (this message,
	// with a number) be told apart from "the session was lost" (no message), and it
	// is the actionable direction - it is the size of what the room measurement was
	// wrong about.
	if pipeline.segments.Load() > 0 {
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

	repairQueue   chan repairJob
	repairResults chan repairResult
	repairDone    sync.WaitGroup

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
}

type repairJob struct {
	pcm         []byte
	startSample int64
	endSample   int64
}

type repairResult struct {
	job  repairJob
	text string
	took time.Duration
	err  error
}

func (s *session) startRepairWorker(ctx context.Context, options Options, engineConfig asr.EngineConfig) {
	if !options.Config.RefineOnSegmentEnd {
		return
	}
	transcriber := options.Transcriber
	if transcriber == nil {
		model := options.RepairModel
		if model == "" {
			model = options.Model
		}
		transcriber = asr.Repairer{
			Binary:         options.Binary,
			WorkDir:        options.Config.AudioCPPDirPath(),
			Model:          model,
			Backend:        engineConfig.Backend,
			Threads:        options.Config.Threads,
			Language:       options.Config.Language,
			RequestOptions: engineConfig.RequestOptions,
			SessionOptions: engineConfig.SessionOptions,
		}
	}

	// One worker, buffered: repairs run off the audio path but stay ordered, so
	// the transcript cannot be reassembled out of order.
	s.repairQueue = make(chan repairJob, 8)
	s.repairResults = make(chan repairResult, 8)
	s.repairDone.Add(1)
	go func() {
		defer s.repairDone.Done()
		// The worker owns closing the results channel, so a consumer's range ends
		// when the last repair has been published rather than when the queue empties.
		defer close(s.repairResults)
		for job := range s.repairQueue {
			started := time.Now()
			text, err := transcriber.Transcribe(ctx, job.pcm)
			s.repairResults <- repairResult{job: job, text: text, took: time.Since(started), err: err}
		}
	}()
	s.repairDone.Add(1)
	go func() {
		defer s.repairDone.Done()
		for result := range s.repairResults {
			if result.err != nil {
				if s.notify.Error != nil {
					s.notify.Error(result.err)
				}
				// A failed repair must still release the utterance, or the text
				// would sit in provisional limbo forever.
				s.commitProvisional()
				continue
			}
			s.transcript.Place(result.job.startSample, result.job.endSample, result.text)
			if s.notify.Repair != nil {
				s.notify.Repair(result.text, result.took,
					float64(result.job.endSample-result.job.startSample)/audio.SampleRate)
			}
			if err := s.publishPreedit(); err != nil && s.notify.Error != nil {
				s.notify.Error(err)
			}
			// The repair is what makes the text worth committing: delivering
			// before it lands would deliver the window-boundary damage.
			s.commitProvisional()
		}
	}()
}

func (s *session) closeRepairs() {
	if s.repairQueue != nil {
		close(s.repairQueue)
	}
}

func (s *session) waitForRepairs() { s.repairDone.Wait() }

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
	}
	pcm, ok := s.buffer.Slice(startSample, endSample)
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
