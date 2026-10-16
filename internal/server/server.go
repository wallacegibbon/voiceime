// Package server starts and stops the resident audio.cpp server that an offline
// decode can run through.
//
// It exists because the alternative - a process per utterance - pays the whole
// model load every time. Measured on the reference machine with Qwen3-ASR-0.6B:
// ~4.4s of CPU per fresh process whatever the audio length, against ~1.4s once,
// which is the difference between a large refine model being affordable and not.
//
// A server is only worth it if the weights stay resident, and keeping them
// resident for the life of the machine is a memory cost on a machine that is not
// always dictating (1.9GB with both ASR entries loaded here). So the lifetime it
// offers is the dictation session's: spawned on demand, stopped at the end. A
// server that is already listening when a session starts is used as it is and left
// running - it belongs to whoever started it.
package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wallacegibbon/voiceime/internal/audio"
)

// DefaultID is the model id the generated config gives the offline entry, and so
// the id a repair asks for when voiceime started the server itself.
const DefaultID = "refine"

// Spec is what voiceime has to decide before a server can be started.
type Spec struct {
	// Binary resolves the audiocpp_server executable. It is a function because it
	// is only needed when a server has to be started: a user who runs their own
	// server needs no local build, and must not be refused for lacking one.
	Binary func() (string, error)
	// WorkDir mirrors the CLI's: audio.cpp resolves some bundled assets relative
	// to the process's working directory.
	WorkDir string
	// Model is the GGUF the offline decode should use.
	Model string
	// Family resolves that model's audio.cpp family, which the server requires. It
	// is a function for the same reason as Binary, and because it costs an
	// inspection of the file rather than being a value voiceime already holds.
	Family func() (string, error)
	// ModelID is the id the generated config gives it.
	ModelID string
	Host    string
	Port    int
	Backend string
	Threads int
	// RequestOptions are the options every request to this model should carry.
	// They cannot ride a multipart transcription upload - only the JSON route
	// reads an options object - so they belong in the config's
	// default_request_options, which is also where the CLI repair route's
	// --request-option values map to.
	RequestOptions map[string]string
	// ConfigPath and LogPath are written by this package; both are artefacts a
	// user can read when a start fails.
	ConfigPath string
	LogPath    string
	// Autostart says whether to start a server when nothing is listening.
	Autostart bool
	// StartTimeout bounds the wait for a freshly started server to answer /health.
	StartTimeout time.Duration
	// IdleUnloadMS releases the weights after this long without use. It does not
	// end the process, so it is the safety net for a server that outlives its
	// session - a parent killed outright cannot run its Stop.
	IdleUnloadMS int
}

// Instance is a server a repair can be sent to.
type Instance struct {
	BaseURL string
	ModelID string

	owned    bool
	cmd      *exec.Cmd
	logPath  string
	stopOnce sync.Once
}

// Ensure returns a server to repair through: the one already listening on the
// spec's address, or one this call starts.
func Ensure(ctx context.Context, spec Spec) (*Instance, error) {
	base, err := spec.baseURL()
	if err != nil {
		return nil, err
	}
	if err := probe(ctx, base); err == nil {
		return &Instance{BaseURL: base, ModelID: spec.ModelID}, nil
	}
	if !spec.Autostart {
		return nil, fmt.Errorf("nothing is listening at %s and asr_server_autostart is off", base)
	}
	if err := spec.validate(); err != nil {
		return nil, err
	}
	// Resolved only once a server actually has to be started: both of these can
	// fail, and neither should stand between a user and the server they already run.
	binary, err := spec.resolveBinary()
	if err != nil {
		return nil, err
	}
	family, err := spec.resolveFamily()
	if err != nil {
		return nil, err
	}
	if err := writeConfig(spec, family); err != nil {
		return nil, err
	}

	logFile, err := os.OpenFile(spec.LogPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", spec.LogPath, err)
	}
	defer logFile.Close()

	// --log puts the report on stdout/stderr, which is the same channel a fatal
	// startup error uses ("audiocpp_server failed: missing required json key:
	// family"), so one file holds both and a failed start explains itself.
	cmd := exec.Command(binary, "--config", spec.ConfigPath, "--log")
	cmd.Dir = spec.WorkDir
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", binary, err)
	}

	instance := &Instance{
		BaseURL: base,
		ModelID: spec.ModelID,
		owned:   true,
		cmd:     cmd,
		logPath: spec.LogPath,
	}
	timeout := spec.StartTimeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	deadline := time.Now().Add(timeout)
	for {
		if err := probe(ctx, base); err == nil {
			return instance, nil
		}
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			instance.reap()
			return nil, fmt.Errorf("%s exited while starting: %s", filepath.Base(binary), instance.logTail())
		}
		if ctx.Err() != nil {
			instance.Stop()
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			instance.Stop()
			return nil, fmt.Errorf("no server answered at %s within %v: %s", base, timeout, instance.logTail())
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Stop ends a server this process started. A server that was already listening
// belongs to whoever started it, so it is left alone. Safe to call more than once.
func (i *Instance) Stop() {
	if i == nil || !i.owned || i.cmd == nil || i.cmd.Process == nil {
		return
	}
	i.stopOnce.Do(func() {
		done := make(chan struct{})
		go func() {
			defer close(done)
			i.reap()
		}()
		// SIGTERM first: the server releases its models and exits on its own. The
		// kill is for a server that is wedged, and only after it has had a moment.
		_ = i.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = i.cmd.Process.Kill()
			<-done
		}
	})
}

