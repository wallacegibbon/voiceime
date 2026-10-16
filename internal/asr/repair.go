package asr

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/wallacegibbon/voiceime/internal/audio"
)

// Repairer re-transcribes a finished segment with the offline path.
//
// This is the component that makes "fix the words I already saw" possible. The
// streaming session decodes independent windows and therefore damages text at
// every boundary, while the offline path sees the whole segment with its real
// context. On the target machine a segment of a few seconds costs a fraction of
// a second to re-decode, so the pass is affordable while the user keeps talking.
type Repairer struct {
	Binary string
	// WorkDir mirrors EngineConfig.WorkDir: bundled assets resolve relative to it.
	WorkDir        string
	Model          string
	Backend        string
	Threads        int
	Language       string
	RequestOptions map[string]string
	SessionOptions map[string]string
	// Timeout bounds one repair. A repair that takes longer than the utterance
	// is worse than no repair, so the caller can move on.
	Timeout time.Duration
}

// Transcribe runs one offline decode over the given 16 kHz mono PCM.
func (r Repairer) Transcribe(ctx context.Context, pcm []byte) (string, error) {
	if r.Binary == "" || r.Model == "" {
		return "", fmt.Errorf("repair needs a binary and a model")
	}
	if len(pcm) == 0 {
		return "", nil
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// The offline path takes a file, not stdin: raw PCM is only accepted on the
	// streaming stdin route. A temporary directory keeps concurrent repairs from
	// colliding.
	dir, err := os.MkdirTemp("", "voiceime-repair-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)

	path := dir + "/segment.wav"
	if err := audio.WriteWAV(path, pcm, audio.SampleRate, 1); err != nil {
		return "", err
	}

	args := []string{
		"--task", "asr",
		"--model", r.Model,
		"--backend", r.backend(),
		"--threads", strconv.Itoa(r.threads()),
		"--audio", path,
	}

	// A repair segment is a single utterance that voiceime has already
	// endpointed, so the model's own chunker and its bundled Silero VAD have
	// nothing useful to do. Turning chunking off is not only faster: running the
	// model's VAD used to be the reason repair could fail outright, because that
	// bundled VAD is resolved as a path relative to the process's working
	// directory. Measured on the reference sample, disabling chunking also
	// transcribed one character more accurately than the VAD path did.
	//
	// The same options have to reach a server-side repair, which is why they are
	// built in one place: a multipart transcription upload cannot carry a request
	// options object, so the server route puts these in the model entry's
	// default_request_options instead. Two routes, one decoding contract.
	requestOptions := RepairRequestOptions(r.RequestOptions)
	if r.Language != "" {
		args = append(args, "--language", r.Language)
	}
	for _, key := range sortedKeys(requestOptions) {
		args = append(args, "--request-option", key+"="+requestOptions[key])
	}
	for _, key := range sortedKeys(r.SessionOptions) {
		args = append(args, "--session-option", key+"="+r.SessionOptions[key])
	}

	command := exec.CommandContext(ctx, r.Binary, args...)
	if r.WorkDir != "" {
		command.Dir = r.WorkDir
	}
	output, err := command.Output()
	text := extractTextOutput(string(output))
	if err != nil {
		if text == "" {
			return "", fmt.Errorf("repair decode failed: %v: %s", err, strings.TrimSpace(string(output)))
		}
	}
	return text, nil
}

func (r Repairer) backend() string {
	if r.Backend == "" {
		return "cpu"
	}
	return r.Backend
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

func (r Repairer) threads() int {
	if r.Threads <= 0 {
		return 4
	}
	return r.Threads
}

// extractTextOutput pulls the transcript out of the CLI's report. Everything the
// caller needs is on a `text_output=` line; the surrounding lines are the same
// key=value report the streaming path produces.
func extractTextOutput(output string) string {
	scanner := bufio.NewScanner(strings.NewReader(output))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	text := ""
	for scanner.Scan() {
		if key, value, ok := classifyFinal(scanner.Text()); ok && key == "text_output" {
			text = value
		}
	}
	return text
}
