package models

import (
	"strings"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// Resolution is the checkpoint a dictation will use.
type Resolution struct {
	// Model is the checkpoint's path; "" when nothing suitable is installed.
	Model string
	// Picked records that Model came from the ranking rather than a setting.
	Picked bool
}

// Resolve answers which checkpoint to use: an explicit setting wins, otherwise the best
// installed transcription model for the language.
func Resolve(scanned []Model, language, configured string) Resolution {
	if explicit := strings.TrimSpace(configured); explicit != "" {
		return Resolution{Model: config.Expand(explicit)}
	}
	if picked := Pick(scanned, language); picked != nil {
		return Resolution{Model: picked.Path, Picked: true}
	}
	return Resolution{}
}
