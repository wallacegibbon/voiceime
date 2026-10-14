package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wallacegibbon/voiceime/internal/asr"
	"github.com/wallacegibbon/voiceime/internal/audio"
	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/models"
)

// cmdStream is the observable end of the pipeline: it captures audio, streams it
// through the engine and re-decodes finished segments, printing everything it
// learns.
//
// It exists because tuning this system is empirical. Window length, endpoint
// thresholds and the value of the repair pass can only be judged from numbers,
// and this is where those numbers come from - the same code path the daemon runs,
// with the results made visible instead of committed to an application.
func cmdStream() *Command {
	var (
		filePath  string
		seconds   float64
		realtime  bool
		chunk     float64
		refine    bool
		refineMod string
		events    bool
		quiet     bool
		threads   int
		model     string
	)
	flags := func(set *flag.FlagSet) {
		set.StringVar(&filePath, "file", "", "replay a WAV file instead of the microphone")
		set.BoolVar(&realtime, "realtime", true, "with --file, pace the replay at real time")
		set.Float64Var(&seconds, "seconds", 0, "stop after this many seconds of capture (0 = until end of input)")
		set.Float64Var(&chunk, "chunk", 0, "streaming window in seconds (default: from config)")
		set.BoolVar(&refine, "refine", true, "re-transcribe finished segments offline")
		set.StringVar(&refineMod, "refine-model", "", "model for the repair pass (default: from config)")
		set.BoolVar(&events, "events", false, "print one JSON object per event instead of text")
		set.BoolVar(&quiet, "quiet", false, "print only committed text")
		set.IntVar(&threads, "threads", 0, "worker threads (default: from config)")
		set.StringVar(&model, "model", "", "streaming model path (default: from config)")
	}
	return &Command{
		Name:    "stream",
		Summary: "Capture audio and print live transcription with timing and repair",
		Usage:   "voiceime stream [--file F] [--mic] [--seconds N] [--events]",
		Flags:   flags,
		Run: func(ctx *Context, args []string) error {
			return runStream(ctx, streamOptions{
				filePath:  filePath,
				realtime:  realtime,
				seconds:   seconds,
				chunk:     chunk,
				refine:    refine,
				refineMod: refineMod,
				events:    events,
				quiet:     quiet,
				threads:   threads,
				model:     model,
			})
		},
	}
}

type streamOptions struct {
	filePath  string
	realtime  bool
	seconds   float64
	chunk     float64
	refine    bool
	refineMod string
	events    bool
	quiet     bool
	threads   int
	model     string
}

// emitter serialises output. It is the single writer, so JSON lines cannot
// interleave when repairs finish while audio is still arriving.
type emitter struct {
	ctx   *Context
	json  bool
	quiet bool
	mu    sync.Mutex
}

func (e *emitter) write(kind string, payload map[string]any) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.json {
		payload["kind"] = kind
		encoder := json.NewEncoder(e.ctx.Stdout)
		_ = encoder.Encode(payload)
		return
	}
	switch kind {
	case "ready":
		e.ctx.Print("engine ready in %v (%s)", payload["load"], payload["model"])
	case "partial":
		if e.quiet {
			return
		}
		e.ctx.Print("[+%5.2fs] %s", payload["fed_seconds"], payload["text"])
	case "repair":
		e.ctx.Print("[repair %.2fs] %s", payload["took_seconds"], payload["text"])
	case "segment":
		e.ctx.Print("[+%5.2fs] segment %.2fs-%.2fs (%.2fs speech)", payload["fed_seconds"],
			payload["start_seconds"], payload["end_seconds"], payload["speech_seconds"])
	case "vad":
		if e.quiet {
			return
		}
		e.ctx.Print("[vad] %s at %.2fs", payload["event"], payload["sample_seconds"])
	case "summary":
		for _, line := range payload["lines"].([]string) {
			e.ctx.Print("%s", line)
		}
	}
}

