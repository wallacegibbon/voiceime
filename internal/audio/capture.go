package audio

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"time"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// Chunk is one block of captured audio.
type Chunk struct {
	Data []byte
	// StartSample is the position of this chunk in the stream the session has
	// seen, in samples at SampleRate. The transcript model uses it to know which
	// text a repair replaces.
	StartSample int64
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
	rate    int
	chunks  chan Chunk
	stop    chan struct{}
	stopped sync.Once
	errMu   sync.Mutex
	err     error

	sampleCursor int64
	dropped      int64
}

func newCaptureBase(rate int) *captureBase {
	return &captureBase{
		rate: rate,
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

// publish hands a chunk to the consumer, dropping the oldest chunk when the
// consumer is behind. Dropping the oldest rather than the newest keeps output
// close to what is being said right now, which is the whole point of a live
// input method.
func (c *captureBase) publish(data []byte) {
	chunk := Chunk{Data: data, StartSample: c.sampleCursor, DroppedSamples: c.dropped}
	c.sampleCursor += int64(SampleCount(data))
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

// readLoop blocks on reader, assembling fixed-size chunks. It is shared by every
// subprocess-backed source because both parec and pw-record behave identically
// here: a byte stream on stdout that ends when the process ends.
func (c *captureBase) readLoop(ctx context.Context, reader io.Reader, wait func() error) {
	defer close(c.chunks)
	blockBytes := c.rate * FrameMS / 1000 * BytesPerSample
	buffer := make([]byte, blockBytes)
	for {
		select {
		case <-c.stop:
			return
		default:
		}
		read, err := io.ReadFull(reader, buffer)
		if read > 0 {
			c.publish(append([]byte(nil), buffer[:read]...))
		}
		if err != nil {
			if err != io.ErrUnexpectedEOF && err != io.EOF {
				c.setErr(fmt.Errorf("capture read: %w", err))
			}
			// A source that exits on its own (device unplugged, stop requested)
			// is not an error; the wait result only matters if it failed early.
			if wait != nil {
				if waitErr := wait(); waitErr != nil {
					c.setErr(fmt.Errorf("capture process: %w", waitErr))
				}
			}
			return
		}
	}
}

// --------------------------------------------------------------------------- //
// Microphone
// --------------------------------------------------------------------------- //

// microphone captures from the sound server through a command-line client.
type microphone struct {
	*captureBase
	command  []string
	cmd      *exec.Cmd
	finished chan struct{}
}

// NewMicrophone picks the capture client available on this machine. parec is
// preferred because its PulseAudio-compatible interface is what PipeWire's pulse
// server implements most completely, including --device selection by source name.
func NewMicrophone(cfg *config.Config) (Capture, error) {
	rate := cfg.CaptureRate
	if rate == 0 {
		rate = SampleRate
	}
	if rate != SampleRate {
		return nil, fmt.Errorf("capture_rate must be %d: the models' VAD rejects other rates", SampleRate)
	}

	if parec := config.FindExecutable("parec"); parec != "" {
		args := []string{
			"--format=s16le",
			fmt.Sprintf("--rate=%d", rate),
			"--channels=1",
			"--raw",
			// Without a latency request the server may deliver seconds at once,
			// which would defeat streaming entirely.
			"--latency-msec=40",
		}
		if cfg.CaptureSource != "" {
			args = append(args, "--device="+cfg.CaptureSource)
		}
		return &microphone{
			captureBase: newCaptureBase(rate),
			command:     append([]string{parec}, args...),
			finished:    make(chan struct{}),
		}, nil
	}

	if pwRecord := config.FindExecutable("pw-record"); pwRecord != "" {
		args := []string{
			"--format", "s16",
			"--rate", fmt.Sprint(rate),
			"--channels", "1",
			"--raw",
			"-",
		}
		if cfg.CaptureSource != "" {
			args = append([]string{"--target=" + cfg.CaptureSource}, args...)
		}
		return &microphone{
			captureBase: newCaptureBase(rate),
			command:     append([]string{pwRecord}, args...),
			finished:    make(chan struct{}),
		}, nil
	}

	return nil, fmt.Errorf("neither parec nor pw-record is installed; install pipewire-bin or pulseaudio-utils")
}

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

	// The client's diagnostics are the only explanation available when a device
	// is missing, so they are kept as the error text.
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			if line := scanner.Text(); line != "" {
				m.setErr(fmt.Errorf("%s: %s", m.command[0], line))
			}
		}
	}()

	go func() {
		defer close(m.finished)
		m.readLoop(context.Background(), stdout, cmd.Wait)
	}()
	return nil
}

func (m *microphone) Stop() {
	m.captureBase.Stop()
	if m.cmd != nil && m.cmd.Process != nil {
		_ = m.cmd.Process.Kill()
	}
	select {
	case <-m.finished:
	case <-time.After(2 * time.Second):
	}
}

// --------------------------------------------------------------------------- //
// File replay
// --------------------------------------------------------------------------- //

// fileReplay replays a WAV file, either as fast as possible (accuracy work) or
// paced at real time (latency measurements, which is what `voiceime stream
// --file --realtime` needs).
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
		captureBase: newCaptureBase(SampleRate),
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
