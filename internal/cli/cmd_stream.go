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

	"github.com/wallacegibbon/voiceime/internal/audio"
	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/daemon"
)

// cmdStream is the observable end of the pipeline: it captures audio, streams it
// through the engine and re-decodes finished segments, printing everything it
// learns.
//
// It runs the same daemon session the IBus engine runs - only the sink differs -
// so that what is measured here is what a user gets. Tuning this system is
// empirical, and this is where the numbers come from.
func cmdStream() *Command {
	var (
		filePath    string
		seconds     float64
		realtime    bool
		chunk       float64
		refine      bool
		refineModel string
		events      bool
		quiet       bool
		threads     int
		model       string
	)
	flags := func(set *flag.FlagSet) {
		set.StringVar(&filePath, "file", "", "replay a WAV file instead of the microphone")
		set.BoolVar(&realtime, "realtime", true, "with --file, pace the replay at real time")
		set.Float64Var(&seconds, "seconds", 0, "stop after this many seconds of capture (0 = until end of input)")
		set.Float64Var(&chunk, "chunk", 0, "streaming window in seconds (default: from config)")
		set.BoolVar(&refine, "refine", true, "re-transcribe finished segments offline")
		set.StringVar(&refineModel, "refine-model", "", "server entry for the repair pass (default: asr_server_model)")
		set.BoolVar(&events, "events", false, "print one JSON object per event instead of text")
		set.BoolVar(&quiet, "quiet", false, "print only committed text")
		set.IntVar(&threads, "threads", 0, "worker threads (default: from config)")
		set.StringVar(&model, "model", "", "server entry for live partials (default: asr_server_stream_model)")
	}
	return &Command{
		Name:    "stream",
		Summary: "Capture audio and print live transcription with timing and repair",
		Usage:   "voiceime stream [--file F] [--seconds N] [--events] [--quiet]",
		Flags:   flags,
		Run: func(ctx *Context, args []string) error {
			return runStream(ctx, streamOptions{
				filePath:    filePath,
				realtime:    realtime,
				seconds:     seconds,
				chunk:       chunk,
				refine:      refine,
				refineModel: refineModel,
				events:      events,
				quiet:       quiet,
				threads:     threads,
				model:       model,
			})
		},
	}
}

type streamOptions struct {
	filePath    string
	realtime    bool
	seconds     float64
	chunk       float64
	refine      bool
	refineModel string
	events      bool
	quiet       bool
	threads     int
	model       string
}

// emitter serialises output. It is the single writer, so JSON lines cannot
// interleave when a repair finishes while audio is still arriving.
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
	case "preedit":
		if e.quiet {
			return
		}
		// No timestamp: a preedit update is a snapshot of everything still
		// provisional, so an "audio position" would be meaningless.
		e.ctx.Print("[preedit] %s", payload["text"])
	case "repair":
		e.ctx.Print("[repair %.2fs] %s", payload["took_seconds"], payload["text"])
	case "segment":
		e.ctx.Print("[+%5.2fs] segment %.2fs-%.2fs (%.2fs speech)", payload["fed_seconds"],
			payload["start_seconds"], payload["end_seconds"], payload["speech_seconds"])
	case "commit":
		e.ctx.Print("[commit] %s", payload["text"])
	case "skipped":
		e.ctx.Print("[skipped] %s", payload["reason"])
	case "summary":
		for _, line := range payload["lines"].([]string) {
			e.ctx.Print("%s", line)
		}
	}
}

// streamSink mirrors what the IBus engine does with the same callbacks, so the
// CLI shows the preedit/commit split a user would actually experience.
type streamSink struct{ emitter *emitter }

func (s streamSink) Commit(text string) error {
	s.emitter.write("commit", map[string]any{"text": text})
	return nil
}

func (s streamSink) Preedit(text string, cursor int, visible bool) error {
	if text == "" {
		return nil
	}
	s.emitter.write("preedit", map[string]any{"text": text, "cursor": cursor})
	return nil
}

