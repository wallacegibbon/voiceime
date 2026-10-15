package config

import "fmt"

// Review is one setting whose default is a property of the machine or the user
// rather than of voiceime.
type Review struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
	// Why says what the default is and what it would be on this machine.
	Why string `json:"why"`
	// Fix is a runnable command, present only when there is one to run.
	Fix string `json:"fix,omitempty"`
}

// ReviewFacts is what the caller detected. It is a plain struct so the policy
// below stays free of I/O and testable on its own.
type ReviewFacts struct {
	// AudioCPPCheckout is a located audio.cpp checkout, or "".
	AudioCPPCheckout string
	// Cores is the physical core count, or 0 when unknown.
	Cores int
	// GPUs lists vendor compute tools found on PATH.
	GPUs []string
}

// ReviewDefaults lists the settings a fresh install should look at rather than
// accept, and says for each why it might be wrong here.
//
// The list exists because every default in Default() is a measurement or a path
// from the machine voiceime was developed on. Most of them are fine anywhere
// (capture rate is a hard constraint; the endpoint and window policy is model
// behaviour), but these six are not, and nothing used to say so - an agent on a
// new machine had no way to tell "4 threads" apart from "16000 Hz".
//
// It is not a validation report: nothing here is an error, and `doctor` is where
// settings that are actually wrong are reported.
func ReviewDefaults(cfg *Config, facts ReviewFacts) []Review {
	reviews := []Review{}

	audioCPP := Review{
		Key:   "audio_cpp_dir",
		Value: cfg.AudioCPPDir,
		Why:   "the default names the checkout voiceime was developed in; audio.cpp may be somewhere else here",
	}
	// Compare expanded forms: a caller may hand over either the configured
	// spelling ("~/audio.cpp") or the resolved one from a filesystem search.
	located := Expand(facts.AudioCPPCheckout)
	switch {
	case located != "" && Expand(cfg.AudioCPPDir) != located:
		audioCPP.Fix = "voiceime config set audio_cpp_dir " + located
		audioCPP.Why += fmt.Sprintf(", and it looks like it is at %s", located)
	case located != "":
		audioCPP.Why = "matches the checkout found on this machine"
	}
	reviews = append(reviews, audioCPP)

	threads := Review{
		Key:   "threads",
		Value: cfg.Threads,
		Why:   "the reference machine has 8 physical cores; more threads than cores contend rather than help",
	}
	if facts.Cores > 0 {
		threads.Why = fmt.Sprintf("this machine has %d physical cores; more threads than cores contend rather than help", facts.Cores)
		if cfg.Threads > facts.Cores {
			threads.Fix = fmt.Sprintf("voiceime config set threads %d", facts.Cores)
		}
	}
	reviews = append(reviews, threads)

	backend := Review{
		Key:   "backend",
		Value: cfg.Backend,
		Why:   "cpu works everywhere; a GPU needs an audio.cpp build with cuda, vulkan, hip or metal",
	}
	if len(facts.GPUs) > 0 {
		backend.Why = fmt.Sprintf("%s is installed here, so a GPU build is possible, but no GPU backend has been measured for this project", facts.GPUs[0])
	}
	reviews = append(reviews, backend)

	reviews = append(reviews,
		Review{
			Key:   "language",
			Value: cfg.Language,
			Why:   "the reference user's language; empty asks the model to auto-detect, which is usually worse for Chinese and better for everything else",
		},
		Review{
			Key:   "silence_rms_threshold",
			Value: cfg.SilenceRMSThreshold,
			Why:   "the reference room's noise level. It is only a floor - the detector adapts upward - but a floor above your voice means nothing is transcribed at all. Measure with `voiceime stream --seconds 10 --events`: silence must produce no `segment`, one sentence must produce one",
		},
		Review{
			Key:   "download_mirror",
			Value: cfg.DownloadMirror,
			Why:   "chosen because huggingface.co is unreachable from the reference network; any Hugging Face protocol endpoint works",
		},
	)
	return reviews
}
