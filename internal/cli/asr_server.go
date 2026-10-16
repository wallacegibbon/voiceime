package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wallacegibbon/voiceime/internal/asr"
	"github.com/wallacegibbon/voiceime/internal/config"
)

// repairTranscriber decides how the repair pass decodes a finished utterance.
//
// Returning nil means "use the default", which is one audiocpp_cli process per
// utterance. A configured server replaces that with one upload to a resident
// process, which is the difference between a large refine model being affordable
// and not: the server pays the model load once, while a process pays it every
// utterance.
//
// A configured but unusable server falls back to the process route with a note
// rather than failing the session. The fallback is a working repair, so
// dictation still corrects its words; it is slower, and saying so is the
// difference between a degradation and a silent one.
func repairTranscriber(ctx context.Context, cfg *config.Config) (asr.Transcriber, string) {
	base := strings.TrimSpace(cfg.ASRServerURL)
	if base == "" {
		return nil, ""
	}
	transcriber := asr.ServerTranscriber{
		BaseURL:  base,
		Model:    strings.TrimSpace(cfg.ASRServerModel),
		Language: cfg.Language,
		Timeout:  time.Duration(cfg.ASRServerTimeoutMS) * time.Millisecond,
	}
	if err := transcriber.Check(ctx); err != nil {
		return nil, fmt.Sprintf("asr_server_url is set but unusable, repairing with a process per utterance: %v", err)
	}
	return transcriber, ""
}

// repairRouteDescription names the route that will run, for a startup log line.
// A non-nil note from repairTranscriber replaces it: the user configured a server
// and needs to know why it is not being used.
func repairRouteDescription(cfg *config.Config, transcriber asr.Transcriber, note string) string {
	if note != "" {
		return note
	}
	if transcriber == nil {
		return "repair: one audiocpp_cli process per utterance"
	}
	return fmt.Sprintf("repair: resident server at %s (model %s)",
		strings.TrimRight(cfg.ASRServerURL, "/"), cfg.ASRServerModel)
}
