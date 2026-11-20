package audio

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// Chunk is one block of captured audio.
type Chunk struct {
	Data []byte
	// DroppedSamples counts audio discarded because the consumer fell behind.
	// A non-zero value means the transcript is missing speech, which is worth
	// surfacing rather than hiding: the alternative would be unbounded memory.
	DroppedSamples int64
}

// Frames returns the number of samples in the chunk.
func (c Chunk) Frames() int { return SampleCount(c.Data) }

// Capture is a source of audio chunks.
type Capture interface {
	Start() error
	Chunks() <-chan Chunk
	Stop()
	// Err reports why the stream ended, if it ended badly.
	Err() error
}

// captureBase holds the reader-thread plumbing shared by every source.
type captureBase struct {
	chunks     chan Chunk
	stop       chan struct{}
	stopped    sync.Once
	errMu      sync.Mutex
	err        error
	diagnostic string

	dropped int64
}

func newCaptureBase() *captureBase {
	return &captureBase{
		// Buffered so a brief consumer stall is absorbed; anything longer is
		// dropped rather than allowed to grow without bound.
		chunks: make(chan Chunk, 250),
		stop:   make(chan struct{}),
	}
}

// Chunks exposes the receive side of the stream.
func (c *captureBase) Chunks() <-chan Chunk { return c.chunks }

// Stop ends the stream; safe to call more than once.
func (c *captureBase) Stop() {
	c.stopped.Do(func() { close(c.stop) })
}

// Err reports a failure that ended the stream.
func (c *captureBase) Err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.err
}

func (c *captureBase) setErr(err error) {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	if c.err == nil {
		c.err = err
	}
}

func (c *captureBase) setDiagnostic(line string) {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	c.diagnostic = line
}

// diagnosticTail reports what the capture client last complained about, for the
// error message when there is one.
func (c *captureBase) diagnosticTail() string {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.diagnostic
}

func (c *captureBase) publish(data []byte) {
	chunk := Chunk{Data: data, DroppedSamples: c.dropped}
	c.dropped = 0

	select {
	case c.chunks <- chunk:
		return
	default:
	}
	select {
	case dropped := <-c.chunks:
		c.dropped += int64(dropped.Frames()) + int64(chunk.Frames())
	default:
		c.dropped += int64(chunk.Frames())
	}
	select {
	case c.chunks <- chunk:
	case <-c.stop:
	}
}

func (c *captureBase) readLoop(reader io.Reader, wait func() error) {
	defer close(c.chunks)
	blockBytes := FrameMS * SampleRate / 1000 * BytesPerSample
	buffer := make([]byte, blockBytes)
	published := 0
	for {
		select {
		case <-c.stop:
			return
		default:
		}
		read, err := io.ReadFull(reader, buffer)
		if read > 0 {
			c.publish(append([]byte(nil), buffer[:read]...))
			published++
		}
		if err != nil {
			if err != io.ErrUnexpectedEOF && err != io.EOF {
				c.setErr(fmt.Errorf("capture read: %w", err))
				return
			}
			// The stream ended. Reap the client first: Wait is what releases it,
			// and it is the only place the exit status is available.
			var waitErr error
			if wait != nil {
				waitErr = wait()
			}
			select {
			case <-c.stop:
				return
			default:
			}
			if waitErr != nil {
				message := fmt.Sprintf("capture process exited: %v", waitErr)
				if tail := c.diagnosticTail(); tail != "" {
					message += ": " + tail
				}
				c.setErr(fmt.Errorf("%s", message))
				return
			}
			if published == 0 {
				message := "capture produced no audio"
				if tail := c.diagnosticTail(); tail != "" {
					message += ": " + tail
				}
				c.setErr(fmt.Errorf("%s", message))
			}
			return
		}
	}
}

// --------------------------------------------------------------------------- //

// microphone captures from the sound server through a command-line client.
type microphone struct {
	*captureBase
	command  []string
	cmd      *exec.Cmd
	finished chan struct{}
}

func NewMicrophone(cfg *config.Config) (Capture, error) {
	command, err := captureCommand(cfg)
	if err != nil {
		return nil, err
	}
	return &microphone{
		captureBase: newCaptureBase(),
		command:     command,
		finished:    make(chan struct{}),
	}, nil
}

