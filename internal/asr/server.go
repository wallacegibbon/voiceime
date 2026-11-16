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

// ServerTranscriber sends a recording to an endpoint that speaks the transcriptions route.
// The endpoint may be a model on this machine or a provider across a network: nothing below
// knows the difference, and nothing here starts one.
type ServerTranscriber struct {
	BaseURL string
	// Model is the id named in the request. When it is empty the endpoint is asked what it
	// serves: a local setup has one model and no reason to name it twice.
	Model    string
	APIKey   string
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
	base, err := Endpoint(s.BaseURL)
	if err != nil {
		return "", err
	}
	model, err := ModelToUse(ctx, base, s.Model, s.APIKey)
	if err != nil {
		return "", err
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
	if token := strings.TrimSpace(s.APIKey); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}

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

// ModelToUse answers, from the settings alone, which model id a request to this endpoint
// would name: the one it was given, or the one the endpoint serves. `voiceime doctor` asks
// the same question, so the finding is what dictation would do rather than an approximation
// of it.
func ModelToUse(ctx context.Context, endpoint, model, apiKey string) (string, error) {
	base, err := Endpoint(endpoint)
	if err != nil {
		return "", err
	}
	if named := strings.TrimSpace(model); named != "" {
		return named, nil
	}
	return askModels(ctx, base, apiKey)
}

// askModels asks the endpoint which model it serves - the local case: one model, and nothing
// in the settings naming it. A remote endpoint answers /v1/models with a catalogue instead,
// so its setup names the id it wants and never comes here.
func askModels(ctx context.Context, base, apiKey string) (string, error) {
	client := http.DefaultClient
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		return "", err
	}
	if token := strings.TrimSpace(apiKey); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	ask := func(format string, args ...any) error {
		return fmt.Errorf("ask the endpoint which model it serves: "+format, args...)
	}
	response, err := client.Do(request)
	if err != nil {
		return "", ask("%w", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", ask("%w", err)
	}
	if response.StatusCode != http.StatusOK {
		return "", ask("%s", response.Status)
	}
	var listing struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &listing); err != nil {
		return "", ask("%w", err)
	}
	switch len(listing.Data) {
	case 1:
		if listing.Data[0].ID == "" {
			return "", fmt.Errorf("the endpoint serves a model with no id")
		}
		return listing.Data[0].ID, nil
	case 0:
		return "", fmt.Errorf("the endpoint serves no model: for a model on this machine run scripts/asr-service.sh, or name model in the settings")
	default:
		return "", fmt.Errorf("the endpoint lists %d models, so this setup has to name one: model = \"...\" in the settings", len(listing.Data))
	}
}

func (s ServerTranscriber) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return http.DefaultClient
}

func Endpoint(raw string) (string, error) {
	base := strings.TrimSpace(raw)
	if base == "" {
		return "", fmt.Errorf("no server configured: run `voiceime apply`")
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
