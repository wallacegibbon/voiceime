package cli

import (
	"fmt"
	"strings"

	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/server"
)

// The resident server is the only decode route, so these three helpers are how the
// front ends name it. There is no per-run process to build and no fallback to
// describe: the ids and the address are the whole configuration surface.

// serverStreamModelID is the server entry that serves live partials.
func serverStreamModelID(cfg *config.Config) string {
	if id := strings.TrimSpace(cfg.ASRServerStreamModel); id != "" {
		return id
	}
	return server.DefaultStreamID
}

// serverModelID is the server entry the offline repair decode uses. It must not be
// the streaming entry: the server holds a model for the whole length of a request, so
// a repair sent to the id a live session owns waits for that session to end -
// measured at 10.9s behind a 12s dictation.
func serverModelID(cfg *config.Config) string {
	if id := strings.TrimSpace(cfg.ASRServerModel); id != "" {
		return id
	}
	return server.DefaultID
}

// serverDescription says which service and entries a session will use, for the
// startup log line and for `stream --events`.
func serverDescription(cfg *config.Config, repairEntry string) string {
	return fmt.Sprintf("server %s: streaming entry %s, repair entry %s (%s)",
		strings.TrimRight(cfg.ASRServerURL, "/"),
		serverStreamModelID(cfg),
		repairEntry,
		serverUnitName)
}
