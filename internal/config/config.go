// Package config holds every user-visible setting, the XDG paths they live in,
// and the discovery rules for the external programs voiceime drives.
//
// There is deliberately no GUI anywhere in this project: the configuration file
// is the single source of truth, and every operation that would have been a
// dialog is a subcommand with machine-readable output. That is a hard design
// constraint, not a preference - it means an automated agent can install,
// configure and repair voiceime by reading docs/AGENT.md and running commands,
// with no human in the loop. See docs/decisions.md (D1, D2).
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// AppName is the directory name under every XDG base directory.
const AppName = "voiceime"

// DefaultModelsDir is where model packages live unless told otherwise. It
// matches the convention used by audio.cpp's own model manager so models
// downloaded by either tool are found by the other.
const DefaultModelsDir = "~/models"

// DefaultDownloadEndpoint is the host voiceime ships pointing downloads at. It
// is an ordinary endpoint, not a special case: https://huggingface.co is equally
// valid and is exactly what setting the value to it means. The default is a
// mirror rather than the upstream project because it is the endpoint that works
// from networks where huggingface.co is unreachable (measured on the reference
// machine: a direct request to huggingface.co times out there).
const DefaultDownloadEndpoint = "https://hf-mirror.com"

// TargetRate is the only capture rate the pipeline supports. The ASR models'
// bundled Silero VAD rejects anything else, so resampling happens in the capture
// path (PipeWire does it server-side) rather than being papered over later.
const TargetRate = 16000

// Config is the whole settings surface. Field order is the order they are
// written to the file, and the `toml` tag is the key. `voiceime config set` and
// `voiceime capabilities` are both driven off this struct by reflection, so
// adding a field here automatically exposes it to agents - there is no parallel
// schema to keep in sync.
type Config struct {
	// Paths and external programs.
	ModelsDir    string `toml:"models_dir" desc:"Directory scanned for model packages; downloads are written here."`
	AudioCPPDir  string `toml:"audio_cpp_dir" desc:"audio.cpp checkout used to locate its CLI and model manager."`
	ASRBin       string `toml:"asr_bin" desc:"Explicit path to audiocpp_cli; empty means discover."`
	ModelManager string `toml:"model_manager" desc:"Explicit path to tools/model_manager_v2.py; empty means discover."`

	// Inference.
	Backend string `toml:"backend" desc:"audio.cpp backend: cpu, cuda, vulkan, hip, metal."`
	Threads int    `toml:"threads" desc:"Worker threads handed to audio.cpp."`

	// Model selection by absolute path; empty means pick automatically from the
	// models that are installed.
	StreamingModel string `toml:"streaming_model" desc:"Model used for live partials; empty picks a default."`
	RefineModel    string `toml:"refine_model" desc:"Model used to re-transcribe a finished segment; empty reuses the streaming model."`

	Language     string  `toml:"language" desc:"Recognition language code, e.g. zh or en."`
	ChunkSeconds float64 `toml:"chunk_seconds" desc:"Streaming window length. Lower cuts latency, higher improves accuracy."`

	// Repair policy. The two-layer design is what makes 'correct earlier words'
	// possible at all: window partials are provisional, and the offline
	// re-decode of a finished segment is what actually corrects them.
	RefineOnSegmentEnd  bool    `toml:"refine_on_segment_end" desc:"Re-transcribe a completed segment with the offline path."`
	MaxUtteranceSeconds float64 `toml:"max_utterance_seconds" desc:"Force a segment boundary after this many seconds."`

	// Endpointing.
	EndpointSilenceMS   int `toml:"endpoint_silence_ms" desc:"Silence after speech before a segment is closed."`
	EndpointMinSpeechMS int `toml:"endpoint_min_speech_ms" desc:"Speech shorter than this is treated as noise."`
	SilenceRMSThreshold int `toml:"silence_rms_threshold" desc:"RMS amplitude below which a frame counts as silence."`

	// Capture.
	CaptureSource string `toml:"capture_source" desc:"PipeWire source name; empty uses the default input."`
	CaptureRate   int    `toml:"capture_rate" desc:"Capture rate; must stay 16000, the models' VAD rejects other rates."`

	// Downloads. The value is an endpoint URL, not a preset name: every package
	// audio.cpp's registry ships downloads over Hugging Face's protocol, so any
	// HF-compatible host works (hf-mirror.com, huggingface.co, a LAN cache) and
	// a preset table would only be a shorter way of writing the same two URLs.
	// It is exported to the model manager as HF_ENDPOINT.
	DownloadMirror string `toml:"download_mirror" desc:"HF-protocol endpoint URL model downloads go through, e.g. https://hf-mirror.com."`

	// Integration.
	Hotkey string `toml:"hotkey" desc:"Shortcut held down to dictate, e.g. <Control>apostrophe."`
	// IBus's global-shortcut property carries a consumer byte, which it echoes
	// back on every event so a listener can tell its own events from another
	// consumer's (the panel's emoji shortcut, for one).
	HotkeyShortcutType int    `toml:"hotkey_shortcut_type" desc:"Consumer byte for the IBus global shortcut registration."`
	IBusEngineName     string `toml:"ibus_engine_name" desc:"IBus engine name registered for text delivery."`

	// Escape hatches passed straight through to audio.cpp. Keeping these maps
	// means a new framework option becomes usable without a voiceime release.
	SessionOptions map[string]string `toml:"session_options" desc:"Passed to audiocpp_cli as --session-option key=value."`
	RequestOptions map[string]string `toml:"request_options" desc:"Passed to audiocpp_cli as --request-option key=value."`
}

