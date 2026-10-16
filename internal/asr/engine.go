// Package asr adapts audio.cpp's streaming CLI, and models the two-layer text
// the pipeline needs.
//
// The adapter spawns `audiocpp_cli --mode streaming --audio -` and feeds raw PCM
// on stdin. That is a deliberate integration choice rather than the only one
// available (there is also an in-process C API and a server with SSE output), and
// it wins here for three reasons:
//
//   - The CLI reads raw PCM from stdin and reports progress line by line, so it
//     is the whole streaming interface without a socket, a port or a supervisor.
//   - Model load is measurable and cheap for the models that matter: SenseVoice
//     Q8 loads in roughly 150 ms, so one process per dictation session is fine
//     and there is no warm-daemon complexity to get wrong.
//   - It keeps the dependency on audio.cpp to "the binary the user installed",
//     which is exactly the relationship this project wants.
//
// The C API stays a per-file swap if a future model needs in-process control: the
// exports of this package are deliberately narrow (Start, Write, Events, Close).
package asr

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wallacegibbon/voiceime/internal/audio"
)

// EventKind classifies what the engine reported.
type EventKind string

const (
	// EventReady means the model finished loading and audio may be written. The
	// delay before it is the model load time.
	EventReady EventKind = "ready"
	// EventPartial is text newly decoded since the previous partial: partials
	// concatenate into the running transcript. It is provisional: a window-based
	// streaming model can still change what it heard, and the repair pass will.
	EventPartial EventKind = "partial"
	// EventSnapshot replaces the entire provisional text. Only parakeet's
	// revision-capable session emits it today, but the framework defines it, so
	// the transcript model honours it.
	EventSnapshot EventKind = "snapshot"
	// EventSpeechStart / EventSpeechEnd are the model's own VAD, when it has one.
	// SenseVoice's streaming session does not report these; kroko's does. They
	// are forwarded rather than relied upon.
	EventSpeechStart EventKind = "speech_start"
	EventSpeechEnd   EventKind = "speech_end"
)

// Event is one observation from the running engine.
type Event struct {
	Kind EventKind `json:"kind"`
	Text string    `json:"text,omitempty"`
	// FedSeconds is how much audio had been written when this arrived. With a
	// real-time feed this is the latency figure that matters: the time from
	// "the user said it" to "text appeared".
	FedSeconds float64 `json:"fed_seconds"`
	// SinceStart is wall time since the process started.
	SinceStart time.Duration `json:"-"`
	// Sample is the model's own position for voice-activity events.
	Sample      int64   `json:"sample,omitempty"`
	Probability float64 `json:"probability,omitempty"`
}

// Result is what a finished session produced.
type Result struct {
	Text       string            `json:"text"`
	Metrics    map[string]string `json:"metrics,omitempty"`
	LoadTime   time.Duration     `json:"-"`
	TotalTime  time.Duration     `json:"-"`
	AudioFed   time.Duration     `json:"-"`
	ProcessErr error             `json:"-"`
}

// EngineConfig is everything the adapter needs to launch the CLI.
type EngineConfig struct {
	Binary string
	// WorkDir is the directory the CLI runs in. audio.cpp resolves some bundled
	// assets (a model's default Silero VAD, for instance) relative to the working
	// directory, so running from the checkout is what upstream's own tests do and
	// is the only reason this field exists.
	WorkDir        string
	Model          string
	Backend        string
	Threads        int
	Language       string
	ChunkSeconds   float64
	SessionOptions map[string]string
	RequestOptions map[string]string
}

// Engine is a running streaming session.
type Engine struct {
	cfg   EngineConfig
	cmd   *exec.Cmd
	stdin io.WriteCloser
	// events is closed when the process exits, so a consumer's range loop ends
	// without needing to poll a status flag.
	events chan Event
	// done is closed once the process has exited and events has been closed, so a
	// caller can wait for the end of the stream without reading it. Close must not
	// read events itself: the session owns a reader goroutine on it, and whoever
	// reads an event first is the only one to see it - a Close that drained the
	// channel would swallow the last partials exactly when a session is stopping,
	// which is when they matter most.
	done     chan struct{}
	fed      atomic.Int64 // samples written
	partials atomic.Int64 // partial events seen, for window bookkeeping if needed
	started  time.Time
	loadOnce sync.Once
	loadTime time.Duration
	// closeOnce guards the wait for the process to exit. Two callers (the
	// session's explicit Close and its deferred safety net, say) would otherwise
	// both wait and both set closeErr.
	closeOnce sync.Once
	closeErr  error

	resultMu sync.Mutex
	result   Result

	stderrLines []string
	stderrMu    sync.Mutex
}

