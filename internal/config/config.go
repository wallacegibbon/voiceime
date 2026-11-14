// Package config holds every user-visible setting, the XDG paths they live in,
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

const DefaultModelsDir = "~/models"

const DefaultDownloadEndpoint = "https://hf-mirror.com"

const TargetRate = 16000

type Config struct {
	ModelsDir    string `toml:"models_dir" desc:"Directory scanned for model packages; downloads are written here."`
	AudioCPPDir  string `toml:"audio_cpp_dir" desc:"audio.cpp checkout used to locate its CLI and model manager."`
	ASRBin       string `toml:"asr_bin" desc:"Explicit path to audiocpp_cli; empty means discover."`
	ModelManager string `toml:"model_manager" desc:"Explicit path to tools/model_manager_v2.py; empty means discover."`

	Backend string `toml:"backend" desc:"audio.cpp backend: cpu, cuda, vulkan, hip, metal."`
	Threads int    `toml:"threads" desc:"Worker threads handed to audio.cpp."`

	// The one checkpoint the service serves and the engine transcribes with. Empty picks
	// the best installed ASR model.
	Model string `toml:"model" desc:"Checkpoint the recognition service loads; empty picks the best installed ASR model."`

	Language   string `toml:"language" desc:"Recognition language code, e.g. zh or en."`
	TextScript string `toml:"text_script" desc:"Chinese output script: simplified converts Traditional characters to Simplified; preserve leaves them."`

	// A hold whose key never comes up stops recording here.
	MaxUtteranceSeconds float64 `toml:"max_utterance_seconds" desc:"Stop one recording after this many seconds."`

	ASRServerURL       string `toml:"asr_server_url" desc:"Resident audiocpp_server root, e.g. http://127.0.0.1:18080; 'voiceime server install' sets it."`
	ASRServerTimeoutMS int    `toml:"asr_server_timeout_ms" desc:"Deadline for one transcription."`

	CaptureSource string `toml:"capture_source" desc:"PipeWire source name; empty uses the default input."`

	DownloadMirror string `toml:"download_mirror" desc:"HF-protocol endpoint URL model downloads go through, e.g. https://hf-mirror.com."`

	// Held down to dictate. A modifier is required, so the press cannot also be ordinary
	// typing.
	PushToTalk     string `toml:"push_to_talk" desc:"Combination held down to dictate; the release transcribes the recording."`
	IBusEngineName string `toml:"ibus_engine_name" desc:"IBus engine name registered for text delivery."`

	// Passed straight into the generated server config, so a new audio.cpp request option
	// is usable without a voiceime release.
	RequestOptions map[string]string `toml:"request_options" desc:"Extra request options for the served model, set as key=value; an empty value removes one."`
}

func Default() *Config {
	return &Config{
		ModelsDir:   DefaultModelsDir,
		AudioCPPDir: "~/playground/audio.cpp",
		Backend:     "cpu",
		Threads:     4,
		Language:    "zh",
		TextScript:  "simplified",
		// A forgotten key must not grow a recording without bound.
		MaxUtteranceSeconds: 60,
		ASRServerTimeoutMS:  30000,
		DownloadMirror:      DefaultDownloadEndpoint,
		// A chord: the engine sees the key while the user is typing, so the press that
		// starts a dictation must not be one the application was going to receive.
		PushToTalk:     "<Control>grave",
		IBusEngineName: "voiceime",
		RequestOptions: map[string]string{},
	}
}

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

func RuntimeDir() string {
	if base := os.Getenv("XDG_RUNTIME_DIR"); base != "" {
		if info, err := os.Stat(base); err == nil && info.IsDir() {
			return filepath.Join(base, AppName)
		}
	}
	return filepath.Join(CacheDir(), "run")
}

// EnvironmentDir is the directory the systemd user manager reads the session
// environment from. It belongs to the session rather than to voiceime, so the file
// written there is named after the program (environment.d/voiceime.conf).
func EnvironmentDir() string {
	return filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), "environment.d")
}

func UserUnitDir() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "systemd", "user")
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