// Owned reports whether this process started the server, which is what decides
// whether it may stop it.
func (i *Instance) Owned() bool { return i != nil && i.owned }

// Warm decodes a fraction of a second of audio so the model is loaded before the
// first real utterance, which is the whole reason a server is started as dictation
// begins rather than on the first repair.
//
// It is best effort by design: a server that refuses this probe audio is still
// usable, and the first repair's own failure is the better signal about that.
func (i *Instance) Warm(ctx context.Context, timeout time.Duration) error {
	if i == nil {
		return fmt.Errorf("no server to warm")
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var payload bytes.Buffer
	form := multipart.NewWriter(&payload)
	if err := form.WriteField("model", i.ModelID); err != nil {
		return err
	}
	part, err := form.CreateFormFile("file", "warmup.wav")
	if err != nil {
		return err
	}
	if _, err := part.Write(warmWAV()); err != nil {
		return err
	}
	if err := form.Close(); err != nil {
		return err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		i.BaseURL+"/v1/audio/transcriptions", &payload)
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", form.FormDataContentType())
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("warm up %s: %w", i.ModelID, err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<20))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("warm up %s: %s", i.ModelID, response.Status)
	}
	return nil
}

// warmWAV is a fifth of a second of quiet tone. A tone rather than digital
// silence because silence is exactly the input some recognisers refuse, and the
// point here is only to make the model load and run a graph.
func warmWAV() []byte {
	samples := make([]int16, audio.SampleRate/5)
	for index := range samples {
		samples[index] = int16(3000 * math.Sin(2*math.Pi*300*float64(index)/float64(audio.SampleRate)))
	}
	return audio.EncodeWAV(audio.SamplesToBytes(samples), audio.SampleRate, 1)
}

func (i *Instance) reap() {
	if i.cmd != nil {
		_ = i.cmd.Wait()
	}
}

