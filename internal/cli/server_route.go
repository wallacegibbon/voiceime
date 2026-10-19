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

// serverDescription says which service and entries a session will use, for the
// startup log line and for `stream --events`.
func serverDescription(cfg *config.Config) string {
	return fmt.Sprintf("server %s: streaming entry %s, repair entry %s (%s)",
		strings.TrimRight(cfg.ASRServerURL, "/"),
		server.DefaultStreamID,
		server.DefaultID,
		serverUnitName)
}
