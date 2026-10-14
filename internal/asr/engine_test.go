package asr

import "testing"

// The line protocol is the whole integration surface with audio.cpp, so its
// parser is tested against lines captured from the real CLI rather than invented
// ones. If upstream changes the format, these tests are what fail.
func TestParseLine(t *testing.T) {
	cases := []struct {
		line string
		want Event
		ok   bool
	}{
		{"audio_input=stdin format=s16le rate=16000 channels=1", Event{Kind: EventReady, Text: "stdin format=s16le rate=16000 channels=1"}, true},
		{"partial_text=Some call me nature.", Event{Kind: EventPartial, Text: "Some call me nature."}, true},
		{"partial_text=", Event{Kind: EventPartial, Text: ""}, true},
		{"partial_text=欢迎大家来体验达摩院。", Event{Kind: EventPartial, Text: "欢迎大家来体验达摩院。"}, true},
		{"partial_text_snapshot=revisable hypothesis", Event{Kind: EventSnapshot, Text: "revisable hypothesis"}, true},
		{"event=speech_start sample=16000 probability=0.93", Event{Kind: EventSpeechStart, Sample: 16000, Probability: 0.93}, true},
		{"event=speech_end sample=32000 probability=0.11", Event{Kind: EventSpeechEnd, Sample: 32000, Probability: 0.11}, true},
		// Result lines are not events; they are collected as the session result.
		{"text_output=Some call me nature.", Event{}, false},
		{"family=sense_asr", Event{}, false},
		{"metrics.rtf=0.05", Event{}, false},
		{"", Event{}, false},
	}
	for _, testCase := range cases {
		got, ok := parseLine(testCase.line)
		if ok != testCase.ok {
			t.Errorf("parseLine(%q) ok = %v, want %v", testCase.line, ok, testCase.ok)
			continue
		}
		if !ok {
			continue
		}
		if got.Kind != testCase.want.Kind || got.Text != testCase.want.Text ||
			got.Sample != testCase.want.Sample || got.Probability != testCase.want.Probability {
			t.Errorf("parseLine(%q) = %+v, want %+v", testCase.line, got, testCase.want)
		}
	}
}

func TestClassifyFinal(t *testing.T) {
	for _, line := range []string{"text_output=hi", "family=sense_asr", "task=asr", "mode=streaming", "metrics.rtf=0.05"} {
		if _, _, ok := classifyFinal(line); !ok {
			t.Errorf("classifyFinal(%q) = not final, want final", line)
		}
	}
	for _, line := range []string{"partial_text=hi", "event=speech_start", "nonsense"} {
		if _, _, ok := classifyFinal(line); ok {
			t.Errorf("classifyFinal(%q) = final, want not final", line)
		}
	}
}

func TestExtractTextOutput(t *testing.T) {
	report := "audio_input=stdin format=s16le rate=16000 channels=1\n" +
		"partial_text=hello\n" +
		"family=sense_asr\ntask=asr\nmode=offline\n" +
		"text_output=hello world\n" +
		"metrics.rtf=0.05\n"
	if got := extractTextOutput(report); got != "hello world" {
		t.Errorf("extractTextOutput = %q, want %q", got, "hello world")
	}
	if got := extractTextOutput("no result here"); got != "" {
		t.Errorf("extractTextOutput on a report with no text = %q, want empty", got)
	}
}

func TestCommandLineIsReproducible(t *testing.T) {
	cfg := EngineConfig{
		Binary:         "/bin/audiocpp_cli",
		Model:          "/models/m.gguf",
		Backend:        "cpu",
		Threads:        4,
		Language:       "zh",
		ChunkSeconds:   3,
		RequestOptions: map[string]string{"zeta": "1", "alpha": "2"},
	}
	args := cfg.CommandLine()
	want := []string{
		"--task", "asr", "--mode", "streaming", "--model", "/models/m.gguf",
		"--backend", "cpu", "--threads", "4", "--audio", "-",
		"--input-format", "s16le", "--input-rate", "16000", "--input-channels", "1",
		"--language", "zh", "--request-option", "audio_chunk_duration_sec=3",
		"--request-option", "alpha=2", "--request-option", "zeta=1",
	}
	if len(args) != len(want) {
		t.Fatalf("CommandLine() = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("CommandLine()[%d] = %q, want %q (full: %v)", i, args[i], want[i], args)
		}
	}
	// A float window must not render as 3.000: the CLI parses it either way, but
	// a reproducible command line is what a bug report needs.
	if got := trimFloat(3); got != "3" {
		t.Errorf("trimFloat(3) = %q, want 3", got)
	}
	if got := trimFloat(2.5); got != "2.5" {
		t.Errorf("trimFloat(2.5) = %q, want 2.5", got)
	}
}