// checkServerURL validates the address of the resident recognition server.
func checkServerURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("is not a valid URL: %v (needs a scheme and a host, e.g. http://127.0.0.1:18080)", err)
	}
	switch parsed.Scheme {
	case "http":
	case "https":
		return errors.New("must be http://, not https://: the live stream speaks HTTP over a raw socket and cannot do TLS")
	case "":
		return errors.New("needs a scheme and a host, e.g. http://127.0.0.1:18080")
	default:
		return fmt.Errorf("must start with http://, not %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return errors.New("has no host")
	}
	return nil
}

// --------------------------------------------------------------------------- //

var retiredSettings = map[string]bool{
	"streaming_model":        true,
	"refine_model":           true,
	"chunk_seconds":          true,
	"refine_on_segment_end":  true,
	"refine_merge_ms":        true,
	"endpoint_silence_ms":    true,
	"endpoint_min_speech_ms": true,
	"silence_rms_threshold":  true,
	"hotkey":                 true,
	"hotkey_shortcut_type":   true,
	"text_numbers":           true,
}

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
		if retiredSettings[key.String()] {
			continue
		}
		unknown = append(unknown, key.String())
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("%s: unknown setting(s) %s (run `voiceime capabilities` for valid keys)",
			path, strings.Join(unknown, ", "))
	}
	if cfg.RequestOptions == nil {
		cfg.RequestOptions = map[string]string{}
	}
	return cfg, nil
}

// Save writes the config so that a later Load returns an identical value.
func Save(cfg *Config, path string) error {
	var builder strings.Builder
	builder.WriteString("# voiceime configuration.\n")
	builder.WriteString("# Managed by `voiceime config set`; hand-editing is fine and supported.\n")
	builder.WriteString("# Key reference: `voiceime capabilities`, docs/AGENT.md.\n\n")
	if err := toml.NewEncoder(&builder).Encode(cfg); err != nil {
		return err
	}
	return WriteFile(path, []byte(builder.String()), 0o644)
}

// WriteFile replaces path with data atomically, creating its directory.
func WriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	// A no-op once the rename has succeeded; otherwise it is the cleanup of a failed
	// write, which must not leave temporary files in the user's config directory.
	defer os.Remove(name)

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	// Sync before the rename: the point of the temporary file is that path either
	// names the old contents or the new ones, including across a power cut.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	// CreateTemp makes the file 0600; the configuration and the index are readable
	// by anyone who can run the binary, which is what the previous os.WriteFile gave.
	if err := os.Chmod(name, perm); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// Issue is one validation finding. Fix is a shell command that resolves it,