func runStream(ctx *Context, options streamOptions) error {
	if options.threads == 0 {
		options.threads = ctx.Config.Threads
	}

	binary, err := ctx.Config.FindASRBinary()
	if err != nil {
		return PreconditionError("voiceime config set audio_cpp_dir /path/to/audio.cpp", "%v", err)
	}
	target := options.model
	if target == "" {
		target = resolveModel(ctx, false)
	}
	if target == "" {
		return PreconditionError("voiceime models install "+models.RecommendedPackages()[0].ID,
			"no streaming model available")
	}

	engineConfig := newEngineConfig(ctx, target, binary)
	if options.chunk > 0 {
		engineConfig.ChunkSeconds = options.chunk
	}

	var capture audio.Capture
	if options.filePath != "" {
		capture, err = audio.NewFileReplay(config.Expand(options.filePath), options.realtime)
		if err != nil {
			return FailureError("%v", err)
		}
	} else {
		capture, err = audio.NewMicrophone(ctx.Config)
		if err != nil {
			return PreconditionError("voiceime devices", "%v", err)
		}
	}

	segmenter := audio.NewSegmenter(audio.SegmenterConfig{
		SilenceMS:           ctx.Config.EndpointSilenceMS,
		MinSpeechMS:         ctx.Config.EndpointMinSpeechMS,
		Threshold:           float64(ctx.Config.SilenceRMSThreshold),
		MaxUtteranceSeconds: ctx.Config.MaxUtteranceSeconds,
		Adaptive:            true,
	})
	buffer := audio.NewRollingBuffer(ctx.Config.MaxUtteranceSeconds + ctx.Config.ChunkSeconds)

	out := &emitter{ctx: ctx, json: options.events, quiet: options.quiet}

	runCtx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	engine, err := asr.Start(runCtx, engineConfig)
	if err != nil {
		return FailureError("%v", err)
	}
	defer engine.Close()

	out.write("ready", map[string]any{
		"load":  engine.LoadTime(),
		"model": target,
		"args":  strings.Join(engineConfig.CommandLine(), " "),
	})

	// Repair runs off the audio path so the next segment can be transcribing
	// while the previous one is being corrected. A single worker is enough: a
	// repair costs a fraction of the utterance length, and serialising repairs
	// keeps the transcript order deterministic.
	var (
		repairCheck   asr.Repairer
		repairQueue   = make(chan repairJob, 8)
		repairResults = make(chan repairResult, 8)
		repairWorkers sync.WaitGroup
	)
	if options.refine {
		picked := options.refineMod
		if picked == "" {
			picked = resolveModel(ctx, true)
		}
		if picked == "" {
			picked = target
		}
		repairCheck = newRepairer(ctx, config.Expand(picked), binary, options.threads, ctx.Config.Language)
		repairWorkers.Add(1)
		go func() {
			defer repairWorkers.Done()
			for job := range repairQueue {
				started := time.Now()
				text, err := repairCheck.Transcribe(context.Background(), job.pcm)
				if err != nil {
					repairResults <- repairResult{job: job, err: err}
					continue
				}
				repairResults <- repairResult{job: job, text: text, took: time.Since(started)}
			}
		}()
	}

	// The transcript and the engine's event stream are consumed by separate
	// goroutines; both run until the process ends.
	var (
		transcript   = asr.NewTranscript()
		partialCount int
		lags         []float64
		testStart    = time.Now()
		audioFed     time.Duration
	)

	engineDone := make(chan struct{})
	go func() {
		defer close(engineDone)
		// Each partial is placed at the audio position reached when it arrived.
		// For the fixed-window streaming this project uses, that position is the
		// window boundary the model just finished decoding, which is exactly what
		// the repair pass needs to know which text to replace.
		var previousPartialSample int64
		for event := range engine.Events() {
			switch event.Kind {
			case asr.EventPartial:
				partialCount++
				lags = append(lags, event.FedSeconds)
				end := int64(event.FedSeconds * audio.SampleRate)
				if end < previousPartialSample {
					end = previousPartialSample
				}
				transcript.AppendStream(event.Text, previousPartialSample, end)
				previousPartialSample = end
				out.write("partial", map[string]any{
					"text":        event.Text,
					"fed_seconds": event.FedSeconds,
					"partials":    partialCount,
				})
			case asr.EventSnapshot:
				transcript.SetSnapshot(event.Text, 0, engine.FedSamples())
				previousPartialSample = engine.FedSamples()
				out.write("partial", map[string]any{
					"text":        event.Text,
					"snapshot":    true,
					"fed_seconds": event.FedSeconds,
				})
			case asr.EventSpeechStart, asr.EventSpeechEnd:
				out.write("vad", map[string]any{
					"event":          string(event.Kind),
					"sample_seconds": float64(event.Sample) / audio.SampleRate,
				})
			}
		}
	}()

	// Capture loop: feed the engine, run endpointing, and queue repairs.
	capture.Start()
	if options.seconds > 0 && options.filePath == "" {
		go func() {
			time.Sleep(time.Duration(options.seconds * float64(time.Second)))
			capture.Stop()
		}()
	}
	for chunk := range capture.Chunks() {
		if err := engine.Write(chunk.Data); err != nil {
			fmt.Fprintf(ctx.Stderr, "voiceime: %v\n", err)
			break
		}
		audioFed = time.Duration(engine.FedSamples()) * time.Second / audio.SampleRate
		buffer.Append(chunk.Data, chunk.StartSample)
		events := segmenter.Push(chunk.Data, chunk.StartSample)
		for _, event := range events {
			handleSegmentEvent(ctx, event, buffer, options, out, repairQueue)
		}
	}
	capture.Stop()

	// A stop is itself an endpoint: whatever was still being said is now
	// finished, so it is committed and repaired like any other utterance.
	for _, event := range segmenter.Flush() {
		handleSegmentEvent(ctx, event, buffer, options, out, repairQueue)
	}

	result, err := engine.Close()
	<-engineDone

	if options.refine {
		close(repairQueue)
		go func() {
			repairWorkers.Wait()
			close(repairResults)
		}()
		for result := range repairResults {
			if result.err != nil {
				fmt.Fprintf(ctx.Stderr, "voiceime: repair failed: %v\n", result.err)
				continue
			}
			transcript.Repair(result.job.startSample, result.job.endSample, result.text)
			out.write("repair", map[string]any{
				"text":           result.text,
				"took_seconds":   result.took.Seconds(),
				"speech_seconds": float64(result.job.endSample-result.job.startSample) / audio.SampleRate,
			})
		}
	}

	if err != nil {
		fmt.Fprintf(ctx.Stderr, "voiceime: %v\n", err)
	}

	// Committing is the last step: the repaired text is what a user would have
	// seen typed into their application.
	committed := transcript.CommitProvisional()
	wall := time.Since(testStart)

	// With a real-time feed, the audio position at which a partial arrived is
	// also the time from the user speaking to text appearing, so these three
	// numbers answer "how fast does it start showing text" and "how far behind
	// does it fall".
	firstLag, minLag, maxLag := 0.0, 0.0, 0.0
	for index, lag := range lags {
		if index == 0 || lag < minLag {
			minLag = lag
		}
		if index == 0 {
			firstLag = lag
		}
		if lag > maxLag {
			maxLag = lag
		}
	}
	lines := []string{
		fmt.Sprintf("model=%s chunk=%.2fs load=%v", target, engineConfig.ChunkSeconds, result.LoadTime),
		fmt.Sprintf("audio=%.2fs wall=%.2fs partials=%d text_lag_s: first=%.2f min=%.2f max=%.2f",
			audioFed.Seconds(), wall.Seconds(), partialCount, firstLag, minLag, maxLag),
		fmt.Sprintf("stream_text=%s", result.Text),
		fmt.Sprintf("voiceime_text=%s", committed),
	}
	if rtf, ok := result.Metrics["rtf"]; ok {
		lines = append(lines, fmt.Sprintf("framework_metrics rtf=%s x_realtime=%s",
			rtf, result.Metrics["x_realtime"]))
	}
	lines = append(lines, "note: with --realtime, text_lag is seconds of audio spoken before that text appeared")
	out.write("summary", map[string]any{"lines": lines})

	return ctx.Emit("stream", map[string]any{
		"model":                  target,
		"chunk_seconds":          engineConfig.ChunkSeconds,
		"load_ms":                result.LoadTime.Milliseconds(),
		"audio_seconds":          audioFed.Seconds(),
		"wall_seconds":           wall.Seconds(),
		"partials":               partialCount,
		"first_text_lag_seconds": firstLag,
		"min_text_lag_seconds":   minLag,
		"max_text_lag_seconds":   maxLag,
		"stream_text":            result.Text,
		"final_text":             committed,
		"metrics":                result.Metrics,
	})
}

