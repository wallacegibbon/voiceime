package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/wallacegibbon/voiceime/internal/asr"
	"github.com/wallacegibbon/voiceime/internal/audio"
	"github.com/wallacegibbon/voiceime/internal/config"
)

// cmdTranscribe is the measurement front end: one recording in, one decode out, with
// the numbers that say what it cost.
func cmdTranscribe() *Command {
	var (
		file    string
		seconds float64
	)
	return &Command{
		Name:    "transcribe",
		Summary: "Transcribe a recording, or a few seconds of the microphone",
		Usage:   "voiceime transcribe [--file recording.wav | --seconds N]",
		Flags: func(set *flag.FlagSet) {
			set.StringVar(&file, "file", "", "WAV file to transcribe; empty uses the microphone")
			set.Float64Var(&seconds, "seconds", 5, "how long to record, when --file is empty")
		},
		Run: func(ctx *Context, args []string) error {
			return runTranscribe(ctx, file, seconds)
		},
	}
}

func runTranscribe(ctx *Context, file string, seconds float64) error {
	cfg := ctx.Config
	if file != "" {
		meta, err := os.Stat(config.Expand(file))
		if err != nil {
			return FailureError("%s: %v", file, err)
		}
		if meta.IsDir() {
			return UsageError("%s is a directory", file)
		}
	}

	var capture audio.Capture
	var err error
	if file != "" {
		capture, err = audio.NewFileReplay(config.Expand(file), false)
	} else {
		capture, err = audio.NewMicrophone(cfg)
	}
	if err != nil {
		return FailureError("%v", err)
	}
	if err := capture.Start(); err != nil {
		return FailureError("%v", err)
	}
	defer capture.Stop()

	// The same bound a dictation applies, so a forgotten command cannot record for ever.
	limit := int64(cfg.MaxUtteranceSeconds * audio.SampleRate)
	if file == "" && seconds > 0 {
		limit = int64(seconds * audio.SampleRate)
	}
	var pcm []byte
	for chunk := range capture.Chunks() {
		pcm = append(pcm, chunk.Data...)
		if int64(audio.SampleCount(pcm)) >= limit {
			break
		}
	}
	capture.Stop()
	if err := capture.Err(); err != nil {
		fmt.Fprintf(ctx.Stderr, "voiceime: %v\n", err)
	}
	if len(pcm) == 0 {
		return FailureError("no audio was recorded")
	}
	recorded := float64(audio.SampleCount(pcm)) / audio.SampleRate

	transcriber := asr.ServerTranscriber{
		BaseURL:  cfg.ASRServerURL,
		Language: cfg.Language,
		Timeout:  time.Duration(cfg.ASRServerTimeoutMS) * time.Millisecond,
	}
	started := time.Now()
	text, err := transcriber.Transcribe(context.Background(), pcm)
	took := time.Since(started)
	if err != nil {
		return FailureError("%v", err)
	}
	ctx.Print("%s", text)
	return ctx.Emit("transcribe", map[string]any{
		"text":     text,
		"seconds":  recorded,
		"took_ms":  took.Milliseconds(),
		"realtime": took.Seconds() / recorded,
		"bytes":    len(pcm),
		"source":   sourceName(file),
	})
}

func sourceName(file string) string {
	if file != "" {
		return file
	}
	return "microphone"
}
