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

	reviews = append(reviews,
		Review{
			Key:   "language",
			Value: cfg.Language,
			Why:   "the reference user's language; empty asks the model to auto-detect, which is usually worse for Chinese and better for everything else",
		},
		Review{
			Key:   "text_script",
			Value: cfg.TextScript,
			Why:   "the reference user writes Simplified Chinese; set preserve if you want the repair model's Traditional characters kept",
		},
	)

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
			Key:   "push_to_talk",
			Value: cfg.PushToTalk,
			Why:   "a chord of your choosing; it must not collide with a shortcut the applications you use claim",
		},
		Review{
			Key:   "download_mirror",
			Value: cfg.DownloadMirror,
			Why:   "chosen because huggingface.co was unreachable from the reference network; any Hugging Face protocol endpoint works",
		},
	)
	return reviews
}
