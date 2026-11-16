package cli

import (
	"fmt"
	"strings"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// serverDescription names what a front end talks to, for the engine's own log.
//
// It says the address exactly as the settings hold it, because that is the address the next
// request goes to: nothing is appended to it on the way out, so a line that tidied it would be
// describing something other than what happened. It names the model id when the settings name
// one, and never the unit: the engine does not start that unit, cannot know whether it exists,
// and the endpoint may be a provider across a network - which is exactly the case where naming a
// local unit in the log would be a sentence about a machine that is not involved. `voiceime
// doctor`'s asr_endpoint and server_unit findings are what know that; a log line should not guess.
//
// The credential is never here: this line ends up in a file that outlives the session.
func serverDescription(cfg *config.Config) string {
	address := strings.TrimSpace(cfg.ASRServerURL)
	if named := strings.TrimSpace(cfg.Model); named != "" {
		return fmt.Sprintf("server %s (model %s)", address, named)
	}
	return fmt.Sprintf("server %s", address)
}
