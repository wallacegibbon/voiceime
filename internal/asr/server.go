// Package asr is the client side of recognition: one recording, one request.
//
// The address in the settings is that request's whole URL, used exactly as it is written: the
// path belongs to whoever set the endpoint, and the model id is the other thing they know about
// it. Nothing here appends a route or asks an address what it serves.
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

// ServerTranscriber sends a recording to an address that answers a transcriptions request.
// The address may be a model on this machine or a provider across a network: nothing below
// knows the difference, nothing here starts one, and nothing here adds to the address.
type ServerTranscriber struct {
	// BaseURL is the whole address a recording is posted to, used exactly as it is written.
	BaseURL string
	// Model is the id the request names. Nothing here finds it out: which id an address
	// serves is a second thing a person knows about an endpoint, and the settings keep it.
	Model    string
	APIKey   string
	Language string
	Timeout  time.Duration
	Client   *http.Client
}

var _ Transcriber = ServerTranscriber{}

// Transcribe uploads the recording and returns the text the server decoded.
//
// The address is used exactly as it is written and the model id exactly as it is written: both
// are things a person knows about an endpoint and this package cannot find out.
func (s ServerTranscriber) Transcribe(ctx context.Context, pcm []byte) (string, error) {
	if len(pcm) == 0 {
		return "", nil
	}
	address, err := RequestURL(s.BaseURL)
	if err != nil {
		return "", err
	}
	model := strings.TrimSpace(s.Model)
	if model == "" {
		return "", fmt.Errorf("no model is named for %s: the settings need a model = \"<id>\" line saying what that address is asked for", address)
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

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, address, &body)
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

// CheckCredential asks an address, with the credential a request would carry and the model id
// it would be given, whether that credential is accepted. `voiceime doctor` asks it; dictation
// does not, because its own request is the same question and costs one round trip instead of two.
//
// It posts ten milliseconds of silence to the address dictation posts to - that one, not one
// guessed at from its shape - and reads the status of the answer and nothing else. A refusal on
// the address the engine really uses is authoritative in a way an answer on one we invented is
// not. Anything else - 400 "the recording is empty", 415, 200 with an empty text - says the
// address is there and did not refuse the credential.
func CheckCredential(ctx context.Context, address, model, apiKey string) error {
	target, err := RequestURL(address)
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
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, &body)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", form.FormDataContentType())
	// So an address - or a person reading its log - can tell this from a dictation.
	request.Header.Set("X-Voiceime", "credential-probe")
	if token := strings.TrimSpace(apiKey); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("%s cannot be reached: %w", target, err)
	}
	defer response.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if refusedCredential(response) {
		return &CredentialError{Status: response.Status, Missing: strings.TrimSpace(apiKey) == "", Detail: summarise(payload)}
	}
	return nil
}

func (s ServerTranscriber) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return http.DefaultClient
}

// RequestURL is the address a recording is posted to, exactly as it was written.
//
// Nothing is appended to it, and nothing is trimmed off it. A model on this machine and a
// provider across a network put that address different numbers of path segments apart, and the
// path is part of what a person had to find out about the endpoint they were given. Guessing at
// it turns a working address into a 404 that names, in its own body, the path it looked for.
// What is checked here is only the shape a request cannot be sent to at all.
func RequestURL(raw string) (string, error) {
	address := strings.TrimSpace(raw)
	if address == "" {
		return "", fmt.Errorf("no address is configured: asr_server_url names the one a recording is posted to")
	}
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("asr_server_url %q: needs a scheme and a host, e.g. http://127.0.0.1:18080/v1/audio/transcriptions", address)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("asr_server_url %q: %s is not a scheme a request is sent over", address, parsed.Scheme)
	}
	return address, nil
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
