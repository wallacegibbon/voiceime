package audio

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// A recording whose bytes are wrong is not an error at start-up: it is a hold that decodes
// to nothing, which is the harder of the two to notice. So the argument list is read from
// the installed tool's own usage block rather than chosen: `-a/--raw` is how a newer
// PipeWire is told to put raw samples on stdout, and a pw-record from before that option
// (1.0.5, the version this was found on) refuses it outright while writing raw samples
// to `-` anyway.
func TestPwRecordArgumentsComeFromTheInstalledTool(t *testing.T) {
	const base = "--format s16 --rate 16000 --channels 1"
	for _, tc := range []struct {
		name string
		help string
		want string
	}{
		{
			name: "a pw-record that has no such option (pipewire 1.0.5)",
			help: "pw-record [options] [<file>|-]\n" +
				"  --rate                        Sample rate (req. for rec) (default 48000)\n" +
				"  -q  --quality                 Resampler quality (0 - 15) (default 4)\n",
			want: base + " -",
		},
		{
			name: "a pw-record that has it",
			help: "pw-cat [options] [<file>|-]\n" +
				"  -a, --raw                         RAW mode\n" +
				"  --rate                        Sample rate (req. for rec) (default 48000)\n",
			want: base + " --raw -",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			// Usage text on stdout and a failing exit: that is what an unrecognised
			// option costs, and the answer has to be the one `--help` gives too.
			// printf, not cat: PATH below is the temp directory alone, which is the
			// point of it, and an external `cat` would not be findable there.
			quoted := make([]string, 0, len(strings.Split(tc.help, "\n")))
			for _, line := range strings.Split(tc.help, "\n") {
				quoted = append(quoted, "'"+line+"'")
			}
			script := "#!/bin/sh\nprintf '%s\\n' " + strings.Join(quoted, " ") + "\nexit 1\n"
			path := filepath.Join(dir, "pw-record")
			if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)

			mic, err := NewMicrophone(&config.Config{})
			if err != nil {
				t.Fatalf("NewMicrophone: %v", err)
			}
			source, ok := mic.(*microphone)
			if !ok {
				t.Fatalf("NewMicrophone returned %T, want *microphone", mic)
			}
			want := append([]string{path}, strings.Fields(tc.want)...)
			if got := source.command; !reflect.DeepEqual(got, want) {
				t.Errorf("argv = [%s], want [%s]", strings.Join(got, " "), strings.Join(want, " "))
			}
		})
	}
}

// The installed tool is asked once, not once per hold. A capture client is built for every
// hold, and this question sits between the key coming up and the first sample - which is
// where a person starts speaking. Measured on this machine on 2026-11-20, the question
// costs 15-21ms, so three dictations would pay three of them for an answer that cannot
// change while the engine runs.
func TestTheInstalledPwRecordIsAskedOnceRatherThanOncePerHold(t *testing.T) {
	dir := t.TempDir()
	asked := filepath.Join(dir, "asked")
	// One line in that file per invocation of the tool, which counts the questions.
	script := "#!/bin/sh\nprintf 'asked\\n' >> " + asked +
		"\nprintf '  -a, --raw   RAW mode\\n'\n"
	path := filepath.Join(dir, "pw-record")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	for hold := 1; hold <= 3; hold++ {
		mic, err := NewMicrophone(&config.Config{})
		if err != nil {
			t.Fatalf("NewMicrophone: %v", err)
		}
		source, ok := mic.(*microphone)
		if !ok {
			t.Fatalf("NewMicrophone returned %T, want *microphone", mic)
		}
		want := []string{path, "--format", "s16", "--rate", "16000", "--channels", "1", "--raw", "-"}
		if got := source.command; !reflect.DeepEqual(got, want) {
			t.Errorf("hold %d: argv = [%s], want [%s]", hold, strings.Join(got, " "), strings.Join(want, " "))
		}
	}

	questions, err := os.ReadFile(asked)
	if err != nil {
		t.Fatalf("the installed tool was never asked: %v", err)
	}
	if got := strings.Count(string(questions), "asked"); got != 1 {
		t.Errorf("the installed tool was asked %d times, want once for three holds", got)
	}
}

// A question that belongs at start-up is asked at start-up: the engine calls WarmUpCapture
// when it becomes an engine, so no press of the trigger pays for the answer. Measured on
// this machine on 2026-11-20, the answer costs 15-21ms, and that is long enough to be the
// difference between the first syllable of a sentence and the start of the recording.
func TestWarmUpCaptureLeavesTheInstalledToolUnaskedAfterwards(t *testing.T) {
	dir := t.TempDir()
	asked := filepath.Join(dir, "asked")
	script := "#!/bin/sh\nprintf 'asked\\n' >> " + asked +
		"\nprintf '  -a, --raw   RAW mode\\n'\n"
	path := filepath.Join(dir, "pw-record")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	tool, err := WarmUpCapture(&config.Config{})
	if err != nil {
		t.Fatalf("WarmUpCapture: %v", err)
	}
	if tool != path {
		t.Errorf("WarmUpCapture named the capture client %q, want %q", tool, path)
	}
	questions, err := os.ReadFile(asked)
	if err != nil {
		t.Fatalf("WarmUpCapture did not ask the installed tool: %v", err)
	}
	if got := strings.Count(string(questions), "asked"); got != 1 {
		t.Errorf("WarmUpCapture asked the installed tool %d times, want once", got)
	}

	for hold := 1; hold <= 3; hold++ {
		if _, err := NewMicrophone(&config.Config{}); err != nil {
			t.Fatalf("NewMicrophone for hold %d: %v", hold, err)
		}
	}
	after, err := os.ReadFile(asked)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(after), "asked"); got != 1 {
		t.Errorf("the installed tool was asked %d times after a warm-up, want three holds to ask nothing", got)
	}
}