// Default returns the settings tuned for a stock GNOME/Wayland machine with
// audio.cpp built from source.
func Default() *Config {
	return &Config{
		ModelsDir:   DefaultModelsDir,
		AudioCPPDir: "~/playground/audio.cpp",
		Backend:     "cpu",
		Threads:     4,
		Language:    "zh",
		// 3s is the smallest window measured to keep Chinese output usable with
		// SenseVoice's independent-window streaming; see docs/measurements.md.
		ChunkSeconds:        3.0,
		RefineOnSegmentEnd:  true,
		MaxUtteranceSeconds: 60,
		EndpointSilenceMS:   800,
		EndpointMinSpeechMS: 300,
		// 800 is the floor, not a fixed threshold: the detector raises its
		// threshold to twice the noise floor it measures in the stream. The value
		// is the measured one for the reference room, whose idle noise reaches a
		// 20ms-frame p90 of 436 RMS (docs/measurements.md) - the older 350 sat
		// inside that distribution, which is what "it never stops listening"
		// looked like. Lower it for a quiet speaker at a distance; the failure
		// mode then is that quiet speech is heard as silence.
		SilenceRMSThreshold: 800,
		CaptureRate:         TargetRate,
		DownloadMirror:      DefaultDownloadEndpoint,
		// A function key, not a modifier combination: IBus's global-shortcut
		// mechanism consumes the key's release, so a shortcut containing a modifier
		// leaves that modifier latched in the focused application (measured - Ctrl
		// stayed down until tapped again, even when the engine switch was delayed by
		// 300ms). A plain function key has no modifier state to latch.
		Hotkey:             "<F9>",
		HotkeyShortcutType: 1,
		IBusEngineName:     "voiceime",
		SessionOptions:     map[string]string{},
		RequestOptions:     map[string]string{},
	}
}

// --------------------------------------------------------------------------- //
// XDG locations
// --------------------------------------------------------------------------- //

func xdg(env, fallback string) string {
	if value := os.Getenv(env); value != "" {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return fallback
	}
	return filepath.Join(home, fallback)
}

// ConfigDir is ~/.config/voiceime by default.
func ConfigDir() string { return filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), AppName) }

