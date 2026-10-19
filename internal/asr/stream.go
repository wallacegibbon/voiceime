package asr

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wallacegibbon/voiceime/internal/audio"
)

// StreamConfig describes a live streaming session against a resident server.
type StreamConfig struct {
	// BaseURL is the server root, e.g. http://127.0.0.1:18080.
	BaseURL string
	// Model is the id of an entry configured with mode "streaming".
	Model string
	// Language is passed through when set.
	Language string
	// DialTimeout bounds connecting and the response headers. Once they arrive the
	// session runs as long as the user does.
	DialTimeout time.Duration
}

// Stream is a live streaming session served by audio.cpp's resident server.
//
// This is why the server exists, and why a dictation session no longer starts a
// process: the weights are already loaded by the service, so beginning to dictate is
// opening a connection. Its surface mirrors the CLI adapter it replaced (Write,
// Events, FedSamples, Close) so the daemon cannot tell the difference.
//
// It speaks HTTP over a socket rather than through net/http, and that is not
// gold-plating: http.Client.Do does not return until the request body is finished,
// even when the server has already answered (measured - the response headers arrive
// at 0.00s and Do still blocks), so the standard client cannot express "open the
// session, then send audio". curl works because it speaks HTTP directly; so does
// this: a chunked request body up, a chunked SSE response down, both written out by
// hand.
type Stream struct {
	conn net.Conn
	cfg  StreamConfig
	// reader is the buffered response stream, kept from the handshake so the SSE
	// reader starts exactly where the headers ended.
	reader *bufio.Reader

	events chan Event
	done   chan struct{}

	writeMu sync.Mutex
	closed  bool
	cancel  context.CancelFunc

	fed     atomic.Int64
	started time.Time
	ready   time.Duration

	resultMu sync.Mutex
	result   Result
	closeErr error
	closeOne sync.Once
}

// OpenStream connects to the server, opens a live session and starts reading it.
//
// It returns as soon as the server has accepted the session, so the caller can start
// feeding audio immediately; a failure here is a failure of the machine's setup (the
// service is not running), which is why the error says so.
func OpenStream(ctx context.Context, cfg StreamConfig) (*Stream, error) {
	if strings.TrimSpace(cfg.BaseURL) == "" {
		return nil, fmt.Errorf("no server configured: run `voiceime server install`, or set asr_server_url")
	}
	if strings.TrimSpace(cfg.Model) == "" {
		return nil, fmt.Errorf("no streaming model id configured: set asr_server_stream_model")
	}
	parsed, err := url.Parse(strings.TrimRight(cfg.BaseURL, "/"))
	if err != nil {
		return nil, fmt.Errorf("asr_server_url %q: %w", cfg.BaseURL, err)
	}
	host := parsed.Host
	if parsed.Port() == "" {
		host = net.JoinHostPort(parsed.Hostname(), "80")
	}
	timeout := cfg.DialTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}

	conn, err := net.DialTimeout("tcp", host, timeout)
	if err != nil {
		return nil, fmt.Errorf("cannot reach the streaming server at %s: %w (is it running? `systemctl --user status voiceime-asr-server`)", host, err)
	}

	stream := &Stream{
		conn:    conn,
		cfg:     cfg,
		events:  make(chan Event, 64),
		done:    make(chan struct{}),
		started: time.Now(),
		result:  Result{Metrics: map[string]string{}},
	}
	ctx, cancel := context.WithCancel(ctx)
	stream.cancel = cancel
	// A cancelled session closes the socket, which is what unblocks the reader.
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()

	_ = conn.SetDeadline(time.Now().Add(timeout))
	if err := stream.handshake(parsed); err != nil {
		_ = conn.Close()
		cancel()
		return nil, err
	}
	// The session is as long as the speaker wants; the deadline was only for opening
	// it. Cancellation is the socket, not a timeout.
	_ = conn.SetDeadline(time.Time{})
	stream.ready = time.Since(stream.started)

	go stream.readEvents()
	return stream, nil
}

