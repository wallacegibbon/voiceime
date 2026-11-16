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
// engine's concern, and starting a model on this machine is a script's doing
// (scripts/asr-service.sh), needed only when the address is this machine's. A setting that
// deployment can work out is a question asked of the wrong person: which audio.cpp binary runs,
// which backend it was built for and how many workers it wants are answers about the machine,
// and they are recorded where they are used - the service unit and the audio.cpp config that
// unit names.
type Config struct {
	// Model is the id named in the request, and it is required: the program does not ask an
	// address what it serves, because a model list is a catalogue and not a promise about what
	// this account accepts, and an endpoint may publish none at all. For a model on this
	// machine the id is the one the script that starts the service prints, and the two lines
	// belong in the settings together.
	Model string `toml:"model" desc:"The model id to name in the request. Required; scripts/asr-service.sh prints the id of a model it starts on this machine."`

	// APIKey is sent as `Authorization: Bearer <value>`. A credential lives here rather
	// than in an environment the engine never starts.
	APIKey string `toml:"api_key" desc:"Bearer token for the address; empty sends no Authorization header."`

	Language   string `toml:"language" desc:"Recognition language code, e.g. zh or en."`
	TextScript string `toml:"text_script" desc:"Chinese output script: simplified converts Traditional characters to Simplified; preserve leaves them."`

	// A hold whose key never comes up stops recording here.
	MaxUtteranceSeconds float64 `toml:"max_utterance_seconds" desc:"Stop one recording after this many seconds."`

	// The whole request address, used verbatim: no route is appended, nothing is derived from
	// it, and the same string is what `curl` would be pointed at.
	ASRServerURL       string `toml:"asr_server_url" desc:"The address a recording is POSTed to, used exactly as written: http://127.0.0.1:18080/v1/audio/transcriptions for a model on this machine (scripts/asr-service.sh prints that line), a provider's full URL otherwise."`
	ASRServerTimeoutMS int    `toml:"asr_server_timeout_ms" desc:"Deadline for one transcription."`

	CaptureSource string `toml:"capture_source" desc:"PipeWire source name; empty uses the default input."`

	// Held down to dictate. A modifier is required, so the press cannot also be ordinary
	// typing.
	PushToTalk string `toml:"push_to_talk" desc:"Combination held down to dictate; the release transcribes the recording."`

	// Where these values came from, recorded by Load so a validation finding can name the
	// file to edit. Nothing in this program writes that file: a finding that says what to
	// change has to say where, because the person reading it is the one who will change it.
	path string `toml:"-"`
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
	// Recorded before the decode: a file that is not there yet is still the file a fix
	// belongs in, and that is the case where saying so helps most.
	cfg.path = path
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

// Issue is one validation finding. Fix says what to do about it: a command to run, or the
// line to write and the file to write it in, named. Nothing in this program writes that
// file, so a finding is the one place the two halves of the instruction are ever
// together, and an agent reading a doctor report can still act on it.
type Issue struct {
	Key      string `json:"key,omitempty"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Fix      string `json:"fix,omitempty"`
}

// FixLine is the instruction to change one setting - the line, and the file it belongs in -
// for advice written outside this package. A finding and a piece of advice are the same
// sentence said twice, so only one of them is written down.
func (c *Config) FixLine(key, line string) string { return c.writeIn(key, line) }

// writeIn is the fix for a value only a person changes: the line, and the file it belongs
// to. Load records that path; a Config built inside a test has none, and says where to
// find it rather than naming a file that does not exist.
func (c *Config) writeIn(key, line string) string {
	where := c.path
	if where == "" {
		where = "the settings file (`voiceime capabilities` prints its path)"
	}
	return fmt.Sprintf("write %s = %s in %s", key, line, where)
}

// Validate reports settings that would stop the pipeline working.
func (c *Config) Validate() []Issue {
	var issues []Issue
	add := func(severity, key, message, fix string) {
		issues = append(issues, Issue{Key: key, Severity: severity, Message: message, Fix: fix})
	}

	if c.Language == "" {
		add("warning", "language", "empty; the model will be asked to auto-detect",
			c.writeIn("language", `"zh"`))
	}
	if value := strings.TrimSpace(c.TextScript); value != "" && value != "simplified" && value != "preserve" {
		add("error", "text_script", "must be simplified or preserve",
			c.writeIn("text_script", `"simplified"`))
	}
	if c.MaxUtteranceSeconds <= 0 {
		add("error", "max_utterance_seconds", "must be positive",
			c.writeIn("max_utterance_seconds", "60"))
	}
	if raw := strings.TrimSpace(c.ASRServerURL); raw != "" {
		if err := checkServerURL(raw); err != nil {
			add("error", "asr_server_url", err.Error(),
				c.writeIn("asr_server_url", `"http://127.0.0.1:18080/v1/audio/transcriptions"`))
		} else if strings.TrimSpace(c.Model) == "" {
			// An address and a name to ask it for are one decision: nothing here asks an
			// address what it serves, so an address on its own cannot be used.
			add("error", "model", fmt.Sprintf("%s is configured, and nothing names the model to ask it for", raw),
				c.writeIn("model", `"<id>"`)+"   # the id that address serves; for a model on this machine, the id scripts/asr-service.sh prints when it starts one")
		}
	}
	if strings.TrimSpace(c.PushToTalk) == "" {
		add("error", "push_to_talk", "must be set; the engine has no other way to start recording",
			c.writeIn("push_to_talk", `"<Control>grave"`))
	}
	// A model is named, never chosen. Nothing is scanned, nothing is downloaded, and no file
	// is read: what belongs here is the id the address is asked for. A path in this key is the
	// one mistake worth catching, because it is what this key used to hold - and the file it
	// names belongs to the service, which this script starts and this program only posts to.
	if named := strings.TrimSpace(c.Model); named != "" {
		if strings.HasPrefix(named, "/") || strings.HasPrefix(named, "~") || strings.EqualFold(filepath.Ext(named), ".gguf") {
			add("error", "model", fmt.Sprintf("%q looks like a file, not a model id: the file belongs to the service on this machine (scripts/asr-service.sh), this key names what the address is asked for", named),
				c.writeIn("model", `"<id>"`)+"   # the id that address serves, not a path")
		}
	}
	// A credential in a file this program only reads is a promise whoever keeps the file
	// made: nothing tightens the mode any more, because nothing writes the file. So the
	// permission is reported beside the credential, which is the one place a person
	// reading the report would not otherwise think to look.
	if strings.TrimSpace(c.APIKey) != "" && c.path != "" {
		if info, err := os.Stat(c.path); err == nil && info.Mode().Perm()&0o077 != 0 {
			add("warning", "api_key", fmt.Sprintf("is in a file other people can read (mode %o)", info.Mode().Perm()),
				"chmod 600 "+c.path)
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
