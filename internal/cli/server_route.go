package cli

import (
	"fmt"
	"strings"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// serverDescription names what a front end talks to, for the engine's own log.
//
// It names the endpoint, and the model id the settings name, and nothing else. The unit is
// deliberately absent: the engine does not start it, cannot know whether it exists, and the
// endpoint may be a provider across a network - which is exactly the case where naming a local
// unit in the log would be a sentence about a machine that is not involved. `voiceime doctor`'s
// asr_endpoint and server_unit findings are what know that; a log line should not guess.
//
// The credential is never here: this line ends up in a file that outlives the session.
func serverDescription(cfg *config.Config) string {
	base := strings.TrimRight(strings.TrimSpace(cfg.ASRServerURL), "/")
	if named := strings.TrimSpace(cfg.Model); named != "" {
		return fmt.Sprintf("server %s (model %s)", base, named)
	}
	return fmt.Sprintf("server %s", base)
}