// CacheDir holds the model inspection cache and other disposable state.
func CacheDir() string { return filepath.Join(xdg("XDG_CACHE_HOME", ".cache"), AppName) }

// DataDir holds installed component definitions such as the IBus XML.
func DataDir() string {
	return filepath.Join(xdg("XDG_DATA_HOME", filepath.Join(".local", "share")), AppName)
}

// RuntimeDir holds the daemon socket. XDG_RUNTIME_DIR is per-user, mode 0700 and
// cleared at logout - exactly the lifetime a socket wants. The fallback keeps
// the daemon usable from a plain shell with no session.
func RuntimeDir() string {
	if base := os.Getenv("XDG_RUNTIME_DIR"); base != "" {
		if info, err := os.Stat(base); err == nil && info.IsDir() {
			return filepath.Join(base, AppName)
		}
	}
	return filepath.Join(CacheDir(), "run")
}

// DefaultPath is the config file every command reads when --config is absent.
func DefaultPath() string { return filepath.Join(ConfigDir(), "config.toml") }

// SocketPath is where the daemon listens for control and status requests.
func SocketPath() string { return filepath.Join(RuntimeDir(), "daemon.sock") }

// ModelIndexPath caches model inspection results so rescans stay cheap.
func ModelIndexPath() string { return filepath.Join(CacheDir(), "model_index.json") }

// Expand resolves a leading ~ in a user-supplied path.
func Expand(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
		}
	}
	return path
}

// ResolvedModelsDir returns the absolute models directory.
func (c *Config) ResolvedModelsDir() string { return Expand(c.ModelsDir) }

// AudioCPPDirPath returns the absolute audio.cpp location.
func (c *Config) AudioCPPDirPath() string { return Expand(c.AudioCPPDir) }