// handshake writes the request and reads the status line and headers, so a session
// that the server refuses is reported before any audio is fed.
func (s *Stream) handshake(base *url.URL) error {
	path := base.Path
	if path == "" || path == "/" {
		path = ""
	}
	query := url.Values{}
	query.Set("model", s.cfg.Model)
	query.Set("sample_rate", strconv.Itoa(audio.SampleRate))
	query.Set("channels", "1")
	query.Set("sample_format", "s16le")
	query.Set("stream", "true")
	if s.cfg.Language != "" {
		query.Set("language", s.cfg.Language)
	}
	request := fmt.Sprintf(
		"POST %s/v1/audio/transcriptions/live?%s HTTP/1.1\r\nHost: %s\r\n"+
			"Content-Type: application/octet-stream\r\nAccept: text/event-stream\r\n"+
			"Transfer-Encoding: chunked\r\n\r\n",
		path, query.Encode(), base.Host)
	if _, err := io.WriteString(s.conn, request); err != nil {
		return fmt.Errorf("send the streaming request: %w", err)
	}

	reader := bufio.NewReader(s.conn)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		return fmt.Errorf("read the streaming response: %w", err)
	}
	fields := strings.Fields(strings.TrimSpace(statusLine))
	if len(fields) < 2 {
		return fmt.Errorf("streaming server sent a malformed status line: %q", strings.TrimSpace(statusLine))
	}
	code, err := strconv.Atoi(fields[1])
	if err != nil {
		return fmt.Errorf("streaming server sent a malformed status line: %q", strings.TrimSpace(statusLine))
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return fmt.Errorf("read the streaming response headers: %w", err)
		}
		if strings.TrimSpace(line) == "" {
			break
		}
	}
	if code != 200 {
		// The body of a refused request is short and says why; one line of it is the
		// most useful thing to carry.
		body, _ := io.ReadAll(io.LimitReader(reader, 2048))
		return fmt.Errorf("streaming server refused the session: %s: %s", fields[1], firstLine(string(body)))
	}
	s.reader = reader
	return nil
}

// Write feeds raw 16 kHz mono PCM. It blocks when the server is behind, which is the
// back-pressure the capture loop needs.
func (s *Stream) Write(pcm []byte) error {
	if len(pcm) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.closed {
		return fmt.Errorf("write audio to a closed session")
	}
	// Chunked framing, written with the payload in one call so the capture loop does
	// not pay two syscalls per block.
	buffer := make([]byte, 0, len(pcm)+16)
	buffer = append(buffer, fmt.Sprintf("%x\r\n", len(pcm))...)
	buffer = append(buffer, pcm...)
	buffer = append(buffer, '\r', '\n')
	if _, err := s.conn.Write(buffer); err != nil {
		return fmt.Errorf("write audio to the streaming server: %w", err)
	}
	s.fed.Add(int64(audio.SampleCount(pcm)))
	return nil
}

// FedSamples is how much audio has been written so far.
func (s *Stream) FedSamples() int64 { return s.fed.Load() }

// Events is the stream of observations. It closes when the server finishes.
func (s *Stream) Events() <-chan Event { return s.events }

// LoadTime is how long the session took to open. There is no model load in a session
// any more - the service did that at startup - so this is what a caller reports as
// "ready".
func (s *Stream) LoadTime() time.Duration { return s.ready }

// Close ends the audio and waits for the server to finish the transcript. Writing the
// terminating chunk is the end-of-audio signal: the server flushes the model and
// answers done, which is what makes a push-to-talk stop work without a special
// command.
//
// The error it returns is how the session ended, and a session that ended badly is a
// failure rather than a short transcript: an error event, a stall past the server's
// idle timeout and a connection that closed without the terminating chunk all come
// back as an error carrying the server's reason.
func (s *Stream) Close() (Result, error) {
	s.closeOne.Do(func() {
		s.writeMu.Lock()
		if !s.closed {
			s.closed = true
			_, _ = io.WriteString(s.conn, "0\r\n\r\n")
		}
		s.writeMu.Unlock()

		select {
		case <-s.done:
		case <-time.After(30 * time.Second):
			s.closeErr = fmt.Errorf("the streaming server did not finish within 30s of end of audio")
			s.cancel()
			<-s.done
		}
		s.cancel()
		_ = s.conn.Close()
		// A failed session has to be a failed call, not a Result field nobody reads.
		// The server refuses to answer "done" for a body that was truncated, an error
		// event, or a stall - precisely so that those cannot be mistaken for the
		// speaker stopping - and this is the half that makes that true for a caller.
		if s.closeErr == nil {
			if result := s.Result(); result.ProcessErr != nil {
				s.closeErr = result.ProcessErr
			}
		}
	})
	return s.Result(), s.closeErr
}

// Result returns what the session produced so far.
func (s *Stream) Result() Result {
	s.resultMu.Lock()
	defer s.resultMu.Unlock()
	out := s.result
	out.LoadTime = s.ready
	out.AudioFed = time.Duration(s.fed.Load()) * time.Second / audio.SampleRate
	if out.Metrics == nil {
		out.Metrics = map[string]string{}
	}
	return out
}