// WarmUpCapture opens the capture once, throws the audio away, and returns the client it
// used with how long that client took to deliver its first sample.
//
// Three things are bought at start-up here, and every one of them is otherwise paid between
// a key coming down and the first sample - which is when a person starts speaking:
//
//   - what the installed capture tool needs to be told about its output: a process, 15-21ms
//     measured on 2026-11-20, and an answer that cannot change while this process runs;
//   - whether there is a client to record with at all, so that a machine with nothing to
//     record with says so in a log instead of in the middle of somebody's sentence;
//   - the sound server's own first open of the device. Measured on this machine on
//     2026-11-21: the first open after the server has started delivers its first sample
//     1.09s after the client starts, and every open after it 0.36s, because the server keeps
//     the device open once something has opened it. The 1.09s is the first dictation after a
//     login and nothing else, which is why it looked like a fault of the first hold rather
//     than of the machine (docs/measurements.md, "The first recording after the microphone
//     has been idle starts late").
//
// No audio is kept and none is sent anywhere: the capture is stopped as soon as one block
// has arrived.
func WarmUpCapture(cfg *config.Config) (string, time.Duration, error) {
	deadline := time.Now().Add(warmUpBudget)
	for {
		tool, took, err := warmUpOnce(cfg)
		if err == nil {
			return tool, took, nil
		}
		// A login can start the engine process before the sound server is listening, and a
		// warm-up that failed there would leave the one cost this exists to remove. It is
		// retried inside a budget rather than waited out for as long as it takes: an engine
		// that never came up would be the worse of the two failures.
		if !time.Now().Add(warmUpRetry).Before(deadline) {
			return "", 0, err
		}
		time.Sleep(warmUpRetry)
	}
}

// warmUpOnce is one attempt: start the client, wait for a block, stop it.
func warmUpOnce(cfg *config.Config) (string, time.Duration, error) {
	command, err := captureCommand(cfg)
	if err != nil {
		return "", 0, err
	}
	capture := &microphone{
		captureBase: newCaptureBase(),
		command:     command,
		finished:    make(chan struct{}),
	}
	started := time.Now()
	if err := capture.Start(); err != nil {
		return "", 0, err
	}
	defer capture.Stop()
	select {
	case chunk, ok := <-capture.Chunks():
		if !ok {
			if err := capture.Err(); err != nil {
				return "", 0, fmt.Errorf("%s: %w", command[0], err)
			}
			return "", 0, fmt.Errorf("%s produced no audio", command[0])
		}
		if len(chunk.Data) == 0 {
			return "", 0, fmt.Errorf("%s produced no audio", command[0])
		}
		return command[0], time.Since(started), nil
	case <-time.After(warmUpFirstSample):
		return "", 0, fmt.Errorf("%s delivered no sample within %v", command[0], warmUpFirstSample)
	}
}

// The warm-up's three bounds are variables so that a test does not have to wait them out: a
// test that sat through the budget would cost more than the failure it guards against.
var (
	// warmUpFirstSample is how long one attempt waits for the first block. A cold first open
	// was measured at 1.09s, which leaves room for a slower machine and not for a hung one.
	warmUpFirstSample = 2 * time.Second
	// warmUpBudget bounds all the attempts together, retries included.
	warmUpBudget = 6 * time.Second
	// warmUpRetry is the pause between attempts.
	warmUpRetry = 500 * time.Millisecond
)

func captureCommand(cfg *config.Config) ([]string, error) {
	// The rate is not a setting, and it is not a parameter either: audio.cpp's bundled VAD
	// rejects anything but 16 kHz, so a value that could be wrong is a value that would be.
	if parec := config.FindExecutable("parec"); parec != "" {
		args := []string{
			"--format=s16le",
			fmt.Sprintf("--rate=%d", SampleRate),
			"--channels=1",
			"--raw",
			// Without a latency request the server may deliver seconds at once,
			// which would defeat streaming entirely.
			"--latency-msec=40",
		}
		if cfg.CaptureSource != "" {
			args = append(args, "--device="+cfg.CaptureSource)
		}
		return append([]string{parec}, args...), nil
	}

	if pwRecord := config.FindExecutable("pw-record"); pwRecord != "" {
		args := []string{
			"--format", "s16",
			"--rate", fmt.Sprint(SampleRate),
			"--channels", "1",
		}
		// Whether this pw-record needs `-a/--raw` to put raw samples on stdout is a
		// property of its version, not of this program: upstream added the option after
		// 1.0.5, and the 1.0.5 on the machine this was found on rejects it outright
		// (`pw-record: unrecognized option '--raw'`) while writing raw samples to `-`
		// anyway. So ask it, once, rather than pick a side: a wrong guess here is not a
		// startup error, it is a recording that decodes to nothing.
		if pwRecordAcceptsRaw(pwRecord) {
			args = append(args, "--raw")
		}
		args = append(args, "-")
		if cfg.CaptureSource != "" {
			args = append([]string{"--target=" + cfg.CaptureSource}, args...)
		}
		return append([]string{pwRecord}, args...), nil
	}

	return nil, fmt.Errorf("neither parec nor pw-record is installed; install pipewire-bin or pulseaudio-utils")
}