// DownloadEndpoint returns the host model downloads go through, normalized by
// dropping any trailing slash.
//
// The check is deliberately syntactic. Whether a host is reachable or holds a
// given package is a question for `voiceime models sizes`, which asks the
// network; validation stays offline so `doctor` and `config validate` keep
// working on a disconnected machine.
func (c *Config) DownloadEndpoint() (string, error) {
	raw := strings.TrimSpace(c.DownloadMirror)
	if raw == "" {
		return "", errors.New("must be an http(s) endpoint URL, not empty")
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("is not a valid URL: %v", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", errors.New("must start with http:// or https://")
	}
	if parsed.Host == "" {
		return "", errors.New("has no host")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", errors.New("must not carry a query string or fragment")
	}
	return strings.TrimRight(raw, "/"), nil
}

// --------------------------------------------------------------------------- //
// Load / save / validate
// --------------------------------------------------------------------------- //

// Load reads the config file. A missing file is not an error: the caller gets
// the defaults, which is what a doctor or setup command needs on a fresh
// machine. Unknown keys are rejected so a typo cannot silently do nothing.
func Load(path string) (*Config, error) {
	cfg := Default()
	metadata, err := toml.DecodeFile(path, cfg)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var unknown []string
	for _, key := range metadata.Undecoded() {
		unknown = append(unknown, key.String())
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("%s: unknown setting(s) %s (run `voiceime capabilities` for valid keys)",
			path, strings.Join(unknown, ", "))
	}
	if cfg.SessionOptions == nil {
		cfg.SessionOptions = map[string]string{}
	}
	if cfg.RequestOptions == nil {
		cfg.RequestOptions = map[string]string{}
	}
	return cfg, nil
}

// Save writes the config so that a later Load returns an identical value.
func Save(cfg *Config, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var builder strings.Builder
	builder.WriteString("# voiceime configuration.\n")
	builder.WriteString("# Managed by `voiceime config set`; hand-editing is fine and supported.\n")
	builder.WriteString("# Key reference: `voiceime capabilities`, docs/AGENT.md.\n\n")
	if err := toml.NewEncoder(&builder).Encode(cfg); err != nil {
		return err
	}
	// Write through a temporary file: an interrupted write would otherwise leave
	// a half-parsed config behind and the daemon would refuse to start.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(builder.String()), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Issue is one validation finding. Fix is a shell command that resolves it,
// which is what lets an agent read a doctor report and act on it directly.
type Issue struct {
	Key      string `json:"key,omitempty"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Fix      string `json:"fix,omitempty"`
}

// Validate reports settings that would stop the pipeline working, or that are
// legal but known to hurt quality.
func (c *Config) Validate() []Issue {
	var issues []Issue
	add := func(severity, key, message, fix string) {
		issues = append(issues, Issue{Key: key, Severity: severity, Message: message, Fix: fix})
	}

	if c.Threads <= 0 {
		add("error", "threads", "must be positive", "voiceime config set threads 4")
	}
	if c.ChunkSeconds <= 0 {
		add("error", "chunk_seconds", "must be positive", "voiceime config set chunk_seconds 3")
	}
	// Sub-1.5s windows are legal, so this is a warning rather than an error, but
	// it is worth stating with the reason: the framework's streaming ASR decodes
	// each window independently, so a shorter window means less context.
	if c.ChunkSeconds > 0 && c.ChunkSeconds < 1.5 {
		add("warning", "chunk_seconds",
			"windows below 1.5s measurably corrupt CJK output with SenseVoice (docs/measurements.md)",
			"voiceime config set chunk_seconds 3")
	}
	if c.CaptureRate != TargetRate {
		add("error", "capture_rate",
			"the models' bundled Silero VAD only accepts 16000 Hz",
			fmt.Sprintf("voiceime config set capture_rate %d", TargetRate))
	}
	if c.Language == "" {
		add("warning", "language", "empty; the model will be asked to auto-detect",
			"voiceime config set language zh")
	}
	if c.MaxUtteranceSeconds < c.ChunkSeconds {
		add("warning", "max_utterance_seconds", "shorter than one streaming window", "")
	}
	if c.EndpointSilenceMS <= 0 {
		add("error", "endpoint_silence_ms", "must be positive", "voiceime config set endpoint_silence_ms 800")
	}
	if _, err := c.DownloadEndpoint(); err != nil {
		add("error", "download_mirror", err.Error(),
			"voiceime config set download_mirror "+DefaultDownloadEndpoint)
	}
	if c.Backend == "" {
		add("error", "backend", "must name a backend", "voiceime config set backend cpu")
	}
	if c.IBusEngineName == "" {
		add("error", "ibus_engine_name", "must be set; text delivery has no other channel",
			"voiceime config set ibus_engine_name voiceime")
	}
	if c.ModelsDir == "" {
		add("error", "models_dir", "must be set", "voiceime config set models_dir "+DefaultModelsDir)
	}
	return issues
}

// Errors filters a validation report down to the blocking issues.
func Errors(issues []Issue) []Issue {
	var out []Issue
	for _, issue := range issues {
		if issue.Severity == "error" {
			out = append(out, issue)
		}
	}
	return out
}

// --------------------------------------------------------------------------- //
// Reflection-based access, so `config get/set` and `capabilities` never need
// updating when a field is added.
// --------------------------------------------------------------------------- //

// Field describes one setting for agents: its TOML key, Go type, current value
// and what it does.
type Field struct {
	Key         string `json:"key"`
	Type        string `json:"type"`
	Value       any    `json:"value"`
	Description string `json:"description"`
}

// Fields lists every setting in file order.
func (c *Config) Fields() []Field {
	value := reflect.ValueOf(c).Elem()
	typ := value.Type()
	fields := make([]Field, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		structField := typ.Field(i)
		key := strings.Split(structField.Tag.Get("toml"), ",")[0]
		if key == "" || key == "-" {
			continue
		}
		fields = append(fields, Field{
			Key:         key,
			Type:        structField.Type.String(),
			Value:       value.Field(i).Interface(),
			Description: structField.Tag.Get("desc"),
		})
	}
	return fields
}

// Get returns one setting by its TOML key.
func (c *Config) Get(key string) (any, error) {
	value := reflect.ValueOf(c).Elem()
	typ := value.Type()
	for i := 0; i < typ.NumField(); i++ {
		if strings.Split(typ.Field(i).Tag.Get("toml"), ",")[0] == key {
			return value.Field(i).Interface(), nil
		}
	}
	return nil, fmt.Errorf("unknown setting %q; run `voiceime capabilities` for the list", key)
}

// Set assigns a setting from its command-line string form. The conversion lives
// here so `voiceime config set` stays one generic command an agent can use for
// any key without knowing the type.
func (c *Config) Set(key, raw string) error {
	value := reflect.ValueOf(c).Elem()
	typ := value.Type()
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if strings.Split(field.Tag.Get("toml"), ",")[0] != key {
			continue
		}
		target := value.Field(i)
		switch target.Kind() {
		case reflect.String:
			target.SetString(raw)
		case reflect.Int:
			number, err := strconv.Atoi(raw)
			if err != nil {
				return fmt.Errorf("%s must be an integer: %w", key, err)
			}
			target.SetInt(int64(number))
		case reflect.Float64:
			number, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				return fmt.Errorf("%s must be a number: %w", key, err)
			}
			target.SetFloat(number)
		case reflect.Bool:
			boolean, err := strconv.ParseBool(raw)
			if err != nil {
				return fmt.Errorf("%s must be true or false: %w", key, err)
			}
			target.SetBool(boolean)
		default:
			return fmt.Errorf("%s cannot be set from the command line (%s)", key, target.Kind())
		}
		return nil
	}
	return fmt.Errorf("unknown setting %q; run `voiceime capabilities` for the list", key)
}

// --------------------------------------------------------------------------- //
// External program discovery
// --------------------------------------------------------------------------- //

// FindASRBinary locates audiocpp_cli. Order: explicit setting, environment, the
// checkout's build tree (a source build may hold several backends and the
// configured one is preferred), then PATH.
func (c *Config) FindASRBinary() (string, error) {
	var candidates []string
	if c.ASRBin != "" {
		candidates = append(candidates, Expand(c.ASRBin))
	}
	for _, env := range []string{"VOICEIME_ASR_BIN", "AUDIOCPP_CLI"} {
		if value := os.Getenv(env); value != "" {
			candidates = append(candidates, Expand(value))
		}
	}
	if root := c.AudioCPPDirPath(); root != "" {
		matches, _ := filepath.Glob(filepath.Join(root, "build", "*", "bin", "audiocpp_cli"))
		sort.Strings(matches)
		var preferred []string
		for _, match := range matches {
			if strings.Contains(match, c.Backend) {
				preferred = append(preferred, match)
			}
		}
		candidates = append(candidates, preferred...)
		candidates = append(candidates, matches...)
	}
	if found, err := exec.LookPath("audiocpp_cli"); err == nil {
		candidates = append(candidates, found)
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", errors.New("audiocpp_cli not found: set asr_bin or audio_cpp_dir, or put it on PATH")
}

// FindModelManager locates audio.cpp's model manager script. Downloads are
// delegated to it on purpose: it already knows the registry, the pinned
// revisions and the mirror rules, and it is the code path upstream tests.
// Reimplementing that client here would drift from the registry silently.
func (c *Config) FindModelManager() (string, error) {
	var candidates []string
	if c.ModelManager != "" {
		candidates = append(candidates, Expand(c.ModelManager))
	}
	if root := c.AudioCPPDirPath(); root != "" {
		candidates = append(candidates, filepath.Join(root, "tools", "model_manager_v2.py"))
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", errors.New("model_manager_v2.py not found: set model_manager or audio_cpp_dir, or install audio.cpp")
}

// FindExecutable returns the first of names present on PATH, or "".
func FindExecutable(names ...string) string {
	for _, name := range names {
		if found, err := exec.LookPath(name); err == nil {
			return found
		}
	}
	return ""
}