// minRepairSeconds is the shortest segment worth a repair pass. A repair costs a
// process start and, for the larger repair models, about a second of model load,
// so running one for a cough or a clipped fragment is pure waste. Segments this
// short are still kept: they stay part of the stream text.
const minRepairSeconds = 0.4

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

// handleSegmentEvent closes out an utterance: it prints the boundary, queues the
// repair, and marks the provisional text as delivered. Repair is queued rather
// than run inline so audio keeps flowing while the correction is computed.
func handleSegmentEvent(
	ctx *Context,
	event audio.SegmentEvent,
	buffer *audio.RollingBuffer,
	options streamOptions,
	out *emitter,
	queue chan<- repairJob,
) {
	if event.Kind != audio.SpeechEnd {
		return
	}
	out.write("segment", map[string]any{
		"fed_seconds":    float64(event.EndSample) / audio.SampleRate,
		"start_seconds":  float64(event.StartSample) / audio.SampleRate,
		"end_seconds":    float64(event.EndSample) / audio.SampleRate,
		"speech_seconds": event.DurationSeconds(),
	})
	if !options.refine || event.DurationSeconds() < minRepairSeconds {
		return
	}
	pcm, ok := buffer.Slice(event.StartSample, event.EndSample)
	if !ok {
		// The segment fell out of the rolling buffer (a very long utterance).
		// Skipping is correct: decoding a truncated segment would produce text
		// that is wrong in a way the user cannot see is wrong.
		fmt.Fprintf(ctx.Stderr, "voiceime: segment too long to repair; keeping stream text\n")
		return
	}
	select {
	case queue <- repairJob{pcm: pcm, startSample: event.StartSample, endSample: event.EndSample}:
	default:
		fmt.Fprintf(ctx.Stderr, "voiceime: repair queue full; keeping stream text\n")
	}
}
