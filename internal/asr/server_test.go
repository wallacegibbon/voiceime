package asr

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wallacegibbon/voiceime/internal/audio"
)

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
		BaseURL:  server.URL + "/v1/audio/transcriptions",
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

// The configured address is used exactly as written: its path is not ours to invent, its port
// and host are not ours to guess, and nothing is appended to it. This is what makes a provider
// whose route is /api/paas/v4/audio/transcriptions as usable as a local one.
func TestTheAddressIsUsedExactlyAsWritten(t *testing.T) {
	var gotPath, gotQuery, gotModel string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		_ = r.ParseMultipartForm(1 << 20)
		gotModel = r.FormValue("model")
		_, _ = io.WriteString(w, `{"text":"what the provider said"}`)
	}))
	defer server.Close()

	address := server.URL + "/api/paas/v4/audio/transcriptions"
	transcriber := ServerTranscriber{BaseURL: address, Model: "glm-asr-2512"}
	text, err := transcriber.Transcribe(context.Background(), audio.SamplesToBytes([]int16{1, 2}))
	if err != nil {
		t.Fatalf("%v", err)
	}
	if text != "what the provider said" {
		t.Errorf("text %q", text)
	}
	if gotPath != "/api/paas/v4/audio/transcriptions" {
		t.Errorf("posted to %s: something was appended to the configured address", gotPath)
	}
	if gotQuery != "" {
		t.Errorf("query %q was added to the address", gotQuery)
	}
	if gotModel != "glm-asr-2512" {
		t.Errorf("named model %q, want the id from the settings", gotModel)
	}
}

// Nothing is asked of an address beyond the request itself: no model list, no discovery, no
// guessing. A setup that names no model is a settings gap, and the message says which key and
// which command, rather than picking one silently.
func TestNoModelNamedSaysSoAndGuessesNothing(t *testing.T) {
	asked := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked++
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"whatever-it-serves"}]}`)
	}))
	defer server.Close()

	_, err := (ServerTranscriber{BaseURL: server.URL}).Transcribe(context.Background(), audio.SamplesToBytes([]int16{1, 2}))
	if err == nil {
		t.Fatal("a request with no model named reported no error")
	}
	if !strings.Contains(err.Error(), `model = "<id>"`) {
		t.Errorf("error %q does not name the line that fixes it", err)
	}
	if asked != 0 {
		t.Errorf("%d requests were made: nothing about an address is guessed at", asked)
	}
}

// The credential question is asked separately, at the configured address, and only the
// status of the answer is read: an endpoint that answers 400, 501 or 200 was reached and did
// not refuse the credential. The two refusals are told apart from each other, because a
// missing key and a wrong key have different fixes, and the endpoint's own reason is kept.
func TestCredentialProbeReadsOnlyTheStatus(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		body     string
		apiKey   string
		wantErr  string
		wantText string
	}{
		{name: "no model list needed, and the route is not there", status: http.StatusNotImplemented, body: "Unsupported method", apiKey: "sk-test"},
		{name: "the endpoint answered, and did not care about the audio", status: http.StatusBadRequest, body: `{"error":{"message":"empty audio"}}`, apiKey: "sk-test"},
		{name: "the endpoint accepted it", status: http.StatusOK, body: `{"text":""}`, apiKey: "sk-test"},
		{
			name: "the key is wrong", status: http.StatusUnauthorized, apiKey: "sk-wrong",
			body: `{"error":{"message":"Incorrect API key provided"}}`, wantErr: "refused the configured api_key", wantText: "Incorrect API key provided",
		},
		{
			name: "there is no key to be wrong", status: http.StatusForbidden, body: `{"error":{"message":"project required"}}`,
			wantErr: "no credential", wantText: "project required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotAuth, gotModel, gotMarker, gotFilename string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/audio/transcriptions" {
					t.Errorf("the credential was probed at %s, not at the configured address", r.URL.Path)
				}
				gotAuth, gotMarker = r.Header.Get("Authorization"), r.Header.Get("X-Voiceime")
				_ = r.ParseMultipartForm(1 << 20)
				gotModel = r.FormValue("model")
				if file, header, err := r.FormFile("file"); err == nil {
					gotFilename = header.Filename
					_ = file.Close()
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()

			err := CheckCredential(context.Background(), server.URL+"/v1/audio/transcriptions", "gpt-4o-mini-transcribe", tc.apiKey)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("an endpoint that did not refuse the credential reported %v", err)
				}
				if gotModel != "gpt-4o-mini-transcribe" {
					t.Errorf("the probe named model %q, want the one the settings name", gotModel)
				}
				if gotMarker != "credential-probe" {
					t.Errorf("the probe is not marked as one (X-Voiceime: %q)", gotMarker)
				}
				if want := "Bearer " + tc.apiKey; gotAuth != want {
					t.Errorf("the probe carried Authorization %q, want %q", gotAuth, want)
				}
				return
			}
			var refused *CredentialError
			if !errors.As(err, &refused) {
				t.Fatalf("error is %T (%v), want a *CredentialError", err, err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not say %q", err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error %q drops what the endpoint said (%q)", err, tc.wantText)
			}
			_ = gotFilename
		})
	}
}

// An endpoint that cannot be reached is a path that does not exist, and that is just as true
// when the model is named: not cross-checking the catalogue is not the same as not asking.
func TestCredentialProbeNamesAnEndpointThatCannotBeReached(t *testing.T) {
	err := CheckCredential(context.Background(), "https://api.does-not-resolve.invalid/v1", "gpt-4o-mini-transcribe", "sk-test")
	if err == nil {
		t.Fatal("an unresolvable endpoint reported no error")
	}
	if !strings.Contains(err.Error(), "cannot be reached") {
		t.Errorf("error %q does not say the endpoint cannot be reached", err)
	}
}

// The transcription request itself reports a refused credential as the same kind of failure,
// with the provider's own reason kept: this is the error an engine log shows a person.
func TestTranscribeReportsARefusedCredentialAsACredentialProblem(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"Incorrect API key provided"}}`)
	}))
	defer server.Close()

	transcriber := ServerTranscriber{BaseURL: server.URL, Model: "gpt-4o-mini-transcribe", APIKey: "sk-wrong"}
	_, err := transcriber.Transcribe(context.Background(), audio.SamplesToBytes([]int16{1, 2}))
	var refused *CredentialError
	if !errors.As(err, &refused) {
		t.Fatalf("error is %T (%v), want a *CredentialError", err, err)
	}
	if !strings.Contains(err.Error(), "api_key") {
		t.Errorf("error %q does not name the setting that fixes it", err)
	}
}
