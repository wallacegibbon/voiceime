package models

// RecommendedModel is one checkpoint worth fetching, as advice.
type RecommendedModel struct {
	// ID is the name audio.cpp publishes the package under, which is what to search for
	// where the GGUFs are hosted.
	ID string `json:"id"`
	// Reason is what distinguishes the package: how big it is on disk and which
	// languages it accepts. Nothing else is claimed here - a speed figure quoted in
	// an error message is a number nobody can check, and the measured figures live
	// in docs/measurements.md with the machine they were measured on.
	Reason string `json:"reason"`
}

// RecommendedModels lists what to download. voiceime does not fetch anything: it reads the
// checkpoints that are already on the machine. This table exists because every
// "no model to serve" answer has to say what would fix it, and an empty answer would
// leave an agent to guess a model family and a size.
func RecommendedModels() []RecommendedModel {
	return []RecommendedModel{
		{"sensevoice_small_q8", "242 MiB; reads a whole recording in one pass; zh, en, yue and more - the default"},
		{"qwen3_asr_0_6b_q8_0", "1.07 GiB; the larger checkpoint, which also reads zh and en"},
		{"kroko_asr_community_q8_0", "160 MiB, one language per package (de/en/es/fr/it/he/nl/pt/sv/tr)"},
		{"moonshine_streaming_tiny_q8_0", "58 MiB, English-only, useful for testing"},
	}
}
