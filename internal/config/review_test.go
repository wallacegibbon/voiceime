package config

import (
	"sort"
	"strings"
	"testing"
)

// The review list is what an agent on a new machine reads before it starts
// changing settings, so what belongs on it - and what does not - is pinned here.

func TestReviewDefaultsNamesTheSettingsThatAreNotPortable(t *testing.T) {
	cfg := Default()
	cfg.ASRBin = "/reference/audio.cpp/build/linux-cpu-release/bin/audiocpp_cli"
	reviews := ReviewDefaults(cfg, ReviewFacts{
		ASRBinary: "/somewhere/else/audio.cpp/build/linux-cuda-release/bin/audiocpp_cli",
		Cores:     2,
		GPUs:      []string{"nvidia-smi"},
	})

	byKey := map[string]Review{}
	for _, review := range reviews {
		byKey[review.Key] = review
	}
	// The list is exactly this, and an exhaustive comparison is what keeps it that way:
	// the list is only useful if it is short enough to actually read, so a setting that is
	// portable - a timeout, an engine name, anything whose value is not a property of the
	// machine - has to be argued for before it can be added.
	want := []string{"asr_bin", "backend", "language", "push_to_talk", "text_script", "threads"}
	var got []string
	for _, review := range reviews {
		got = append(got, review.Key)
	}
	sort.Strings(got)
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("reviewed settings = %v, want exactly %v", got, want)
	}
	for _, setting := range want {
		if _, found := byKey[setting]; !found {
			t.Errorf("%s is not reviewed", setting)
		}
	}

	if fix := byKey["asr_bin"].Fix; !strings.Contains(fix, "/somewhere/else/audio.cpp/build/") {
		t.Errorf("asr_bin fix = %q, want the CLI found on this machine in it", fix)
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
	found := "/home/u/audio.cpp/build/linux-cpu-release/bin/audiocpp_cli"
	cfg.ASRBin = found
	reviews := ReviewDefaults(cfg, ReviewFacts{
		ASRBinary: found,
		Cores:     cfg.Threads * 2,
	})
	byKey := map[string]Review{}
	for _, review := range reviews {
		byKey[review.Key] = review
	}
	if byKey["asr_bin"].Fix != "" {
		t.Errorf("asr_bin offered a fix when it already names the CLI found here: %q", byKey["asr_bin"].Fix)
	}
	if byKey["threads"].Fix != "" {
		t.Errorf("threads offered a fix at %d threads on %d cores: %q", cfg.Threads, cfg.Threads*2, byKey["threads"].Fix)
	}
	if !strings.Contains(byKey["asr_bin"].Why, "matches") {
		t.Errorf("asr_bin why = %q, want it to say the CLI was found", byKey["asr_bin"].Why)
	}
}

// The failure mode on a fresh machine is that audio.cpp has not been built at all. Saying
// so is the useful part: the reader needs to know there is nothing to point at.
func TestReviewDefaultsSaysWhenThereIsNothingToName(t *testing.T) {
	cfg := Default()
	cfg.ASRBin = ""
	byKey := map[string]Review{}
	for _, review := range ReviewDefaults(cfg, ReviewFacts{Cores: 8}) {
		byKey[review.Key] = review
	}
	if byKey["asr_bin"].Fix != "" {
		t.Errorf("asr_bin fix = %q, want none: there is no path here to suggest", byKey["asr_bin"].Fix)
	}
	if !strings.Contains(byKey["asr_bin"].Why, "nothing built") {
		t.Errorf("asr_bin why = %q, want it to say nothing was found on this machine", byKey["asr_bin"].Why)
	}
}
