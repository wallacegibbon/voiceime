package cli

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wallacegibbon/voiceime/internal/audio"
)

// An empty decode is an answer, not a failure: the audio went in and the model found no
// words in it. `transcribe` is how a deploy proves the path, so an empty stdout with exit 0
// is the one output it must not produce - it is indistinguishable from a crash, and the
// person holding the microphone cannot tell which half of the path is missing.
func TestTranscribeSaysWhenNothingWasRecognized(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"sensevoice-small"}]}`))
		default:
			_, _ = w.Write([]byte(`{"text":"","timing":{"wall_ms":12}}`))
		}
	}))
	defer server.Close()

	configPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(configPath,
		[]byte(fmt.Sprintf("asr_server_url = %q\n", server.URL)), 0o644); err != nil {
		t.Fatal(err)
	}
	// Half a second of digital silence, in the format the tool reads.
	wavPath := filepath.Join(t.TempDir(), "silence.wav")
	silence := make([]byte, audio.SampleRate/2*audio.BytesPerSample)
	if err := os.WriteFile(wavPath, audio.EncodeWAV(silence, audio.SampleRate, 1), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runForTest(t, "--config", configPath, "transcribe", "--file", wavPath)
	if code != ExitOK {
		t.Fatalf("an empty decode exited %d, want %d: the path worked\nstderr: %s", code, ExitOK, stderr)
	}
	if !strings.Contains(stdout, "no words recognized") {
		t.Errorf("an empty decode printed %q, want a line saying no words were recognized", stdout)
	}
	if !strings.Contains(stdout, "0.5s") {
		t.Errorf("the line does not say how much audio was carried: %q", stdout)
	}

	// The machine-readable form already said this, and stays as it was: the empty string
	// and the timing are the finding, and no caller wants to parse a sentence.
	code, stdout, stderr = runForTest(t, "--config", configPath, "--json", "transcribe", "--file", wavPath)
	if code != ExitOK {
		t.Fatalf("--json exited %d, stderr %s", code, stderr)
	}
	payload := decodeOne(t, stdout)["data"].(map[string]any)
	if payload["text"] != "" {
		t.Errorf("text = %v, want the empty string the server returned", payload["text"])
	}
	if seconds, _ := payload["seconds"].(float64); seconds < 0.4 {
		t.Errorf("seconds = %v, want the recording's length: the numbers are how a caller knows the path carried audio", payload["seconds"])
	}
}
