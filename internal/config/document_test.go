package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The file is written by the program and read by a person, and the two things a person keeps in
// it are comments and a commented-out alternative: the endpoint that was switched away from, kept
// so it can be switched back. A write that regenerates the document deletes both, which is how a
// hand-edited settings file ended up with the reasoning stripped out of it. So a write edits the
// lines it is asked about, and leaves the rest byte for byte.
func TestSaveKeepsTheCommentsAPersonWrote(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	handwritten := `# my voiceime settings
# local model: scripts/asr-service.sh wrote this; bigmodel is what I use now
asr_server_url = "http://127.0.0.1:18080/v1/audio/transcriptions"   # keep until the token runs out
# asr_server_url = "https://open.bigmodel.cn/api/paas/v4/audio/transcriptions"
# model = "glm-asr-2512"

model = "sensevoice-small-q8-audiocpp-v1"
language = "zh"
push_to_talk = "<Control>grave"
`
	if err := os.WriteFile(path, []byte(handwritten), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.ASRServerURL = "https://open.bigmodel.cn/api/paas/v4/audio/transcriptions"
	cfg.Model = "glm-asr-2512"
	cfg.APIKey = "a token with a # in it"
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(written)
	for _, keep := range []string{
		"# my voiceime settings",
		"# local model: scripts/asr-service.sh wrote this; bigmodel is what I use now",
		`# asr_server_url = "https://open.bigmodel.cn/api/paas/v4/audio/transcriptions"`,
		`# model = "glm-asr-2512"`,
		"# keep until the token runs out",
		`language = "zh"`,
		`push_to_talk = "<Control>grave"`,
	} {
		if !strings.Contains(text, keep) {
			t.Errorf("%q is gone from the file:\n%s", keep, text)
		}
	}
	if lines := liveKeyLines(text, "asr_server_url"); lines != 1 {
		t.Errorf("asr_server_url is set on %d lines, want one (the commented-out one stays a comment):\n%s", lines, text)
	}

	// And the values are the ones that were asked for, including one holding a `#`.
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load after Save: %v", err)
	}
	if loaded.ASRServerURL != cfg.ASRServerURL || loaded.Model != cfg.Model || loaded.APIKey != cfg.APIKey {
		t.Errorf("round trip changed settings: %+v", loaded)
	}
}

// liveKeyLines counts the lines that set a key, as opposed to the lines that only mention it
// in a comment.
func liveKeyLines(document, key string) int {
	count := 0
	for _, line := range strings.Split(document, "\n") {
		if _, name, _, found := splitAssignment(line); found && name == key {
			count++
		}
	}
	return count
}

// A file written from nothing is still a whole file: the header, and every key.
func TestSaveWritesAWholeFileWhenThereIsNothingToEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	cfg := Default()
	cfg.Model = "glm-asr-2512"
	if err := Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(written)
	if !strings.HasPrefix(text, "# voiceime configuration.\n") {
		t.Errorf("the header is missing:\n%s", text)
	}
	for _, field := range cfg.Fields() {
		if !strings.Contains(text, "\n"+field.Key+" = ") {
			t.Errorf("%s is not in the file it wrote:\n%s", field.Key, text)
		}
	}
	if _, err := Load(path); err != nil {
		t.Errorf("the file it wrote does not read back: %v", err)
	}
}

// A key a person removed is not a key the program forgot: it comes back at the end, where
// adding something to a list belongs, rather than in the middle of the order they set up.
func TestSaveAddsAKeyTheFileDoesNotHave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	handwritten := "# just what I changed\npush_to_talk = \"<Control>grave\"\n"
	if err := os.WriteFile(path, []byte(handwritten), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.Model = "glm-asr-2512"
	if err := Save(cfg, path); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(written)
	if !strings.HasPrefix(text, "# just what I changed\npush_to_talk = ") {
		t.Errorf("the file's own beginning was changed:\n%s", text)
	}
	if !strings.Contains(text, `model = "glm-asr-2512"`) {
		t.Errorf("model was not written:\n%s", text)
	}
	if strings.Count(text, "push_to_talk = ") != 1 {
		t.Errorf("push_to_talk appears twice:\n%s", text)
	}
}