// pwRecordAccepts asks the capture tool whether it knows an option. Its usage block is the
// only place the option set of the installed version is written down, and pw-record prints
// that block both for `--help` and as the error for an unrecognised option - so the answer
// is read from the output and the exit status is not looked at.
func pwRecordAccepts(tool, option string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), pwOptionProbeTimeout)
	defer cancel()
	out, _ := exec.CommandContext(ctx, tool, "--help").CombinedOutput()
	return strings.Contains(string(out), option)
}

// pwRecordAcceptsRaw asks the installed tool once and remembers what it answered.
//
// A capture client is built for every hold, so asking inside NewMicrophone would put the
// probe between the key coming up and the first sample - which is the moment a person
// starts speaking. Measured on this machine on 2026-11-20, `pw-record --help` costs
// 15-21ms: not something a person would notice, and not something to pay once per
// dictation either. The tool's path is part of what is remembered, so an installation
// that changes under a running engine is asked again rather than trusted.
var pwRecordRawAnswer = struct {
	mu    sync.Mutex
	tool  string
	asked bool
	raw   bool
}{}

func pwRecordAcceptsRaw(tool string) bool {
	pwRecordRawAnswer.mu.Lock()
	defer pwRecordRawAnswer.mu.Unlock()
	if !pwRecordRawAnswer.asked || pwRecordRawAnswer.tool != tool {
		pwRecordRawAnswer.raw = pwRecordAccepts(tool, "--raw")
		pwRecordRawAnswer.tool, pwRecordRawAnswer.asked = tool, true
	}
	return pwRecordRawAnswer.raw
}

// pwOptionProbeTimeout bounds the probe. pw-record prints a usage block and exits; a second
// means this machine is not answering, and then the shorter argument list is what gets tried
// - the same one a pw-record older than the option needs.
const pwOptionProbeTimeout = 2 * time.Second

// Start launches the capture client.
func (m *microphone) Start() error {
	cmd := exec.Command(m.command[0], m.command[1:]...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", m.command[0], err)
	}
	m.cmd = cmd

	// The client's diagnostics are the only explanation available when a device is
	// missing, so the last line is kept - but as a diagnostic, not as a failure:
	// these clients narrate formats and latencies while working correctly.
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			if line := scanner.Text(); line != "" {
				m.setDiagnostic(fmt.Sprintf("%s: %s", m.command[0], line))
			}
		}
	}()

	go func() {
		defer close(m.finished)
		m.readLoop(stdout, cmd.Wait)
	}()
	return nil
}

func (m *microphone) Stop() {
	m.captureBase.Stop()
	if m.cmd != nil && m.cmd.Process != nil {
		_ = m.cmd.Process.Kill()
	}
	if m.cmd == nil {
		// Never started: there is no reader goroutine to wait for, and waiting
		// would stall the caller for the whole timeout.
		return
	}
	select {
	case <-m.finished:
	case <-time.After(2 * time.Second):
	}
}

// --------------------------------------------------------------------------- //

type fileReplay struct {
	*captureBase
	data     []byte
	realtime bool
	done     chan struct{}
	cancel   chan struct{}
}

// NewFileReplay prepares a replay source.
func NewFileReplay(path string, realtime bool) (Capture, error) {
	wav, err := ReadWAV(path)
	if err != nil {
		return nil, err
	}
	return &fileReplay{
		captureBase: newCaptureBase(),
		data:        wav.Data,
		realtime:    realtime,
		done:        make(chan struct{}),
		cancel:      make(chan struct{}),
	}, nil
}

// Duration reports how much audio the file holds.
func (f *fileReplay) Duration() time.Duration {
	return time.Duration(DurationSeconds(f.data) * float64(time.Second))
}

func (f *fileReplay) Start() error {
	go func() {
		defer close(f.done)
		defer close(f.chunks)
		blockBytes := FrameMS * SampleRate / 1000 * BytesPerSample
		start := time.Now()
		for offset := 0; offset < len(f.data); offset += blockBytes {
			select {
			case <-f.cancel:
				return
			case <-f.stop:
				return
			default:
			}
			end := offset + blockBytes
			if end > len(f.data) {
				end = len(f.data)
			}
			if f.realtime {
				// Pace from the start of the replay so latency measurements
				// reflect what a live stream would look like.
				due := start.Add(time.Duration(SampleCount(f.data[:offset])) * time.Second / SampleRate)
				if wait := time.Until(due); wait > 0 {
					time.Sleep(wait)
				}
			}
			f.publish(f.data[offset:end])
		}
	}()
	return nil
}

func (f *fileReplay) Stop() {
	select {
	case <-f.cancel:
	default:
		close(f.cancel)
	}
	<-f.done
}