// readEvents decodes the chunked SSE response. Deltas become partial events, exactly
// as the CLI's partial_text lines did. The final "done" carries the joined transcript
// and is recorded rather than emitted: the repair pass decides what text becomes
// final, and a snapshot here would overwrite it.
func (s *Stream) readEvents() {
	defer func() {
		close(s.events)
		close(s.done)
	}()
	body := &chunkedReader{reader: s.reader}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		payload, ok := strings.CutPrefix(strings.TrimSpace(scanner.Text()), "data:")
		if !ok {
			continue
		}
		payload = strings.TrimSpace(payload)
		if payload == "" || payload == "[DONE]" {
			continue
		}
		// The error event nests its reason: the server answers
		// {"type":"error","error":{"message":...}} (app/server/http.cpp), so a
		// reader that only looks at the top level gets a failure with no cause.
		// The top-level field is kept because it costs nothing and a different
		// server may use it.
		var event struct {
			Type    string `json:"type"`
			Delta   string `json:"delta"`
			Text    string `json:"text"`
			Message string `json:"message"`
			Error   *struct {
				Message string `json:"message"`
			} `json:"error"`
			Timing json.RawMessage `json:"timing"`
		}
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			continue
		}
		switch event.Type {
		case "transcript.text.delta":
			if event.Delta == "" {
				continue
			}
			s.emit(Event{
				Kind:       EventPartial,
				Text:       event.Delta,
				FedSeconds: float64(s.fed.Load()) / audio.SampleRate,
			})
		case "transcript.text.done":
			s.resultMu.Lock()
			s.result.Text = event.Text
			s.result.TotalTime = time.Since(s.started)
			if len(event.Timing) > 0 {
				s.result.Metrics["timing"] = string(event.Timing)
			}
			s.resultMu.Unlock()
		case "error":
			message := event.Message
			if message == "" && event.Error != nil {
				message = event.Error.Message
			}
			if message == "" {
				// Nothing to quote. The payload is then the only evidence left,
				// and replacing it with "something went wrong" is how a failure
				// becomes unreportable.
				message = "the streaming server reported an error: " + firstLine(payload)
			}
			s.resultMu.Lock()
			if s.result.ProcessErr == nil {
				s.result.ProcessErr = fmt.Errorf("streaming server: %s", message)
			}
			s.resultMu.Unlock()
		}
	}
	if err := scanner.Err(); err != nil {
		s.resultMu.Lock()
		if s.result.ProcessErr == nil {
			s.result.ProcessErr = fmt.Errorf("read from the streaming server: %w", err)
		}
		s.resultMu.Unlock()
	}
}

func (s *Stream) emit(event Event) {
	event.SinceStart = time.Since(s.started)
	select {
	case s.events <- event:
	default:
		// A slow consumer must not block the reader: this goroutine is also the only
		// thing draining the socket. Dropping a partial is recoverable; stalling the
		// audio feed is not.
	}
}

// chunkedReader unwraps HTTP/1.1 chunked transfer encoding, which is what the server
// answers with: it has no length to declare, and the transcript arrives as it is
// produced.
type chunkedReader struct {
	reader *bufio.Reader
	left   int
	done   bool
}

func (c *chunkedReader) Read(p []byte) (int, error) {
	if c.done {
		return 0, io.EOF
	}
	if c.left == 0 {
		size, err := c.nextLine()
		if err != nil {
			if err == io.EOF {
				// The connection ended before the terminating chunk: a session the
				// server died in the middle of, which must not look like a finished
				// transcript.
				return 0, io.ErrUnexpectedEOF
			}
			return 0, err
		}
		if size == 0 {
			c.done = true
			return 0, io.EOF
		}
		c.left = size
	}
	if len(p) > c.left {
		p = p[:c.left]
	}
	n, err := c.reader.Read(p)
	c.left -= n
	if c.left == 0 {
		// Consume the CRLF that ends the chunk.
		if _, err := c.nextLine(); err != nil {
			if err == io.EOF {
				return n, io.ErrUnexpectedEOF
			}
			return n, err
		}
	}
	if err != nil && err != io.EOF {
		return n, err
	}
	return n, nil
}

func (c *chunkedReader) nextLine() (int, error) {
	line, err := c.reader.ReadString('\n')
	if err != nil {
		return 0, err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return 0, nil
	}
	if index := strings.IndexByte(line, ';'); index >= 0 {
		line = line[:index]
	}
	size, err := strconv.ParseInt(line, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("malformed chunk size %q", line)
	}
	return int(size), nil
}

func firstLine(text string) string {
	if index := strings.IndexAny(text, "\r\n"); index >= 0 {
		text = text[:index]
	}
	if len(text) > 200 {
		text = text[:200] + "..."
	}
	return strings.TrimSpace(text)
}
