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

const TargetRate = 16000

type Config struct {
	ASRBin string `toml:"asr_bin" desc:"Path to audio.cpp's audiocpp_cli; audiocpp_server is taken from beside it. 'voiceime apply' records a build it finds when this is empty."`

	Backend string `toml:"backend" desc:"audio.cpp backend: cpu, cuda, vulkan, hip, metal."`
	Threads int    `toml:"threads" desc:"Worker threads handed to audio.cpp."`

	Model string `toml:"model" desc:"The GGUF the service serves and the engine transcribes with. You name it: voiceime scans nothing and downloads nothing."`

	Language   string `toml:"language" desc:"Recognition language code, e.g. zh or en."`
	TextScript string `toml:"text_script" desc:"Chinese output script: simplified converts Traditional characters to Simplified; preserve leaves them."`

	// A hold whose key never comes up stops recording here.
	MaxUtteranceSeconds float64 `toml:"max_utterance_seconds" desc:"Stop one recording after this many seconds."`

	ASRServerURL       string `toml:"asr_server_url" desc:"Resident audiocpp_server root, e.g. http://127.0.0.1:18080; 'voiceime apply' sets it."`
	ASRServerTimeoutMS int    `toml:"asr_server_timeout_ms" desc:"Deadline for one transcription."`

	CaptureSource string `toml:"capture_source" desc:"PipeWire source name; empty uses the default input."`

	// Held down to dictate. A modifier is required, so the press cannot also be ordinary
	// typing.
	PushToTalk string `toml:"push_to_talk" desc:"Combination held down to dictate; the release transcribes the recording."`

	// Passed straight into the generated server config, so a new audio.cpp request option
	// is usable without a voiceime release.
	RequestOptions map[string]string `toml:"request_options" desc:"Extra request options for the served model, set as key=value; an empty value removes one."`
}

func Default() *Config {
	return &Config{
		Backend:    "cpu",
		Threads:    4,
		Language:   "zh",
		TextScript: "simplified",
		// A forgotten key must not grow a recording without bound.
		MaxUtteranceSeconds: 60,
		ASRServerTimeoutMS:  30000,
		// A chord: the engine sees the key while the user is typing, so the press that
		// starts a dictation must not be one the application was going to receive.
		PushToTalk:     "<Control>grave",
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

// RuntimeDir holds state that belongs to one session: the daemon socket, the
// generated audio.cpp service config. A desktop session owns it (systemd's logind
// sets XDG_RUNTIME_DIR to a private directory that is emptied at logout), and a
// machine without one has nothing session-scoped to offer, so the answer degrades
// to the cache directory - still disposable, just no longer per-login.
//
// It is never relative: a generated path that depended on the caller's working
// directory would be a different file to the installer, the service and `doctor`.
func RuntimeDir() string {
	if base := os.Getenv("XDG_RUNTIME_DIR"); base != "" {
		if info, err := os.Stat(base); err == nil && info.IsDir() {
			return filepath.Join(base, AppName)
		}
	}
	if cache := CacheDir(); filepath.IsAbs(cache) {
		return filepath.Join(cache, "run")
	}
	// No session directory and no home: the cache answer would be relative. The
	// temp directory is the only remaining answer that is the same file to
	// whoever resolves it.
	return filepath.Join(os.TempDir(), AppName)
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

// Expand resolves a leading ~ in a user-supplied path.
func Expand(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
		}
	}
	return path
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
	if raw := strings.TrimSpace(c.ASRServerURL); raw != "" {
		if err := checkServerURL(raw); err != nil {
			add("error", "asr_server_url", err.Error(),
				"voiceime config set asr_server_url http://127.0.0.1:18080")
		}
	}
	if c.Backend == "" {
		add("error", "backend", "must name a backend", "voiceime config set backend cpu")
	}
	if strings.TrimSpace(c.PushToTalk) == "" {
		add("error", "push_to_talk", "must be set; the engine has no other way to start recording",
			`voiceime config set push_to_talk "<Control>grave"`)
	}
	// A checkpoint is named, never chosen: voiceime reads the one GGUF this key points at
	// and asks nothing of the directory around it.
	if path := strings.TrimSpace(c.Model); path == "" {
		add("error", "model", "must name a GGUF; nothing is scanned and nothing is downloaded",
			"voiceime config set model /path/to/model.gguf")
	} else if _, err := os.Stat(Expand(path)); err != nil {
		reason := err.Error()
		if errors.Is(err, os.ErrNotExist) {
			reason = "does not exist"
		}
		add("error", "model", fmt.Sprintf("%s %s", path, reason),
			"voiceime config set model /path/to/model.gguf")
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

// FindASRBinary returns the audio.cpp CLI the settings name.
//
// There is one way to get it: the asr_bin setting. The only fallback is PATH, which is a
// decision someone made about this machine rather than a guess about where a build tree
// might be. It used to guess - a checkout directory, then build/*/bin/audiocpp_cli under it,
// preferring the path whose name contained the backend - so adding a second build tree
// could change which inference a running service performed. Settling it belongs to
// `voiceime apply`, which writes the answer - what it finds, or what PATH named - into
// asr_bin and says which it used. Measured on the reference machine, ~/.local/bin and a
// build tree both hold a runnable audiocpp_cli, of different sizes and md5s.
func (c *Config) FindASRBinary() (string, error) {
	if configured := strings.TrimSpace(c.ASRBin); configured != "" {
		path := Expand(configured)
		if isRunnable(path) {
			return path, nil
		}
		return "", fmt.Errorf("%s is what asr_bin names, and it is not a runnable file: point asr_bin at the audio.cpp build's audiocpp_cli, or build audio.cpp", path)
	}
	if found, err := exec.LookPath("audiocpp_cli"); err == nil {
		return found, nil
	}
	return "", errors.New("no audio.cpp CLI: point asr_bin at audiocpp_cli, or put it on PATH (`voiceime capabilities` shows what is found)")
}

// FindServerBinary returns the resident host that serves the decodes. It is the sibling of
// the CLI, which is where audio.cpp builds it: one recorded path fixes both.
func (c *Config) FindServerBinary() (string, error) {
	if configured := strings.TrimSpace(c.ASRBin); configured != "" {
		sibling := filepath.Join(filepath.Dir(Expand(configured)), serverBinaryName)
		if isRunnable(sibling) {
			return sibling, nil
		}
	} else if found, err := exec.LookPath(serverBinaryName); err == nil {
		return found, nil
	}
	return "", fmt.Errorf("%s not found: asr_bin names %s, and %s is not beside it (audio.cpp builds both into the same directory)",
		serverBinaryName, strings.TrimSpace(c.ASRBin), serverBinaryName)
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
