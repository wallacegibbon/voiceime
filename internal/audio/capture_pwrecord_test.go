package audio

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

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
	path := fakeCapture(t, dir, "printf 'asked\\n' >> "+asked+"\nprintf '%0640d' 0\n")

	tool, _, err := WarmUpCapture(&config.Config{})
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

// fakeCapture writes a pw-record stand-in into dir, puts dir on PATH, and returns its path.
// It answers the question the program asks about raw samples and then runs body, which is
// what it does when it is started for a capture. Only shell builtins: PATH is dir alone.
func fakeCapture(t *testing.T, dir, body string) string {
	t.Helper()
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = '--help' ]; then\n" +
		"  printf '  -a, --raw   RAW mode\\n'\n" +
		"  exit 1\n" +
		"fi\n" + body
	path := filepath.Join(dir, "pw-record")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return path
}

// A warm-up that only asked the tool a question would leave the device closed, and the first
// open of it is what costs: measured on this machine on 2026-11-21, 1.09s before the first
// sample for the first open after the sound server starts, 0.36s for every open after it. The
// engine opens it once at start-up instead, throws the audio away, and pays that second
// before anybody has pressed anything.
func TestWarmUpCaptureOpensTheDeviceAndKeepsNoAudio(t *testing.T) {
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	path := fakeCapture(t, dir, "printf 'started\\n' >> "+started+"\nprintf '%0640d' 0\n")

	tool, took, err := WarmUpCapture(&config.Config{})
	if err != nil {
		t.Fatalf("WarmUpCapture: %v", err)
	}
	if tool != path {
		t.Errorf("WarmUpCapture named the capture client %q, want %q", tool, path)
	}
	if took <= 0 {
		t.Errorf("WarmUpCapture reported the first sample after %v: a client that never opened anything is what this is meant to catch", took)
	}
	runs, err := os.ReadFile(started)
	if err != nil {
		t.Fatalf("the capture client was never started, so the device was never opened: %v", err)
	}
	if got := strings.Count(string(runs), "started"); got != 1 {
		t.Errorf("the capture client was started %d times for one warm-up, want once", got)
	}
}

// A login can start this process before the sound server is listening, and a warm-up that
// gave up at the first refusal would fail at exactly the moment it is worth having.
func TestWarmUpCaptureRetriesUntilTheSoundServerAnswers(t *testing.T) {
	shrinkWarmUp(t, time.Second, 5*time.Second, 10*time.Millisecond)
	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	// The first two attempts fail the way a client with no server to talk to fails: nothing
	// on stdout, a non-zero exit.
	path := fakeCapture(t, dir, "n=0\n"+
		"if [ -f "+count+" ]; then read -r n < "+count+"; fi\n"+
		"n=$((n+1))\n"+
		"printf '%s\\n' \"$n\" > "+count+"\n"+
		"if [ \"$n\" -lt 3 ]; then exit 1; fi\n"+
		"printf '%0640d' 0\n")

	tool, _, err := WarmUpCapture(&config.Config{})
	if err != nil {
		t.Fatalf("WarmUpCapture after two refusals: %v", err)
	}
	if tool != path {
		t.Errorf("WarmUpCapture named the capture client %q, want %q", tool, path)
	}
	if got := readCount(t, count); got != "3" {
		t.Errorf("the client was started %s times, want 3: the third attempt is the one that delivered", got)
	}
}

// The budget is a budget: an engine that never came up would be worse than a warm device.
func TestWarmUpCaptureGivesUpWithinItsBudget(t *testing.T) {
	budget := 100 * time.Millisecond
	shrinkWarmUp(t, 20*time.Millisecond, budget, 20*time.Millisecond)
	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	path := fakeCapture(t, dir, "n=0\n"+
		"if [ -f "+count+" ]; then read -r n < "+count+"; fi\n"+
		"n=$((n+1))\n"+
		"printf '%s\\n' \"$n\" > "+count+"\n"+
		"exit 1\n")

	began := time.Now()
	if _, _, err := WarmUpCapture(&config.Config{}); err == nil {
		t.Fatal("WarmUpCapture reported success for a client that never delivered a sample")
	} else if !strings.Contains(err.Error(), path) {
		t.Errorf("the failure does not name the client it is about: %v", err)
	}
	if elapsed := time.Since(began); elapsed > 10*budget {
		t.Errorf("WarmUpCapture took %v with a %v budget", elapsed, budget)
	}
	if got := readCount(t, count); got == "1" {
		t.Error("the client was tried once: a login can start this process before the sound server")
	}
}

// A client that starts and then says nothing is the case the timeout exists for: the engine
// has to come up anyway, and the log line has to say what it waited for.
func TestWarmUpCaptureStopsWaitingForAClientThatSaysNothing(t *testing.T) {
	shrinkWarmUp(t, 50*time.Millisecond, 100*time.Millisecond, 10*time.Millisecond)
	dir := t.TempDir()
	fakeCapture(t, dir, "while :; do :; done\n")

	began := time.Now()
	_, _, err := WarmUpCapture(&config.Config{})
	if err == nil {
		t.Fatal("WarmUpCapture reported success for a client that never wrote a sample")
	}
	if elapsed := time.Since(began); elapsed > 500*time.Millisecond {
		t.Errorf("WarmUpCapture waited %v for a client that says nothing", elapsed)
	}
}

// shrinkWarmUp shortens the warm-up's bounds for one test and puts them back afterwards: a
// test that sat through the real budget would cost more than the failure it guards against.
func shrinkWarmUp(t *testing.T, first, budget, retry time.Duration) {
	t.Helper()
	wasFirst, wasBudget, wasRetry := warmUpFirstSample, warmUpBudget, warmUpRetry
	warmUpFirstSample, warmUpBudget, warmUpRetry = first, budget, retry
	t.Cleanup(func() { warmUpFirstSample, warmUpBudget, warmUpRetry = wasFirst, wasBudget, wasRetry })
}

// readCount is the last attempt number the fake client wrote.
func readCount(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the client recorded no attempt: %v", err)
	}
	return strings.TrimSpace(string(data))
}
