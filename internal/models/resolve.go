package models

import (
	"strings"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// Resolution is what each of the two recognition roles will use.
//
// One rule, in one place: an explicit path wins; an empty streaming_model picks the best
// installed streaming model; an empty refine_model reuses the streaming model. The
// configuration documents exactly that (config.go's refine_model, docs/AGENT.md), and it
// used to be implemented three times - the model list, `config show` and the generated
// server config - which disagreed: the server's copy picked a *second* model for repair
// instead of reusing the configured one, so choosing a streaming model that was not the
// auto-picked one made `config show` say "reuses the streaming model: A" while the
// service repaired with B. One function means no caller can hold a different opinion.
type Resolution struct {
	// Stream is the checkpoint live partials come from; "" when nothing is installed.
	Stream string
	// Repair is the checkpoint a finished utterance is re-decoded with.
	Repair string
	// StreamPicked records that Stream came from the ranking rather than a setting, and
	// RepairReused that Repair is the streaming model rather than a setting of its own.
	// Callers report them; nothing decides on them.
	StreamPicked bool
	RepairReused bool
}

// Resolve answers which checkpoint each role uses. streaming and refine are the
// configured values, not the paths; flags are the caller's business (a command line
// option that overrides a setting should say so where it parses it).
func Resolve(scanned []Model, language, streaming, refine string) Resolution {
	resolved := Resolution{}
	if explicit := strings.TrimSpace(streaming); explicit != "" {
		resolved.Stream = config.Expand(explicit)
	} else if picked := PickStreaming(scanned, language); picked != nil {
		resolved.Stream = picked.Path
		resolved.StreamPicked = true
	}
	if explicit := strings.TrimSpace(refine); explicit != "" {
		resolved.Repair = config.Expand(explicit)
	} else {
		resolved.Repair = resolved.Stream
		resolved.RepairReused = true
	}
	return resolved
}
