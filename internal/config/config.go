// Package config holds every user-visible setting, the XDG paths they live in, and the
// rules that say whether a set of them can work.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// AppName is the directory name under every XDG base directory.
const AppName = "voiceime"

// Config is what a person owns: which address to ask, what to call the model there, the
// credential if one is needed, and how dictation behaves - the key, how long a hold may run,
// which language, which Chinese script, which microphone.
//
// Recognition is an address. The engine posts one recording to it and reads text back; the
// address is used exactly as written, because an address that names a route is the thing being
// configured. Whether it is a model on this machine or a provider across a network is not the
// engine's concern, and what stands behind the address - a service on this machine, another
// machine's, a provider's - is not something the program asks about. A setting that deployment
// can work out is a question asked of the wrong person: which audio.cpp binary runs, which
// backend it was built for and how many workers it wants are answers about a machine that this
// program has no reason to look at.
type Config struct {
	// Model is the id named in the request, and it is required: the program does not ask an
	// address what it serves, because a model list is a catalogue and not a promise about what
	// this account accepts, and an endpoint may publish none at all.
	Model string `toml:"model"`

	// APIKey is sent as `Authorization: Bearer <value>`. A credential lives here rather
	// than in an environment the engine never starts; empty sends no header.
	APIKey string `toml:"api_key"`

	// Language is the code passed to the model. Empty asks it to detect the language from
	// the audio, which is usually worse for Chinese and better for most other languages.
	Language string `toml:"language"`

	// TextScript is what happens to Chinese output: simplified converts whatever script the
	// model produced, preserve leaves it as it came.
	TextScript string `toml:"text_script"`

	// A hold whose key never comes up stops recording here.
	MaxUtteranceSeconds float64 `toml:"max_utterance_seconds"`

	// The whole request address, used verbatim: no route is appended, nothing is derived from
	// it, and the same string is what `curl` would be pointed at.
	ASRServerURL string `toml:"asr_server_url"`

	// The deadline for one transcription, from the end of the recording to the text.
	ASRServerTimeoutMS int `toml:"asr_server_timeout_ms"`

	// A PipeWire source name. Empty uses whatever the sound server says is the default
	// input.
	CaptureSource string `toml:"capture_source"`

	// Held down to dictate. A modifier is required, so the press cannot also be ordinary
	// typing.
	PushToTalk string `toml:"push_to_talk"`

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

// CacheDir holds state that is written and never read back by this program: the engine's
// log, which ibus-daemon's child writes because its own stdout is discarded.
func CacheDir() string { return filepath.Join(xdg("XDG_CACHE_HOME", ".cache"), AppName) }

// DataDir holds installed component definitions such as the IBus XML.
func DataDir() string {
	return filepath.Join(xdg("XDG_DATA_HOME", filepath.Join(".local", "share")), AppName)
}

// EnvironmentDir is the directory the systemd user manager reads the session
// environment from. It belongs to the session rather than to voiceime, so the file
// written there is named after the program (environment.d/voiceime.conf).
func EnvironmentDir() string {
	return filepath.Join(xdg("XDG_CONFIG_HOME", ".config"), "environment.d")
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
		return nil, fmt.Errorf("%s: unknown setting(s) %s", path, strings.Join(unknown, ", "))
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

// ASRServerTimeout is the ceiling one request gets. The file holds milliseconds because the
// file is written by a person; this is the only place that number becomes a duration, so a
// dictation and the probe `voiceime doctor` sends in its place are bounded by one ceiling
// rather than by two.
func (c *Config) ASRServerTimeout() time.Duration {
	return time.Duration(c.ASRServerTimeoutMS) * time.Millisecond
}

// FixLine is the instruction to change one setting - the line, and the file it belongs in -
// for advice written outside this package. A finding and a piece of advice are the same
// sentence said twice, so only one of them is written down.
func (c *Config) FixLine(key, line string) string { return c.writeIn(key, line) }

// writeIn is the fix for a value only a person changes: the line, and the file it belongs
// to. Load records that path; a Config built inside a test has none, and says which file it
// means in words rather than naming a file that does not exist.
func (c *Config) writeIn(key, line string) string {
	where := c.path
	if where == "" {
		where = "the settings file"
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
	// The two bounds belong together in the file and here: one recording is what
	// max_utterance_seconds allows, and this is the time it is given to come back as text.
	// A value that is not positive is not a short deadline, it is no deadline - every
	// request would be abandoned before it was sent - and it is caught here rather than
	// quietly replaced by a number this program keeps to itself.
	if c.ASRServerTimeoutMS <= 0 {
		add("error", "asr_server_timeout_ms", "must be a positive number of milliseconds",
			c.writeIn("asr_server_timeout_ms", "30000"))
	}
	if raw := strings.TrimSpace(c.ASRServerURL); raw != "" {
		if err := checkServerURL(raw); err != nil {
			add("error", "asr_server_url", err.Error(),
				c.writeIn("asr_server_url", `"https://api.example.com/v1/audio/transcriptions"`))
		} else if strings.TrimSpace(c.Model) == "" {
			// An address and a name to ask it for are one decision: nothing here asks an
			// address what it serves, so an address on its own cannot be used.
			add("error", "model", fmt.Sprintf("%s is configured, and nothing names the model to ask it for", raw),
				c.writeIn("model", `"<id>"`)+"   # the id that address is asked for")
		}
	}
	if strings.TrimSpace(c.PushToTalk) == "" {
		add("error", "push_to_talk", "must be set; the engine has no other way to start recording",
			c.writeIn("push_to_talk", `"<Control>grave"`))
	}
	// A model is named, never chosen. Nothing is scanned, nothing is downloaded, and no file
	// is read: what belongs here is the id the address is asked for. A path in this key is the
	// one mistake worth catching, because it is what this key used to hold.
	if named := strings.TrimSpace(c.Model); named != "" {
		if strings.HasPrefix(named, "/") || strings.HasPrefix(named, "~") || strings.EqualFold(filepath.Ext(named), ".gguf") {
			add("error", "model", fmt.Sprintf("%q looks like a file, not a model id: this key names what the address is asked for, and a file on a disk is not an id", named),
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

// FindExecutable returns the first of names present on PATH, or "".
func FindExecutable(names ...string) string {
	for _, name := range names {
		if found, err := exec.LookPath(name); err == nil {
			return found
		}
	}
	return ""
}
