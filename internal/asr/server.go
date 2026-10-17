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

// Transcriber is the offline decode the repair pass needs: one finished utterance
// in, its text out.
//
// It is an interface because there are two ways to run that decode and the
// session does not care which it gets. Repairer starts a process per utterance;
// ServerTranscriber talks to a resident audiocpp_server. The difference is not
// cosmetic: a process pays the whole model load every time (measured on the
// reference machine, Qwen3-ASR-0.6B: ~4.4s of setup per invocation, against
// ~1.0s to load it once on the server), which is what makes a large refine model
// unusable on the CLI route and affordable on the server route.
type Transcriber interface {
	Transcribe(ctx context.Context, pcm []byte) (string, error)
}

// ServerTranscriber re-transcribes a finished segment against a running
// audio.cpp server, over the OpenAI-compatible transcription endpoint.
//
// Why a server at all: the repair pass re-decodes every accepted utterance, so a
// refine model that is better than the streaming one is worth having until the
// per-utterance cost of loading it exceeds the utterance itself. The server keeps
// the weights resident, so that cost is paid once. The endpoint takes the WAV as
// a multipart body, so unlike the CLI route this needs no temporary file.
//
// The one hazard is the model lock. The server holds a model for the whole length
// of a request, so if the refine model and the streaming model are the *same*
// entry, a repair queues behind the live dictation that owns it: measured here, a
// 214ms repair waited 10.9s for a 12s dictation to end. Configuring the refine
// model as its own entry is therefore a requirement, not a preference, and
// Check reports the ids the server actually offers so a mistake surfaces at
// startup instead of on the first utterance.
type ServerTranscriber struct {
	// BaseURL is the server root, e.g. http://127.0.0.1:8080.
	BaseURL string
	// Model is the id the server config gave the offline model.
	Model string
	// Language is passed through when set.
	Language string
	// Timeout bounds one repair. A repair that takes longer than the utterance is
	// worse than no repair.
	Timeout time.Duration
	// Client overrides the HTTP client, for tests.
	Client *http.Client
}

var _ Transcriber = ServerTranscriber{}

// Transcribe uploads the segment and returns the text the server decoded.
func (s ServerTranscriber) Transcribe(ctx context.Context, pcm []byte) (string, error) {
	if len(pcm) == 0 {
		return "", nil
	}
	base, err := s.baseURL()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(s.Model) == "" {
		return "", fmt.Errorf("server repair needs asr_server_model: the id the server config gave the offline model")
	}

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	if err := form.WriteField("model", s.Model); err != nil {
		return "", err
	}
	if s.Language != "" {
		if err := form.WriteField("language", s.Language); err != nil {
			return "", err
		}
	}
	part, err := form.CreateFormFile("file", "segment.wav")
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
		return "", fmt.Errorf("server repair: %w", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("server repair: read response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("server repair: %s: %s", response.Status, summarise(payload))
	}

	var decoded struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return "", fmt.Errorf("server repair: decode response: %w", err)
	}
	return decoded.Text, nil
}

// Check reports whether the server is reachable and offers the configured model
// id. A wrong id is the failure mode that would otherwise appear as a repair
// error on the user's first utterance, so it is worth one request at startup.
func (s ServerTranscriber) Check(ctx context.Context) error {
	base, err := s.baseURL()
	if err != nil {
		return err
	}
	if strings.TrimSpace(s.Model) == "" {
		return fmt.Errorf("asr_server_url is set but asr_server_model is empty")
	}

	// /health does not list models, and it does not prove one is loaded, but it
	// does prove something is listening before the models request is attempted.
	if err := s.get(ctx, base+"/health"); err != nil {
		return err
	}
	payload, err := s.getPayload(ctx, base+"/v1/models")
	if err != nil {
		return err
	}
	var listing struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &listing); err != nil {
		return fmt.Errorf("asr server: decode %s/v1/models: %w", base, err)
	}
	available := make([]string, 0, len(listing.Data))
	for _, entry := range listing.Data {
		if entry.ID == s.Model {
			return nil
		}
		available = append(available, entry.ID)
	}
	return fmt.Errorf("asr server has no model %q (it offers: %s)", s.Model, strings.Join(available, ", "))
}

func (s ServerTranscriber) get(ctx context.Context, target string) error {
	_, err := s.getPayload(ctx, target)
	return err
}

func (s ServerTranscriber) getPayload(ctx context.Context, target string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, err
	}
	response, err := s.client().Do(request)
	if err != nil {
		return nil, fmt.Errorf("asr server unreachable: %w", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("asr server: %s: %s", response.Status, summarise(payload))
	}
	return payload, nil
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
		return "", fmt.Errorf("server repair needs asr_server_url")
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return "", fmt.Errorf("asr_server_url %q: %v (needs a scheme and a host, e.g. http://127.0.0.1:8080)", base, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("asr_server_url %q: needs a scheme and a host, e.g. http://127.0.0.1:8080", base)
	}
	return strings.TrimRight(base, "/"), nil
}

// summarise keeps an error body to one short line, so a server's failure page
// cannot flood a log or a notification.
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

// RepairRequestOptions returns the request options the offline repair decode
// needs, with the caller's overlaid on top.
//
// It is exported and shared because the two repair routes have to decode
// identically or a user comparing them is comparing two things: the CLI route
// passes these as --request-option flags, and the server route writes them into
// the model entry's default_request_options, since a multipart transcription
// upload has no field to carry them per request.
func RepairRequestOptions(overrides map[string]string) map[string]string {
	options := map[string]string{"audio_chunk_mode": "none"}
	for key, value := range overrides {
		options[key] = value
	}
	return options
}