// logTail returns the last few lines of the server's log, for a start that failed:
// the reason is in there and nowhere else.
func (i *Instance) logTail() string {
	if i == nil || i.logPath == "" {
		return "no log"
	}
	raw, err := os.ReadFile(i.logPath)
	if err != nil {
		return fmt.Sprintf("log unreadable: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) > 8 {
		lines = lines[len(lines)-8:]
	}
	if text := strings.TrimSpace(strings.Join(lines, " | ")); text != "" {
		return text
	}
	return "log is empty"
}

func (s Spec) baseURL() (string, error) {
	if s.Host == "" || s.Port == 0 {
		return "", fmt.Errorf("server needs a host and a port")
	}
	return fmt.Sprintf("http://%s:%d", s.Host, s.Port), nil
}

func (s Spec) validate() error {
	switch {
	case s.Model == "":
		return fmt.Errorf("asr_server_autostart needs a refine model to serve")
	case s.ConfigPath == "":
		return fmt.Errorf("no config path to write")
	case s.LogPath == "":
		return fmt.Errorf("no log path to write")
	case !isLocalHost(s.Host):
		return fmt.Errorf("refusing to start a server for %s: autostart is for this machine", s.Host)
	}
	return nil
}

func (s Spec) resolveBinary() (string, error) {
	if s.Binary == nil {
		return "", fmt.Errorf("no audiocpp_server binary configured")
	}
	binary, err := s.Binary()
	if err != nil {
		return "", err
	}
	if binary == "" {
		return "", fmt.Errorf("no audiocpp_server binary found")
	}
	return binary, nil
}

func (s Spec) resolveFamily() (string, error) {
	if s.Family == nil {
		return "", fmt.Errorf("the refine model's audio.cpp family is unknown, and the server requires it")
	}
	family, err := s.Family()
	if err != nil {
		return "", err
	}
	if family == "" {
		return "", fmt.Errorf("the refine model's audio.cpp family is unknown, and the server requires it")
	}
	return family, nil
}

// isLocalHost reports whether an address is this machine, which is what makes
// starting a process for it sensible rather than surprising.
func isLocalHost(host string) bool {
	switch host {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	if parsed := net.ParseIP(host); parsed != nil {
		return parsed.IsLoopback()
	}
	return false
}

// ParseURL splits the configured server address into the host and port a Spec
// needs.
func ParseURL(raw string) (host string, port int, err error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", 0, fmt.Errorf("asr_server_url %q: %v (needs a scheme and a host, e.g. http://127.0.0.1:8080)", raw, err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", 0, fmt.Errorf("asr_server_url %q: needs a scheme and a host, e.g. http://127.0.0.1:8080", raw)
	}
	host = parsed.Hostname()
	if host == "" {
		return "", 0, fmt.Errorf("asr_server_url %q: no host", raw)
	}
	if parsed.Port() == "" {
		if parsed.Scheme == "https" {
			return host, 443, nil
		}
		return host, 80, nil
	}
	port, err = net.LookupPort("tcp", parsed.Port())
	if err != nil {
		return "", 0, fmt.Errorf("asr_server_url %q: %v", raw, err)
	}
	return host, port, nil
}

func probe(ctx context.Context, base string) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/health", nil)
	if err != nil {
		return err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1<<16))
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("%s/health: %s", base, response.Status)
	}
	return nil
}

// wireConfig is the server config this package writes. Only the offline entry is
// needed: the streaming layer stays on the CLI's stdin route, so a server started
// here never holds a second copy of a model to serve partials with.
type wireConfig struct {
	Host            string      `json:"host"`
	Port            int         `json:"port"`
	Backend         string      `json:"backend"`
	Threads         int         `json:"threads"`
	LazyLoad        bool        `json:"lazy_load"`
	MaxLoadedModels int         `json:"max_loaded_models"`
	IdleUnloadMS    int         `json:"idle_unload_ms,omitempty"`
	UI              bool        `json:"ui"`
	UIManagement    bool        `json:"ui_management"`
	Models          []wireModel `json:"models"`
}

type wireModel struct {
	ID                    string            `json:"id"`
	Family                string            `json:"family"`
	Path                  string            `json:"path"`
	Task                  string            `json:"task"`
	Mode                  string            `json:"mode"`
	DefaultRequestOptions map[string]string `json:"default_request_options,omitempty"`
}

func writeConfig(spec Spec, family string) error {
	modelID := spec.ModelID
	if modelID == "" {
		modelID = DefaultID
	}
	backend := spec.Backend
	if backend == "" {
		backend = "cpu"
	}
	config := wireConfig{
		Host:            "127.0.0.1",
		Port:            spec.Port,
		Backend:         backend,
		Threads:         spec.Threads,
		LazyLoad:        true,
		MaxLoadedModels: 0,
		IdleUnloadMS:    spec.IdleUnloadMS,
		Models: []wireModel{{
			ID:                    modelID,
			Family:                family,
			Path:                  spec.Model,
			Task:                  "asr",
			Mode:                  "offline",
			DefaultRequestOptions: spec.RequestOptions,
		}},
	}
	encoded, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	if dir := filepath.Dir(spec.ConfigPath); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	return os.WriteFile(spec.ConfigPath, append(encoded, '\n'), 0o644)
}
