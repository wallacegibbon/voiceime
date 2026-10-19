package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeASRServer stands in for the resident audio.cpp server that a session talks to.
//
// It is a raw TCP server rather than an httptest one on purpose: Go's HTTP server
// buffers a response until the request body ends, so it cannot stand in for a *live*
// endpoint - the thing these tests are about is text arriving while audio is still
// going up. The live path therefore speaks HTTP by hand, and the repair path answers
// the same way, so one address serves both roles as the real service does.
type fakeASRServer struct {
	listener net.Listener
	url      string

	// repairText is what the offline decode answers; repairStatus overrides it with an
	// HTTP error, which is how the "repair failed" path is exercised.
	repairText   string
	repairStatus int
	// deltaEvery is how much audio produces one partial. Small, so a short utterance
	// still gets draft text before it closes.
	deltaEvery time.Duration
	// partial is the text each delta carries.
	partial string
	// silencePartial, when set, is sent instead of partial for a window whose audio is
	// digital silence - what a real model answers with in a quiet room ("。" per window),
	// and the hallucination the session must drop.
	silencePartial string
	// liveFail, when set, ends the live session with an SSE error event carrying
	// this message once the first partial has been sent - a model that dies
	// mid-dictation.
	liveFail string

	mu           sync.Mutex
	repairs      int
	liveSessions int
}

func newFakeASRServer(t *testing.T) *fakeASRServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fake := &fakeASRServer{
		listener:   listener,
		url:        "http://" + listener.Addr().String(),
		repairText: "修复后的句子。",
		deltaEvery: 300 * time.Millisecond,
		partial:    "片段",
	}
	go fake.serve()
	t.Cleanup(func() { listener.Close() })
	return fake
}

func (f *fakeASRServer) repairCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.repairs
}

func (f *fakeASRServer) liveCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.liveSessions
}

func (f *fakeASRServer) serve() {
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeASRServer) handle(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)
	requestLine, err := reader.ReadString('\n')
	if err != nil {
		return
	}
	fields := strings.Fields(requestLine)
	if len(fields) < 2 {
		return
	}
	path := fields[1]
	headers := map[string]string{}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		if strings.TrimSpace(line) == "" {
			break
		}
		key, value, ok := strings.Cut(line, ":")
		if ok {
			headers[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(value)
		}
	}

	switch {
	case strings.HasPrefix(path, "/v1/audio/transcriptions/live"):
		f.handleLive(conn, reader)
	case strings.HasPrefix(path, "/v1/audio/transcriptions"):
		// Drain the multipart body before answering: a client that is still writing
		// would otherwise see its connection reset instead of the response.
		if length, err := strconv.Atoi(headers["content-length"]); err == nil {
			_, _ = io.CopyN(io.Discard, reader, int64(length))
		}
		f.mu.Lock()
		f.repairs++
		f.mu.Unlock()
		if f.repairStatus != 0 {
			body := "repair refused\nsecond line"
			fmt.Fprintf(conn, "HTTP/1.1 %d Refused\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n%s",
				f.repairStatus, len(body), body)
			return
		}
		payload, _ := json.Marshal(map[string]any{
			"text":   f.repairText,
			"timing": map[string]any{"wall_ms": 12, "audio_duration_ms": 1000, "rtf": 0.012},
		})
		fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
			len(payload), payload)
	case strings.HasPrefix(path, "/v1/models"):
		payload := []byte(`{"object":"list","data":[{"id":"stream"},{"id":"refine"}]}`)
		fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
			len(payload), payload)
	default:
		payload := []byte(`{"status":"ok"}`)
		fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
			len(payload), payload)
	}
}

// handleLive answers a streaming session: a delta every deltaEvery of audio, then the
// joined transcript and a terminating chunk when the client ends the audio.
func (f *fakeASRServer) handleLive(conn net.Conn, reader *bufio.Reader) {
	f.mu.Lock()
	f.liveSessions++
	f.mu.Unlock()

	io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")

	var partials []string
	silent := true
	received := 0
	perDelta := int(f.deltaEvery.Seconds() * 16000 * 2)
	for {
		size, err := readHTTPChunkSize(reader)
		if err != nil {
			return
		}
		if size == 0 {
			break
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return
		}
		if _, err := reader.ReadString('\n'); err != nil {
			return
		}
		// Track the loudest byte in the window, so the stand-in can answer the way the real
		// model does: words for speech, punctuation for silence.
		if f.silencePartial != "" {
			silent = true
			for _, b := range payload {
				if b != 0 {
					silent = false
					break
				}
			}
		}
		received += size
		for received/perDelta > len(partials) {
			text := f.partial
			if f.silencePartial != "" && silent {
				text = f.silencePartial
			}
			partials = append(partials, text)
			sendSSE(conn, fmt.Sprintf(`{"type":"transcript.text.delta","delta":%q}`, text))
		}
		if f.liveFail != "" && len(partials) >= 1 {
			// The server's failure shape: a nested reason, then the terminating
			// chunk instead of a done event, so nothing can mistake this for a
			// finished transcript.
			sendSSE(conn, fmt.Sprintf(`{"type":"error","error":{"message":%q}}`, f.liveFail))
			io.WriteString(conn, "0\r\n\r\n")
			return
		}
	}
	sendSSE(conn, fmt.Sprintf(`{"type":"transcript.text.done","text":%q}`, strings.Join(partials, "")))
	sendSSE(conn, "[DONE]")
	io.WriteString(conn, "0\r\n\r\n")
}

func sendSSE(conn net.Conn, payload string) {
	line := "data: " + payload + "\n\n"
	fmt.Fprintf(conn, "%x\r\n%s\r\n", len(line), line)
}

func readHTTPChunkSize(reader *bufio.Reader) (int, error) {
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return 0, err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var size int
		if _, err := fmt.Sscanf(line, "%x", &size); err != nil {
			return 0, err
		}
		return size, nil
	}
}
