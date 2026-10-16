package asr

import (
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wallacegibbon/voiceime/internal/audio"
)

// The repair pass uploads one utterance and reads back its text. Everything the
// session needs from a server route is in this exchange, so it is worth pinning:
// which fields the request carries, that the audio survives as a WAV container,
// and that the response is parsed rather than guessed at.
func TestServerTranscriberUploadsTheSegmentAndReturnsItsText(t *testing.T) {
	var (
		gotModel    string
		gotLanguage string
		gotFilename string
		gotWAV      []byte
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/audio/transcriptions" {
			t.Errorf("posted to %s, want /v1/audio/transcriptions", r.URL.Path)
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("request was not a multipart form: %v", err)
		}
		gotModel = r.FormValue("model")
		gotLanguage = r.FormValue("language")
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("no file field: %v", err)
		}
		defer file.Close()
		gotFilename = header.Filename
		gotWAV, err = io.ReadAll(file)
		if err != nil {
			t.Fatalf("read upload: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"Some call me nature.","timing":{"wall_ms":1045}}`))
	}))
	defer server.Close()

	pcm := audio.SamplesToBytes([]int16{1, -2, 3, -4, 5, -6})
	transcriber := ServerTranscriber{
		BaseURL:  server.URL,
		Model:    "refine-qwen3",
		Language: "en",
		Timeout:  5 * time.Second,
	}
	text, err := transcriber.Transcribe(context.Background(), pcm)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if text != "Some call me nature." {
		t.Errorf("text = %q, want the server's answer", text)
	}
	if gotModel != "refine-qwen3" {
		t.Errorf("model = %q, want refine-qwen3", gotModel)
	}
	if gotLanguage != "en" {
		t.Errorf("language = %q, want en", gotLanguage)
	}
	if !strings.HasSuffix(gotFilename, ".wav") {
		t.Errorf("filename = %q, want a .wav name", gotFilename)
	}

	// The endpoint decodes a container, so the body has to be one - and it has to
	// carry the utterance, not a truncated or padded copy of it.
	if len(gotWAV) <= len(pcm) {
		t.Fatalf("upload is %d bytes for %d bytes of PCM: no container", len(gotWAV), len(pcm))
	}
	if string(gotWAV[0:4]) != "RIFF" || string(gotWAV[8:12]) != "WAVE" {
		t.Fatalf("upload is not a RIFF/WAVE file")
	}
	if rate := int(binary.LittleEndian.Uint32(gotWAV[24:28])); rate != audio.SampleRate {
		t.Errorf("upload rate = %d, want %d", rate, audio.SampleRate)
	}
	payload := gotWAV[len(gotWAV)-len(pcm):]
	for i := range pcm {
		if payload[i] != pcm[i] {
			t.Fatalf("uploaded PCM differs at byte %d: %d != %d", i, payload[i], pcm[i])
		}
	}
}

// A repair that fails must say why, because the session's only alternative is to
// commit the streaming text and let the user see unrefined words.
func TestServerTranscriberReportsAServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "model is busy\nsecond line", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	transcriber := ServerTranscriber{BaseURL: server.URL, Model: "refine", Timeout: 5 * time.Second}
	_, err := transcriber.Transcribe(context.Background(), audio.SamplesToBytes([]int16{1, 2}))
	if err == nil {
		t.Fatal("Transcribe succeeded against a 503")
	}
	if !strings.Contains(err.Error(), "503") {
		t.Errorf("error %q does not mention the status", err)
	}
	if strings.Contains(err.Error(), "second line") {
		t.Errorf("error %q carries more than the first line of the body", err)
	}
}

// An empty segment is not worth a request, and a server route must not turn it
// into one: the session calls Transcribe for every accepted utterance.
func TestServerTranscriberSkipsEmptyAudio(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer server.Close()

	transcriber := ServerTranscriber{BaseURL: server.URL, Model: "refine"}
	if _, err := transcriber.Transcribe(context.Background(), nil); err != nil {
		t.Fatalf("Transcribe(nil): %v", err)
	}
	if called {
		t.Error("empty audio produced a request")
	}
}

// Check exists to catch the mistake that is otherwise invisible until the first
// utterance: an id the server does not offer. The failure mode it prevents is
// real, because a repair against the *streaming* model's id queues behind the
// live session that holds it.
func TestCheckRejectsAnUnknownModelID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"stream-sense"},{"id":"refine-qwen3"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	ok := ServerTranscriber{BaseURL: server.URL, Model: "refine-qwen3"}
	if err := ok.Check(context.Background()); err != nil {
		t.Fatalf("Check for a configured model: %v", err)
	}

	// The id a user gets wrong by pointing the repair at the streaming entry.
	wrong := ServerTranscriber{BaseURL: server.URL, Model: "stream-sense"}
	if err := wrong.Check(context.Background()); err != nil {
		t.Fatalf("Check should accept a model the server offers: %v", err)
	}

	missing := ServerTranscriber{BaseURL: server.URL, Model: "refine-qwen"}
	err := missing.Check(context.Background())
	if err == nil {
		t.Fatal("Check accepted an id the server does not offer")
	}
	if !strings.Contains(err.Error(), "refine-qwen3") {
		t.Errorf("error %q does not list what the server does offer", err)
	}
}

func TestCheckReportsAnUnreachableServer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := server.URL
	server.Close()

	transcriber := ServerTranscriber{BaseURL: url, Model: "refine"}
	if err := transcriber.Check(context.Background()); err == nil {
		t.Fatal("Check succeeded against a closed server")
	}
}

func TestServerTranscriberRejectsAMissingConfiguration(t *testing.T) {
	cases := []struct {
		name        string
		transcriber ServerTranscriber
		want        string
	}{
		{"no url", ServerTranscriber{Model: "refine"}, "asr_server_url"},
		{"no scheme", ServerTranscriber{BaseURL: "127.0.0.1:8080", Model: "refine"}, "scheme"},
		{"no model", ServerTranscriber{BaseURL: "http://127.0.0.1:8080"}, "asr_server_model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.transcriber.Check(context.Background())
			if err == nil {
				t.Fatal("Check succeeded on an unusable configuration")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}
