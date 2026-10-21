package config

import (
	"strings"
	"testing"
)

// The review list is what an agent on a new machine reads before it starts
// changing settings, so what belongs on it - and what does not - is pinned here.

func TestReviewDefaultsNamesTheSettingsThatAreNotPortable(t *testing.T) {
	cfg := Default()
	cfg.AudioCPPDir = "/reference/audio.cpp"
	reviews := ReviewDefaults(cfg, ReviewFacts{
		AudioCPPCheckout: "/somewhere/else/audio.cpp",
		Cores:            2,
		GPUs:             []string{"nvidia-smi"},
	})

	byKey := map[string]Review{}
	for _, review := range reviews {
		byKey[review.Key] = review
	}
	for _, want := range []string{"audio_cpp_dir", "threads", "backend", "language", "text_script", "text_numbers", "silence_rms_threshold", "download_mirror"} {
		if _, found := byKey[want]; !found {
			t.Errorf("%s is not reviewed", want)
		}
	}
	// Settings that are portable must not be on the list: the list is only useful
	// if it is short enough to actually review.
	for _, unwanted := range []string{"capture_rate", "chunk_seconds", "endpoint_silence_ms", "hotkey"} {
		if _, found := byKey[unwanted]; found {
			t.Errorf("%s is reviewed, but its value is not a property of the machine", unwanted)
		}
	}

	if fix := byKey["audio_cpp_dir"].Fix; !strings.Contains(fix, "/somewhere/else/audio.cpp") {
		t.Errorf("audio_cpp_dir fix = %q, want the located checkout in it", fix)
	}
	if fix := byKey["threads"].Fix; !strings.Contains(fix, "threads 2") {
		t.Errorf("threads fix = %q, want it to name this machine's core count", fix)
	}
	if why := byKey["backend"].Why; !strings.Contains(why, "nvidia-smi") {
		t.Errorf("backend why = %q, want it to mention the GPU found here", why)
	}
	// Nothing can guess a language, so no fix is offered for it.
	if byKey["language"].Fix != "" {
		t.Errorf("language fix = %q, want none: the value cannot be discovered", byKey["language"].Fix)
	}
}

func TestReviewDefaultsStaysQuietWhenTheDefaultsFit(t *testing.T) {
	cfg := Default()
	reviews := ReviewDefaults(cfg, ReviewFacts{
		AudioCPPCheckout: cfg.AudioCPPDir,
		Cores:            cfg.Threads * 2,
	})
	byKey := map[string]Review{}
	for _, review := range reviews {
		byKey[review.Key] = review
	}
	if byKey["audio_cpp_dir"].Fix != "" {
		t.Errorf("audio_cpp_dir offered a fix when the configured path is the one found: %q", byKey["audio_cpp_dir"].Fix)
	}
	if byKey["threads"].Fix != "" {
		t.Errorf("threads offered a fix at %d threads on %d cores: %q", cfg.Threads, cfg.Threads*2, byKey["threads"].Fix)
	}
	if !strings.Contains(byKey["audio_cpp_dir"].Why, "matches") {
		t.Errorf("audio_cpp_dir why = %q, want it to say the path was found", byKey["audio_cpp_dir"].Why)
	}
}