// which is what lets an agent read a doctor report and act on it directly.
type Issue struct {
	Key      string `json:"key,omitempty"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Fix      string `json:"fix,omitempty"`
}

// Validate reports settings that would stop the pipeline working.
func (c *Config) Validate() []Issue {
	var issues []Issue
	add := func(severity, key, message, fix string) {
		issues = append(issues, Issue{Key: key, Severity: severity, Message: message, Fix: fix})
	}

	if c.Threads <= 0 {
		add("error", "threads", "must be positive", "voiceime config set threads 4")
	}
	if c.Language == "" {
		add("warning", "language", "empty; the model will be asked to auto-detect",
			"voiceime config set language zh")
	}
	if value := strings.TrimSpace(c.TextScript); value != "" && value != "simplified" && value != "preserve" {
		add("error", "text_script", "must be simplified or preserve",
			"voiceime config set text_script simplified")
	}
	if c.MaxUtteranceSeconds <= 0 {
		add("error", "max_utterance_seconds", "must be positive",
			"voiceime config set max_utterance_seconds 60")
	}
	if _, err := c.DownloadEndpoint(); err != nil {
		add("error", "download_mirror", err.Error(),
			"voiceime config set download_mirror "+DefaultDownloadEndpoint)
	}
	if raw := strings.TrimSpace(c.ASRServerURL); raw != "" {
		if err := checkServerURL(raw); err != nil {
			add("error", "asr_server_url", err.Error(),
				"voiceime config set asr_server_url http://127.0.0.1:18080")
		}
	}
	if c.Backend == "" {
		add("error", "backend", "must name a backend", "voiceime config set backend cpu")
	}
	if c.IBusEngineName == "" {
		add("error", "ibus_engine_name", "must be set; text delivery has no other channel",
			"voiceime config set ibus_engine_name voiceime")
	}
	if strings.TrimSpace(c.PushToTalk) == "" {
		add("error", "push_to_talk", "must be set; the engine has no other way to start recording",
			`voiceime config set push_to_talk "<Control>grave"`)
	}
	if c.ModelsDir == "" {
		add("error", "models_dir", "must be set", "voiceime config set models_dir "+DefaultModelsDir)
	}
	if path := strings.TrimSpace(c.Model); path != "" {
		if _, err := os.Stat(Expand(path)); err != nil {
			reason := err.Error()
			if errors.Is(err, os.ErrNotExist) {
				reason = "does not exist"
			}
			add("error", "model", fmt.Sprintf("%s %s", path, reason),
				`voiceime config set model ""   # empty picks an installed model`)
		}
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
		case reflect.Map:
			if target.Type().Key().Kind() != reflect.String || target.Type().Elem().Kind() != reflect.String {
				return fmt.Errorf("%s cannot be set from the command line (%s)", key, target.Type())
			}
			entry, value, found := strings.Cut(raw, "=")
			entry = strings.TrimSpace(entry)
			if !found || entry == "" {
				return fmt.Errorf("%s needs key=value, e.g. %s audio_chunk_mode=none (an empty value removes a key)", key, key)
			}
			if target.IsNil() {
				target.Set(reflect.MakeMap(target.Type()))
			}
			if value == "" {
				target.SetMapIndex(reflect.ValueOf(entry).Convert(target.Type().Key()), reflect.Value{})
				return nil
			}
			target.SetMapIndex(
				reflect.ValueOf(entry).Convert(target.Type().Key()),
				reflect.ValueOf(value).Convert(target.Type().Elem()))
		default:
			return fmt.Errorf("%s cannot be set from the command line (%s)", key, target.Kind())
		}
		return nil
	}
	return fmt.Errorf("unknown setting %q; run `voiceime capabilities` for the list", key)
}

// --------------------------------------------------------------------------- //

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
	for _, root := range c.checkoutRoots() {
		matches, _ := filepath.Glob(filepath.Join(root, filepath.FromSlash(audioCPPBuildGlob)))
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

func (c *Config) FindServerBinary() (string, error) {
	var candidates []string
	if c.ASRBin != "" {
		candidates = append(candidates, filepath.Join(filepath.Dir(Expand(c.ASRBin)), serverBinaryName))
	}
	if value := os.Getenv("VOICEIME_ASR_SERVER"); value != "" {
		candidates = append(candidates, Expand(value))
	}
	for _, root := range c.checkoutRoots() {
		matches, _ := filepath.Glob(filepath.Join(root, filepath.FromSlash(audioCPPBuildGlob)))
		sort.Strings(matches)
		var preferred, rest []string
		for _, match := range matches {
			candidate := filepath.Join(filepath.Dir(match), serverBinaryName)
			if strings.Contains(candidate, c.Backend) {
				preferred = append(preferred, candidate)
			}
			rest = append(rest, candidate)
		}
		candidates = append(candidates, preferred...)
		candidates = append(candidates, rest...)
	}
	if found, err := exec.LookPath(serverBinaryName); err == nil {
		candidates = append(candidates, found)
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%s not found: point asr_bin at the audio.cpp build's audiocpp_cli, or put %s on PATH",
		serverBinaryName, serverBinaryName)
}

func (c *Config) checkoutRoots() []string {
	var roots []string
	if configured := c.AudioCPPDirPath(); configured != "" {
		roots = append(roots, configured)
	}
	if located := LocateAudioCPP(c); located != "" {
		duplicate := false
		for _, existing := range roots {
			if existing == located {
				duplicate = true
			}
		}
		if !duplicate {
			roots = append(roots, located)
		}
	}
	return roots
}

func (c *Config) FindModelManager() (string, error) {
	var candidates []string
	if c.ModelManager != "" {
		candidates = append(candidates, Expand(c.ModelManager))
	}
	for _, root := range c.checkoutRoots() {
		candidates = append(candidates, filepath.Join(root, filepath.FromSlash(audioCPPManagerMarker)))
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
