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
	// ASRBinary is a built audio.cpp CLI found on this machine, or "".
	ASRBinary string
	// Cores is the physical core count, or 0 when unknown.
	Cores int
	// GPUs lists vendor compute tools found on PATH.
	GPUs []string
}

func ReviewDefaults(cfg *Config, facts ReviewFacts) []Review {
	reviews := []Review{}

	asr := Review{
		Key:   "asr_bin",
		Value: cfg.ASRBin,
		Why:   "every decode goes through audio.cpp's CLI, and where it was built is a property of this machine, not of voiceime",
	}
	// Compare expanded forms: a caller may hand over either the configured spelling
	// ("~/audio.cpp/...") or the resolved one from a filesystem search.
	found := Expand(facts.ASRBinary)
	switch {
	case found == "" && Expand(cfg.ASRBin) == "":
		asr.Why += "; nothing built was found on this machine either, so audio.cpp has to be built or named"
	case found == "":
		asr.Why += ", and nothing built was found here to point it at"
	case Expand(cfg.ASRBin) != found:
		asr.Fix = "voiceime config set asr_bin " + found
		asr.Why += fmt.Sprintf(", and a build was found here at %s", found)
	default:
		asr.Why = "matches the audio.cpp build found on this machine"
	}
	reviews = append(reviews, asr)

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
	)
	return reviews
}
