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
	if refusedCredential(response) {
		return "", fmt.Errorf("transcribe: %w", &CredentialError{
			Status:  response.Status,
			Missing: strings.TrimSpace(s.APIKey) == "",
			Detail:  summarise(payload),
		})
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

// ModelToUse answers, from the settings, which model id a request to this endpoint would
// name: the one it was given, or the one the endpoint serves. `voiceime doctor` asks the same
// question, so the finding is what dictation would do rather than an approximation of it.
//
// A named model is taken from the settings and never cross-checked against the endpoint's
// list: a provider's catalogue is not a promise about what this account is allowed to use, and
// an endpoint with no list at all is a working endpoint. The credential is a separate
// question, and the engine does not ask it - the request that carries it is the answer.
func ModelToUse(ctx context.Context, endpoint, model, apiKey string) (string, error) {
	base, err := Endpoint(endpoint)
	if err != nil {
		return "", err
	}
	named := strings.TrimSpace(model)
	if named == "" {
		return askModels(ctx, base, apiKey)
	}
	return named, nil
}

// CredentialError is the endpoint refusing the credential a request would carry. It is a type
// because its fix is a different command from every other endpoint failure: a settings file,
// not a service.
type CredentialError struct {
	// Status is the status line the endpoint answered with.
	Status string
	// Missing says no api_key is configured, so no header was sent at all.
	Missing bool
	// Detail is what the endpoint said, kept because providers write the reason there
	// ("Incorrect API key provided", "no credentials were provided", ...) rather than
	// leaving it to the status code.
	Detail string
}

func (e *CredentialError) Error() string {
	reason := "the endpoint refused the configured api_key"
	if e.Missing {
		reason = "the endpoint refused a request sent with no credential: this endpoint needs one, api_key in the settings"
	}
	if e.Detail != "" {
		return fmt.Sprintf("%s (%s): %s", reason, e.Status, e.Detail)
	}
	return fmt.Sprintf("%s (%s)", reason, e.Status)
}

// refusedCredential is the one decision every request makes the same way.
func refusedCredential(response *http.Response) bool {
	return response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden
}

// CheckCredential asks an endpoint, with the credential a request would carry and the model
// it would name, whether it accepts that credential. `voiceime doctor` asks it; dictation does
// not, because its own request is the same question and costs one round trip instead of two.
//
// It posts ten milliseconds of silence to the route dictation uses and reads the status of the
// answer and nothing else. That route, not a model list, because a provider's `/v1/models` may
// not exist at all - and a refusal on the route the engine really uses is authoritative in a
// way an answer on a route we invented is not. Anything else - 400 "the recording is empty",
// 415, 200 with an empty text - says the endpoint is there and did not refuse the credential.
func CheckCredential(ctx context.Context, endpoint, model, apiKey string) error {
	base, err := Endpoint(endpoint)
	if err != nil {
		return err
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	silence := make([]byte, audio.SampleRate/100*2) // 10 ms, one channel, 16-bit
	part, err := form.CreateFormFile("file", "probe.wav")
	if err != nil {
		return err
	}
	if _, err := part.Write(audio.EncodeWAV(silence, audio.SampleRate, 1)); err != nil {
		return err
	}
	if err := form.WriteField("model", model); err != nil {
		return err
	}
	if err := form.Close(); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/audio/transcriptions", &body)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", form.FormDataContentType())
	// So an endpoint - or a person reading its log - can tell this from a dictation.
	request.Header.Set("X-Voiceime", "credential-probe")
	if token := strings.TrimSpace(apiKey); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("%s cannot be reached: %w", base, err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if refusedCredential(response) {
		return &CredentialError{Status: response.Status, Missing: strings.TrimSpace(apiKey) == "", Detail: summarise(payload)}
	}
	return nil
}

// modelsRequest is the one request both questions above ask: the model list, with the
// credential a real request would carry. The caller reads the body and closes both it and
// the cancel.
func modelsRequest(ctx context.Context, base, apiKey string) (*http.Response, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	if token := strings.TrimSpace(apiKey); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	return response, cancel, nil
}

// askModels asks the endpoint which model it serves - the local case: one model, and nothing
// in the settings naming it. A remote endpoint answers /v1/models with a catalogue instead,
// so its setup names the id it wants and never comes here.
func askModels(ctx context.Context, base, apiKey string) (string, error) {
	response, cancel, err := modelsRequest(ctx, base, apiKey)
	if err != nil {
		return "", fmt.Errorf("ask the endpoint which model it serves: %w", err)
	}
	defer cancel()
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("ask the endpoint which model it serves: %w", err)
	}
	switch response.StatusCode {
	case http.StatusOK:
	default:
		if refusedCredential(response) {
			return "", &CredentialError{Status: response.Status, Missing: strings.TrimSpace(apiKey) == "", Detail: summarise(payload)}
		}
		return "", fmt.Errorf("ask the endpoint which model it serves: %s", response.Status)
	}
	var listing struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &listing); err != nil {
		return "", fmt.Errorf("ask the endpoint which model it serves: %w", err)
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