// CommandLine returns the argv the engine runs, for logs and for a user who
// wants to reproduce a session by hand. Reproducibility matters more than
// tidiness here: every quality question can be answered by re-running this.
func (c EngineConfig) CommandLine() []string {
	args := []string{
		"--task", "asr",
		"--mode", "streaming",
		"--model", c.Model,
		"--backend", c.Backend,
		"--threads", strconv.Itoa(c.Threads),
		"--audio", "-",
		"--input-format", "s16le",
		"--input-rate", strconv.Itoa(audio.SampleRate),
		"--input-channels", "1",
	}
	// The family is inferred from the model path by the CLI's own resolver,
	// which is also what model discovery relies on. Passing it explicitly would
	// only add a way to get it wrong.
	if c.Language != "" {
		args = append(args, "--language", c.Language)
	}
	if c.ChunkSeconds > 0 {
		args = append(args, "--request-option", "audio_chunk_duration_sec="+trimFloat(c.ChunkSeconds))
	}
	for _, key := range sortedKeys(c.RequestOptions) {
		args = append(args, "--request-option", key+"="+c.RequestOptions[key])
	}
	for _, key := range sortedKeys(c.SessionOptions) {
		args = append(args, "--session-option", key+"="+c.SessionOptions[key])
	}
	return args
}

func trimFloat(value float64) string {
	text := strconv.FormatFloat(value, 'f', 3, 64)
	return strings.TrimRight(strings.TrimRight(text, "0"), ".")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}

// Start launches the engine and waits until it can accept audio.
func Start(ctx context.Context, cfg EngineConfig) (*Engine, error) {
	if cfg.Binary == "" {
		return nil, fmt.Errorf("no audiocpp_cli binary configured")
	}
	if cfg.Model == "" {
		return nil, fmt.Errorf("no model configured")
	}
	if cfg.Backend == "" {
		cfg.Backend = "cpu"
	}
	if cfg.Threads <= 0 {
		cfg.Threads = 4
	}

	args := cfg.CommandLine()
	cmd := exec.CommandContext(ctx, cfg.Binary, args...)
	if cfg.WorkDir != "" {
		cmd.Dir = cfg.WorkDir
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	engine := &Engine{
		cfg:     cfg,
		cmd:     cmd,
		stdin:   stdin,
		events:  make(chan Event, 64),
		done:    make(chan struct{}),
		started: time.Now(),
		result:  Result{Metrics: map[string]string{}},
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", cfg.Binary, err)
	}

	stdoutDone := make(chan struct{})
	go func() {
		defer close(stdoutDone)
		engine.readStdout(stdout)
	}()
	go engine.readStderr(stderr)
	go func() {
		waitErr := cmd.Wait()
		// Wait() closes the stdout pipe, so the reader sees EOF; waiting for it to
		// finish here is what makes closing events safe. Without it the reader
		// could still be sending to the channel after the process was reaped.
		<-stdoutDone
		engine.resultMu.Lock()
		engine.result.ProcessErr = waitErr
		engine.result.TotalTime = time.Since(engine.started)
		engine.resultMu.Unlock()
		close(engine.events)
		close(engine.done)
	}()

	// Wait for the ready line instead of sleeping: the load time is reported
	// this way, and a model that cannot load fails here rather than after the
	// user has spoken.
	readyDeadline := time.After(120 * time.Second)
	for {
		select {
		case event, ok := <-engine.events:
			if !ok {
				return nil, engine.loadFailure()
			}
			if event.Kind == EventReady {
				return engine, nil
			}
		case <-readyDeadline:
			engine.Close()
			return nil, fmt.Errorf("engine did not become ready within 120s: %s", engine.stderrTail())
		case <-ctx.Done():
			engine.Close()
			return nil, ctx.Err()
		}
	}
}

// loadFailure explains a process that died before becoming ready.
func (e *Engine) loadFailure() error {
	if tail := e.stderrTail(); tail != "" {
		return fmt.Errorf("engine exited before it was ready: %s", tail)
	}
	return fmt.Errorf("engine exited before it was ready (exit status %v)", e.cmd.ProcessState)
}

// Events is the stream of observations. It closes when the process exits.
func (e *Engine) Events() <-chan Event { return e.events }

// Write feeds raw 16 kHz mono PCM. The write blocks when the engine is behind,
// which is the back-pressure the capture layer needs.
func (e *Engine) Write(pcm []byte) error {
	if len(pcm) == 0 {
		return nil
	}
	if _, err := e.stdin.Write(pcm); err != nil {
		return fmt.Errorf("write audio to engine: %w", err)
	}
	e.fed.Add(int64(audio.SampleCount(pcm)))
	return nil
}

// FedSamples is how much audio has been written so far.
func (e *Engine) FedSamples() int64 { return e.fed.Load() }

// LoadTime is how long the model took to become ready.
func (e *Engine) LoadTime() time.Duration { return e.loadTime }

// Close finishes the stream and returns the session result. Closing stdin is the
// protocol's end-of-audio signal: the CLI flushes the model state, prints the
// joined transcript and exits, which is what makes a push-to-talk style stop work
// without a special command. Safe to call more than once, and from more than one
// goroutine; the second caller gets the same result.
//
// It waits for the process to exit without reading the event stream, so a session
// draining that stream still sees everything the process emitted on its way out.
func (e *Engine) Close() (Result, error) {
	e.closeOnce.Do(func() {
		_ = e.stdin.Close()
		select {
		case <-e.done:
			return
		case <-time.After(30 * time.Second):
		}
		_ = e.cmd.Process.Kill()
		// Reaping the process releases the pipes; not waiting at all would leave
		// the deadline path racing the reader goroutine for the last events.
		select {
		case <-e.done:
		case <-time.After(5 * time.Second):
		}
		e.closeErr = fmt.Errorf("engine did not exit within 30s of end of audio")
	})
	return e.Result(), e.closeErr
}

// Result returns what the session produced so far.
func (e *Engine) Result() Result {
	e.resultMu.Lock()
	defer e.resultMu.Unlock()
	out := e.result
	out.LoadTime = e.loadTime
	if out.Metrics == nil {
		out.Metrics = map[string]string{}
	}
	return out
}

func (e *Engine) readStdout(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		// Result lines are the session's authoritative output: the joined
		// transcript and the framework's own timings. They are recorded rather
		// than emitted, because a consumer's stream of partials is already a
		// superset of what they say.
		if key, value, ok := classifyFinal(line); ok {
			e.recordFinal(key, value)
			continue
		}
		event, ok := parseLine(line)
		if !ok {
			continue
		}
		event.FedSeconds = float64(e.fed.Load()) / audio.SampleRate
		event.SinceStart = time.Since(e.started)
		if event.Kind == EventReady {
			e.loadOnce.Do(func() { e.loadTime = time.Since(e.started) })
		}
		if event.Kind == EventPartial {
			e.partials.Add(1)
		}
		select {
		case e.events <- event:
		default:
			// A slow consumer must not block the reader: stdout is also the
			// process's only liveness signal. Dropping a partial is recoverable
			// because the final transcript is authoritative, whereas blocking
			// here would stall the audio feed instead.
		}
	}
}

