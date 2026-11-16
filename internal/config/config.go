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

// Config is what a person owns: which address to ask, what to call the model there, the
// credential if one is needed, and how dictation behaves - the key, how long a hold may run,
// which language, which Chinese script, which microphone.
//
// Recognition is an address. The engine posts one recording to it and reads text back; the
// address is used exactly as written, because an address that names a route is the thing being
// configured. Whether it is a model on this machine or a provider across a network is not the
// engine's concern, and starting a local model is a separate thing
// (scripts/asr-service.sh), needed only when the address is this machine's. A setting that
// deployment can work out is a question asked of the wrong person: which audio.cpp binary
// runs, which backend it was built for and how many workers it wants are answers about the
// machine, and they are recorded where they are used - the service unit and the audio.cpp
// config that unit names.
type Config struct {
	// Model is the id named in the request, and it is required: the program does not ask an
	// address what it serves, because a model list is a catalogue and not a promise about what
	// this account accepts, and an endpoint may publish none at all. A local service's id is
	// written here by scripts/asr-service.sh, which is the program that chooses it.
	Model string `toml:"model" desc:"The model id to name in the request. Required; scripts/asr-service.sh writes the id of a model it starts here."`

	// APIKey is sent as `Authorization: Bearer <value>`. A credential lives here rather
	// than in an environment the engine never starts.
	APIKey string `toml:"api_key" desc:"Bearer token for the address; empty sends no Authorization header."`

	Language   string `toml:"language" desc:"Recognition language code, e.g. zh or en."`
	TextScript string `toml:"text_script" desc:"Chinese output script: simplified converts Traditional characters to Simplified; preserve leaves them."`

	// A hold whose key never comes up stops recording here.
	MaxUtteranceSeconds float64 `toml:"max_utterance_seconds" desc:"Stop one recording after this many seconds."`

	// The whole request address, used verbatim: no route is appended, nothing is derived from
	// it, and the same string is what `curl` would be pointed at.
	ASRServerURL       string `toml:"asr_server_url" desc:"The address a recording is POSTed to, used exactly as written: http://127.0.0.1:18080/v1/audio/transcriptions for a model on this machine (scripts/asr-service.sh writes that), a provider's full URL otherwise."`
	ASRServerTimeoutMS int    `toml:"asr_server_timeout_ms" desc:"Deadline for one transcription."`

	CaptureSource string `toml:"capture_source" desc:"PipeWire source name; empty uses the default input."`

	// Held down to dictate. A modifier is required, so the press cannot also be ordinary
	// typing.
	PushToTalk string `toml:"push_to_talk" desc:"Combination held down to dictate; the release transcribes the recording."`
}

func Default() *Config {
	return &Config{
		Language:   "zh",
		TextScript: "simplified",
		// A forgotten key must not grow a recording without bound.
		MaxUtteranceSeconds: 60,
		ASRServerTimeoutMS:  30000,
		// A chord: the engine sees the key while the user is typing, so the press that
		// starts a dictation must not be one the application was going to receive.
		PushToTalk: "<Control>grave",
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

// checkServerURL validates the address a recording is posted to. It is an address, not a
// local process: the same client posts to a model on this machine and to a provider across a
// network, so both schemes are valid and TLS is the transport's business rather than the
// engine's. What is checked is only that a request can be made to it at all, because the
// address is used exactly as written - nothing is appended to it and nothing is derived from
// it - so a full address with a path in it is normal and expected.
func checkServerURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("is not a valid URL: %v (needs a scheme and a host, e.g. http://127.0.0.1:18080/v1/audio/transcriptions)", err)
	}
	switch parsed.Scheme {
	case "http", "https":
	case "":
		return errors.New("needs a scheme and a host, e.g. http://127.0.0.1:18080/v1/audio/transcriptions")
	default:
		return fmt.Errorf("must start with http:// or https://, not %q", parsed.Scheme)
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
	return cfg, nil
}

// Save writes the settings so that a later Load returns an identical value.
//
// A file that exists is edited, not regenerated. Its comments, its commented-out alternatives,
// its blank lines and the order of its keys belong to whoever reads it: an endpoint that was
// switched away from is kept as a comment precisely so it can be switched back, and a document
// built from a parsed struct has nowhere to put that. Only the lines holding a key this program
// owns are replaced, and a key the file does not have is added at the end. A file that is not
// there is written whole, with the header that says where the key reference is.
func Save(cfg *Config, path string) error {
	document, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if strings.TrimSpace(string(document)) == "" {
		document = []byte(settingsHeader)
	}
	text := string(document)
	for _, field := range cfg.Fields() {
		text = setKeyInDocument(text, field.Key, encodeValue(field.Value))
	}
	// 0600, not 0644: `api_key` is a bearer token, and a file that can hold a credential is
	// not something everyone who can read this home directory should be able to read.
	// CreateTemp already made the temporary file 0600; the chmod names it explicitly so that
	// a file written before credentials could live here is tightened the next time anything
	// is written to it.
	return WriteFile(path, []byte(text), 0o600)
}

const settingsHeader = "# voiceime configuration.\n" +
	"# Managed by `voiceime config set`; hand-editing is fine and supported.\n" +
	"# Key reference: `voiceime capabilities`, docs/AGENT.md.\n"

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
				"voiceime config set asr_server_url http://127.0.0.1:18080/v1/audio/transcriptions")
		} else if strings.TrimSpace(c.Model) == "" {
			// An address and a name to ask it for are one decision: nothing here asks an
			// address what it serves, so an address on its own cannot be used.
			add("error", "model", fmt.Sprintf("%s is configured, and nothing names the model to ask it for", raw),
				`voiceime config set model <id>   # or run scripts/asr-service.sh, which writes the id of a model it starts here`)
		}
	}
	if strings.TrimSpace(c.PushToTalk) == "" {
		add("error", "push_to_talk", "must be set; the engine has no other way to start recording",
			`voiceime config set push_to_talk "<Control>grave"`)
	}
	// A model is named, never chosen. Nothing is scanned, nothing is downloaded, and no file
	// is read: what belongs here is the id the address is asked for. A path in this key is the
	// one mistake worth catching, because it is what this key used to hold - and the file it
	// names belongs to the service, which is a different thing (scripts/asr-service.sh).
	if named := strings.TrimSpace(c.Model); named != "" {
		if strings.HasPrefix(named, "/") || strings.HasPrefix(named, "~") || strings.EqualFold(filepath.Ext(named), ".gguf") {
			add("error", "model", fmt.Sprintf("%q looks like a file, not a model id: the file belongs to the service on this machine (scripts/asr-service.sh), this key names what the address is asked for", named),
				`voiceime config set model <id>   # the id that address serves, not a path`)
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
		default:
			return fmt.Errorf("%s cannot be set from the command line (%s)", key, target.Kind())
		}
		return nil
	}
	return fmt.Errorf("unknown setting %q; run `voiceime capabilities` for the list", key)
}

// --------------------------------------------------------------------------- //

// FindExecutable returns the first of names present on PATH, or "".
func FindExecutable(names ...string) string {
	for _, name := range names {
		if found, err := exec.LookPath(name); err == nil {
			return found
		}
	}
	return ""
}
