// Package asr is the client side of recognition: one recording, one request.
package asr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/wallacegibbon/voiceime/internal/audio"
)

// Transcriber turns one recording into text.
type Transcriber interface {
	Transcribe(ctx context.Context, pcm []byte) (string, error)
}

// ServerTranscriber sends a recording to a resident audio.cpp server.
type ServerTranscriber struct {
	BaseURL  string
	Model    string
	Language string
	Timeout  time.Duration
	Client   *http.Client
}

var _ Transcriber = ServerTranscriber{}

// Transcribe uploads the recording and returns the text the server decoded.
func (s ServerTranscriber) Transcribe(ctx context.Context, pcm []byte) (string, error) {
	if len(pcm) == 0 {
		return "", nil
	}
	base, err := s.baseURL()
	if err != nil {
		return "", err
	}
	model := strings.TrimSpace(s.Model)
	if model == "" {
		model, err = ModelID(ctx, base, s.client())
		if err != nil {
			return "", err
		}
	}

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("model", model); err != nil {
		return "", err
	}
	if s.Language != "" {
		if err := form.WriteField("language", s.Language); err != nil {
			return "", err
		}
	}
	part, err := form.CreateFormFile("file", "recording.wav")
	if err != nil {
		return "", err
	}
	if _, err := part.Write(audio.EncodeWAV(pcm, audio.SampleRate, 1)); err != nil {
		return "", err
	}
	if err := form.Close(); err != nil {
		return "", err
	}

	timeout := s.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/audio/transcriptions", &body)
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", form.FormDataContentType())

	response, err := s.client().Do(request)
	if err != nil {
		return "", fmt.Errorf("transcribe: %w", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("transcribe: read response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("transcribe: %s: %s", response.Status, summarise(payload))
	}
	var decoded struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return "", fmt.Errorf("transcribe: decode response: %w", err)
	}
	return decoded.Text, nil
}

// ModelID asks the server which entry it serves.
func ModelID(ctx context.Context, base string, client *http.Client) (string, error) {
	if client == nil {
		client = http.DefaultClient
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return "", err
	}
	response, err := client.Do(request)
	if err != nil {
		return "", fmt.Errorf("ask the server which model it serves: %w", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("ask the server which model it serves: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("ask the server which model it serves: %s", response.Status)
	}
	var listing struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &listing); err != nil {
		return "", fmt.Errorf("ask the server which model it serves: %w", err)
	}
	switch len(listing.Data) {
	case 1:
		if listing.Data[0].ID == "" {
			return "", fmt.Errorf("the server serves a model with no id")
		}
		return listing.Data[0].ID, nil
	case 0:
		return "", fmt.Errorf("the server serves no model: run `voiceime server install`")
	default:
		return "", fmt.Errorf("the server serves %d models: this build serves one", len(listing.Data))
	}
}

func (s ServerTranscriber) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return http.DefaultClient
}

func (s ServerTranscriber) baseURL() (string, error) {
	base := strings.TrimSpace(s.BaseURL)
	if base == "" {
		return "", fmt.Errorf("no server configured: run `voiceime server install`")
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("asr_server_url %q: %v (needs a scheme and a host, e.g. http://127.0.0.1:18080)", base, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("asr_server_url %q: needs a scheme and a host, e.g. http://127.0.0.1:18080", base)
	}
	return strings.TrimRight(base, "/"), nil
}

// summarise keeps an error body to one short line.
func summarise(payload []byte) string {
	text := strings.TrimSpace(string(payload))
	if index := strings.IndexAny(text, "\r\n"); index >= 0 {
		text = text[:index]
	}
	if len(text) > 200 {
		text = text[:200] + "..."
	}
	if text == "" {
		return "(no body)"
	}
	return text
}

// RequestOptions are the options every transcription request carries: the whole
// recording in one pass, with the caller's overrides on top.
func RequestOptions(overrides map[string]string) map[string]string {
	options := map[string]string{"audio_chunk_mode": "none"}
	for key, value := range overrides {
		options[key] = value
	}
	return options
}