// recordFinal stores a result line. text_output is the joined transcript; the
// metrics.* keys are the framework's own timing and memory report, kept verbatim
// so a measurement run can quote them without voiceime re-deriving anything.
func (e *Engine) recordFinal(key, value string) {
	e.resultMu.Lock()
	defer e.resultMu.Unlock()
	switch {
	case key == "text_output":
		e.result.Text = value
	case strings.HasPrefix(key, "metrics."):
		e.result.Metrics[strings.TrimPrefix(key, "metrics.")] = value
	}
}

func (e *Engine) readStderr(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		e.stderrMu.Lock()
		e.stderrLines = append(e.stderrLines, line)
		if len(e.stderrLines) > 20 {
			e.stderrLines = e.stderrLines[len(e.stderrLines)-20:]
		}
		e.stderrMu.Unlock()
	}
}

func (e *Engine) stderrTail() string {
	e.stderrMu.Lock()
	defer e.stderrMu.Unlock()
	return strings.Join(e.stderrLines, " | ")
}

// parseLine decodes one line of the CLI's streaming report.
//
// The format is line-oriented key=value because it has to work when stdout is a
// pipe: in that case the CLI prints `partial_text=` lines rather than painting
// running text, which is what makes this integration a protocol instead of
// screen-scraping. See app/cli/partial_render.h in audio.cpp.
func parseLine(line string) (Event, bool) {
	line = strings.TrimRight(line, "\r")
	if line == "" {
		return Event{}, false
	}
	switch key, value, _ := strings.Cut(line, "="); key {
	case "audio_input":
		// "audio_input=stdin format=s16le rate=16000 channels=1"
		return Event{Kind: EventReady, Text: value}, true
	case "partial_text":
		return Event{Kind: EventPartial, Text: value}, true
	case "partial_text_snapshot":
		return Event{Kind: EventSnapshot, Text: value}, true
	case "event":
		// "event=speech_start sample=16000 probability=0.9"
		fields := strings.Fields(value)
		if len(fields) == 0 {
			return Event{}, false
		}
		event := Event{}
		switch fields[0] {
		case "speech_start":
			event.Kind = EventSpeechStart
		case "speech_end":
			event.Kind = EventSpeechEnd
		case "speech_segment":
			// Not a boundary; treated as a marker with no state change.
			event.Kind = EventSpeechEnd
		default:
			return Event{}, false
		}
		for _, field := range fields[1:] {
			name, raw, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			switch name {
			case "sample":
				event.Sample, _ = strconv.ParseInt(raw, 10, 64)
			case "probability":
				event.Probability, _ = strconv.ParseFloat(raw, 64)
			}
		}
		return event, true
	}
	return Event{}, false
}

// classifyFinal recognises the lines the CLI prints after the stream ends. They
// are consumed by the reader goroutine's caller (see consumeFinal) rather than
// turned into events, so this function only answers "is this a result line".
func classifyFinal(line string) (key, value string, ok bool) {
	key, value, found := strings.Cut(strings.TrimRight(line, "\r"), "=")
	if !found {
		return "", "", false
	}
	switch {
	case key == "text_output", key == "family", key == "task", key == "mode",
		strings.HasPrefix(key, "metrics."):
		return key, value, true
	}
	return "", "", false
}