func runStream(ctx *Context, options streamOptions) error {
	if options.threads == 0 {
		options.threads = ctx.Config.Threads
	}
	if strings.TrimSpace(ctx.Config.ASRServerURL) == "" {
		return PreconditionError("voiceime server install",
			"nothing is configured to decode audio: run `voiceime server install`")
	}
	// --model and --refine-model now name server *entries*, not checkpoint paths: the
	// service owns the weights. They exist for measuring one entry against another.
	streamModel := strings.TrimSpace(options.model)
	if streamModel == "" {
		streamModel = serverStreamModelID()
	}

	// A per-run copy of the config: chunk and refine are per-invocation knobs, and
	// mutating the loaded config would leak them into anything else that reads it.
	cfg := *ctx.Config
	if options.chunk > 0 {
		cfg.ChunkSeconds = options.chunk
	}
	cfg.RefineOnSegmentEnd = options.refine
	cfg.Threads = options.threads

	newCapture := func() (audio.Capture, error) { return audio.NewMicrophone(&cfg) }
	if options.filePath != "" {
		path := config.Expand(options.filePath)
		if _, err := os.Stat(path); err != nil {
			return FailureError("--file %s: %v", path, err)
		}
		newCapture = func() (audio.Capture, error) { return audio.NewFileReplay(path, options.realtime) }
	}

	out := &emitter{ctx: ctx, json: options.events, quiet: options.quiet}
	runCtx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if options.seconds > 0 && options.filePath == "" {
		go func() {
			time.Sleep(time.Duration(options.seconds * float64(time.Second)))
			cancel()
		}()
	}

	started := time.Now()
	// The repair entry follows the same rule as the engine's: the flag, then
	// asr_server_model.
	repairModel := strings.TrimSpace(options.refineModel)
	if repairModel == "" {
		repairModel = serverModelID()
	}
	out.write("repair_route", map[string]any{"detail": serverDescription(&cfg, repairModel)})
	session := daemon.Options{
		Config:      &cfg,
		ServerURL:   cfg.ASRServerURL,
		StreamModel: streamModel,
		RepairModel: repairModel,
		NewCapture:  newCapture,
		Sink:        streamSink{emitter: out},
		Observer: daemon.Observer{
			Ready: func(load time.Duration) {
				out.write("ready", map[string]any{
					"load": load, "model": streamModel,
				})
			},
			Partial: func(text string, fedSeconds float64) {
				out.write("partial", map[string]any{"text": text, "fed_seconds": fedSeconds})
			},
			Segment: func(start, end, speech float64) {
				out.write("segment", map[string]any{
					"fed_seconds": end, "start_seconds": start, "end_seconds": end, "speech_seconds": speech,
				})
			},
			Repair: func(text string, took time.Duration, speech float64) {
				out.write("repair", map[string]any{
					"text": text, "took_seconds": took.Seconds(), "speech_seconds": speech,
				})
			},
			Skipped: func(reason string) { out.write("skipped", map[string]any{"reason": reason}) },
			Error:   func(err error) { fmt.Fprintf(ctx.Stderr, "voiceime: %v\n", err) },
		},
	}

	result, runErr := session.Run(runCtx)
	wall := time.Since(started)

	lines := []string{
		fmt.Sprintf("server=%s stream_entry=%s load=%v", cfg.ASRServerURL, streamModel, result.LoadTime),
		fmt.Sprintf("repair_entry=%s", repairModel),
		fmt.Sprintf("audio=%.2fs wall=%.2fs partials=%d", result.AudioFed.Seconds(), wall.Seconds(), result.Partials),
		fmt.Sprintf("stream_text=%s", result.StreamText),
		fmt.Sprintf("voiceime_text=%s", result.FinalText),
	}
	if rtf, ok := result.Metrics["rtf"]; ok {
		lines = append(lines, fmt.Sprintf("framework_metrics rtf=%s x_realtime=%s",
			rtf, result.Metrics["x_realtime"]))
	}
	out.write("summary", map[string]any{"lines": lines})

	if runErr != nil && runCtx.Err() == nil {
		return FailureError("%v", runErr)
	}
	return ctx.Emit("stream", map[string]any{
		"model":         streamModel,
		"chunk_seconds": cfg.ChunkSeconds,
		"load_ms":       result.LoadTime.Milliseconds(),
		"audio_seconds": result.AudioFed.Seconds(),
		"wall_seconds":  wall.Seconds(),
		"partials":      result.Partials,
		"stream_text":   result.StreamText,
		"final_text":    result.FinalText,
		"metrics":       result.Metrics,
	})
}
