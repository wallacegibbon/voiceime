package asr

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wallacegibbon/voiceime/internal/audio"
)

// fakeLiveServer is a stand-in for audio.cpp's live endpoint, written as a raw
// listener rather than with net/http on purpose: Go's HTTP server buffers the
// response until the request body ends, so an httptest double cannot show whether
// the client really handles text arriving *while* audio is still going up. This one
// speaks the same HTTP the real server does - headers immediately, chunked request
// body in, chunked SSE out - and can therefore be trusted about the thing under test.
type fakeLiveServer struct {
	listener net.Listener
	url      string
	deltas   []string
	// status, when set, is answered instead of a session: the refusal path.
	status int
	body   string
	// closeAfterDeltas drops the connection abruptly, as a dying service would.
	closeAfterDeltas int
	// failWith, when set, ends the session with an SSE error event carrying this
	// message in audio.cpp's shape - nested under "error" - and no done event.
	failWith string

	mu       sync.Mutex
	requests []string
}

func newFakeLive(t *testing.T, deltas ...string) *fakeLiveServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	fake := &fakeLiveServer{
		listener:         listener,
		url:              "http://" + listener.Addr().String(),
		deltas:           deltas,
		closeAfterDeltas: -1,
	}
	go fake.serve()
	t.Cleanup(func() { listener.Close() })
	return fake
}

func (f *fakeLiveServer) serve() {
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeLiveServer) handle(conn net.Conn) {
	defer conn.Close()
	reader := bufio.NewReader(conn)

	requestLine, err := reader.ReadString('\n')
	if err != nil {
		return
	}
	f.mu.Lock()
	f.requests = append(f.requests, strings.TrimSpace(requestLine))
	f.mu.Unlock()
	for {
		line, err := reader.ReadString('\n')
		if err != nil || strings.TrimSpace(line) == "" {
			break
		}
	}

	if f.status != 0 {
		fmt.Fprintf(conn, "HTTP/1.1 %d Refused\r\nContent-Type: text/plain\r\nContent-Length: %d\r\n\r\n%s",
			f.status, len(f.body), f.body)
		return
	}

	// Headers first, before any audio: the real server answers 200 at 0.00s with no
	// body sent, which is what lets the client return from opening the session.
	io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n")

	second := audio.SampleRate * audio.BytesPerSample
	received, sent := 0, 0
	for {
		size, err := readChunkSize(reader)
		if err != nil {
			return
		}
		if size == 0 {
			break
		}
		if _, err := io.CopyN(io.Discard, reader, int64(size)); err != nil {
			return
		}
		if _, err := reader.ReadString('\n'); err != nil { // chunk CRLF
			return
		}
		received += size
		for received/second > sent && sent < len(f.deltas) {
			if f.closeAfterDeltas >= 0 && sent >= f.closeAfterDeltas {
				return
			}
			f.send(conn, fmt.Sprintf("data: {\"type\":\"transcript.text.delta\",\"delta\":%q}\n\n", f.deltas[sent]))
			sent++
		}
	}
	if f.failWith != "" {
		// The shape the server actually uses: the reason is nested under "error",
		// and no done event follows - a truncated transcript must not be
		// indistinguishable from the speaker stopping.
		f.send(conn, fmt.Sprintf("data: {\"type\":\"error\",\"error\":{\"message\":%q}}\n\n", f.failWith))
		io.WriteString(conn, "0\r\n\r\n")
		return
	}
	joined := strings.Join(f.deltas[:sent], "")
	f.send(conn, fmt.Sprintf("data: {\"type\":\"transcript.text.done\",\"text\":%q}\n\n", joined))
	f.send(conn, "data: [DONE]\n\n")
	io.WriteString(conn, "0\r\n\r\n")
}

func (f *fakeLiveServer) send(conn net.Conn, payload string) {
	fmt.Fprintf(conn, "%x\r\n%s\r\n", len(payload), payload)
}

func readChunkSize(reader *bufio.Reader) (int, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return 0, err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return readChunkSize(reader)
	}
	var size int
	if _, err := fmt.Sscanf(line, "%x", &size); err != nil {
		return 0, err
	}
	return size, nil
}

func (f *fakeLiveServer) requestLines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func silence(seconds int) []byte {
	samples := make([]int16, audio.SampleRate*seconds)
	return audio.SamplesToBytes(samples)
}

