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

// serverStreamModelID and serverModelID are the entry ids the generated config uses.
//
// They were settings, so that a server someone else runs could name its entries
// anything. That made four names for two models - two checkpoint paths and two ids - and
// the ids were never anything a user wanted to choose: `voiceime server run` writes them,
// so they are constants now. A server with different names is reported by name in
// `doctor` and `server status` rather than accommodated.
func serverStreamModelID() string { return server.DefaultStreamID }

// serverModelID is the server entry the offline repair decode uses. It must not be
// the streaming entry: the server holds a model for the whole length of a request, so
// a repair sent to the id a live session owns waits for that session to end -
// measured at 10.9s behind a 12s dictation.
func serverModelID() string { return server.DefaultID }

// serverDescription says which service and entries a session will use, for the
// startup log line and for `stream --events`.
func serverDescription(cfg *config.Config) string {
	return fmt.Sprintf("server %s: streaming entry %s, repair entry %s (%s)",
		strings.TrimRight(cfg.ASRServerURL, "/"),
		serverStreamModelID(),
		serverModelID(),
		serverUnitName)
}
