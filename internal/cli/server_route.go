package cli

import (
	"fmt"
	"strings"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// The resident server is the only decode route, so these three helpers are how the

// serverDescription names the service a front end talks to.
func serverDescription(cfg *config.Config) string {
	return fmt.Sprintf("server %s (%s)", strings.TrimRight(cfg.ASRServerURL, "/"), serverUnitName)
}