// The session surface has to match the CLI adapter's: audio in, partial events out,
// and the final text available after Close. This is what lets the daemon swap one for
// the other without knowing.
func TestStreamDeliversPartialsAndFinalText(t *testing.T) {
	server := newFakeLive(t, "你好", "，世界")

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream, err := OpenStream(ctx, StreamConfig{BaseURL: server.url, Model: "stream-sense"})
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}

	var partials []string
	collected := make(chan struct{})
	go func() {
		defer close(collected)
		for event := range stream.Events() {
			if event.Kind == EventPartial {
				partials = append(partials, event.Text)
			}
		}
	}()

	for i := 0; i < 2; i++ {
		if err := stream.Write(silence(1)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	if got := stream.FedSamples(); got != int64(2*audio.SampleRate) {
		t.Errorf("FedSamples = %d, want %d", got, 2*audio.SampleRate)
	}
	result, err := stream.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	<-collected

	if len(partials) != 2 || partials[0] != "你好" || partials[1] != "，世界" {
		t.Errorf("partials = %v, want the server's deltas in order", partials)
	}
	if result.Text != "你好，世界" {
		t.Errorf("final text = %q, want the server's joined transcript", result.Text)
	}
	if result.AudioFed < 2*time.Second {
		t.Errorf("AudioFed = %v, want about 2s", result.AudioFed)
	}
	if lines := server.requestLines(); len(lines) != 1 || !strings.Contains(lines[0], "model=stream-sense") {
		t.Errorf("request line(s) = %v, want one carrying the model id", lines)
	}
}

// Partial text has to arrive while the audio is still going up, not after the body
// ends: that is the difference between streaming and an upload, and the reason the
// daemon can show a draft at all.
func TestStreamDeliversDeltasDuringCapture(t *testing.T) {
	server := newFakeLive(t, "第一段", "第二段")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream, err := OpenStream(ctx, StreamConfig{BaseURL: server.url, Model: "stream-sense"})
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}

	if err := stream.Write(silence(1)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	select {
	case event := <-stream.Events():
		if event.Kind != EventPartial || event.Text != "第一段" {
			t.Fatalf("first event = %+v, want the first delta", event)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no delta arrived before the audio was closed")
	}
	if _, err := stream.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// The failure a user will actually hit: the service is not running. Nothing else in a
// session reports it, so the message has to say what to do.
func TestOpenStreamExplainsAMissingServer(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	address := listener.Addr().String()
	listener.Close()

	_, err = OpenStream(context.Background(), StreamConfig{BaseURL: "http://" + address, Model: "stream-sense"})
	if err == nil {
		t.Fatal("OpenStream succeeded with nothing listening")
	}
	if !strings.Contains(err.Error(), "voiceime-asr-server") {
		t.Errorf("error %q does not name the service to start", err)
	}
}

func TestOpenStreamNeedsConfiguration(t *testing.T) {
	if _, err := OpenStream(context.Background(), StreamConfig{Model: "stream-sense"}); err == nil {
		t.Error("OpenStream accepted an empty server URL")
	}
	if _, err := OpenStream(context.Background(), StreamConfig{BaseURL: "http://127.0.0.1:1"}); err == nil {
		t.Error("OpenStream accepted an empty model id")
	}
}

// A refused session is reported before any audio is fed, with the status and the
// server's own first line.
func TestOpenStreamReportsARefusal(t *testing.T) {
	server := newFakeLive(t)
	server.status = 400
	server.body = "live transcription requires chunked body\nsecond line"

	_, err := OpenStream(context.Background(), StreamConfig{BaseURL: server.url, Model: "stream-sense"})
	if err == nil {
		t.Fatal("OpenStream succeeded against a 400")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error %q does not mention the status", err)
	}
	if !strings.Contains(err.Error(), "chunked body") {
		t.Errorf("error %q does not carry the server's reason", err)
	}
	if strings.Contains(err.Error(), "second line") {
		t.Errorf("error %q carries more than the first line", err)
	}
}

// A service that dies mid-session must end the session rather than hang: the user has
// to see that dictation stopped, and the loop that feeds audio has to be released.
func TestStreamEndsWhenTheServerDies(t *testing.T) {
	server := newFakeLive(t, "一段", "二段")
	server.closeAfterDeltas = 1

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream, err := OpenStream(ctx, StreamConfig{BaseURL: server.url, Model: "stream-sense"})
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	go func() {
		for {
			if err := stream.Write(silence(1)); err != nil {
				return
			}
		}
	}()

	closed := make(chan struct{})
	go func() {
		defer close(closed)
		for range stream.Events() {
		}
	}()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("the session did not end when the server went away")
	}
	result, err := stream.Close()
	if result.ProcessErr == nil {
		t.Error("a session that lost its server reported no error")
	}
	// The error has to reach the caller too: the daemon decides with it whether the
	// remaining draft may be committed, and a front end with only a Result field it
	// never reads reports success for a truncated transcript.
	if err == nil {
		t.Fatal("Close returned no error for a session whose server went away")
	}
	if !strings.Contains(err.Error(), "streaming server") {
		t.Errorf("the failure %q does not say where it came from", err)
	}
}

// A stream the server ends with an error event must fail the session with the
// server's own reason.
//
// The reason is nested under "error" ({"type":"error","error":{"message":...}}).
// Reading only the top level produced a failure with no cause, and the sentence a
// user needs - which model, which resource, which asset - was the part that got
// dropped.
func TestStreamCarriesTheServersErrorReason(t *testing.T) {
	server := newFakeLive(t, "一段")
	server.failWith = "model died: cuda OOM"

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	stream, err := OpenStream(ctx, StreamConfig{BaseURL: server.url, Model: "stream-sense"})
	if err != nil {
		t.Fatalf("OpenStream: %v", err)
	}
	go func() {
		for range stream.Events() {
		}
	}()
	if err := stream.Write(silence(1)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	result, closeErr := stream.Close()
	if closeErr == nil {
		t.Fatal("a session the server ended with an error event reported success")
	}
	if !strings.Contains(closeErr.Error(), "cuda OOM") {
		t.Errorf("failure %q does not carry the server's reason", closeErr)
	}
	if result.ProcessErr == nil {
		t.Error("Result.ProcessErr is nil for a failed session, so a caller that reads only the result sees success")
	}
	// No done event arrived, so there is no final transcript to report. An empty
	// text is right here; the failure is what the caller acts on.
	if result.Text != "" {
		t.Errorf("final text = %q for a session that never finished, want empty", result.Text)
	}
}

// buffer helper used by the sink test above.
var _ = binary.LittleEndian
