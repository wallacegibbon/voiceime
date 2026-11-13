package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/models"
	"github.com/wallacegibbon/voiceime/internal/server"
)

// serverUnitInstalled reports whether a unit exists to start the server, which decides
// which fix doctor offers: "start it" and "install it" are different repairs.
func serverUnitInstalled() bool {
	path := serverUnitPath()
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// asrServerCheck reports on the resident server, the one prerequisite dictation cannot
// work without.
func asrServerCheck(cfg *config.Config, scanned []models.Model, unitInstalled bool) Check {
	raw := strings.TrimSpace(cfg.ASRServerURL)
	if raw == "" {
		return Check{
			ID:     "asr_server",
			Status: "fail",
			Detail: "no server is configured, and dictation decodes through one",
			Fix:    "voiceime server install",
		}
	}
	host, _, err := server.ParseURL(raw)
	if err != nil {
		return Check{
			ID:     "asr_server",
			Status: "fail",
			Detail: err.Error(),
			Fix:    "voiceime server install",
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	probeErr := server.Probe(ctx, raw)
	if probeErr != nil {
		// The unit is what should be running it, so the advice depends on whether one
		// exists: "start it" and "install it" are different fixes.
		if unitInstalled {
			return Check{
				ID:     "asr_server",
				Status: "fail",
				Detail: fmt.Sprintf("%s is not answering: %v", strings.TrimRight(raw, "/"), probeErr),
				Fix:    "voiceime config apply",
			}
		}
		fix := "voiceime server install"
		if !server.IsLocalHost(host) {
			fix = "systemctl --user start " + serverUnitName + " on that machine"
		}
		return Check{
			ID:     "asr_server",
			Status: "fail",
			Detail: fmt.Sprintf("nothing is answering at %s, and no service on this machine is installed to answer it", raw),
			Fix:    fix,
		}
	}

	// Something answers. What is left is whether it serves one model, and whether that
	// model is the one the settings name: the id comes from the listing, so a service
	// left on another checkpoint is the failure that matters here.
	entries, err := server.Entries(ctx, raw)
	if err != nil {
		return Check{ID: "asr_server", Status: "fail", Detail: err.Error(), Fix: "voiceime server install"}
	}
	if len(entries) != 1 {
		offered := make([]string, 0, len(entries))
		for _, entry := range entries {
			offered = append(offered, entry.ID)
		}
		return Check{
			ID:     "asr_server",
			Status: "fail",
			Detail: fmt.Sprintf("%s serves %d models (%s); voiceime serves one",
				strings.TrimRight(raw, "/"), len(entries), strings.Join(offered, ", ")),
			Fix: "voiceime server install",
		}
	}
	detail := fmt.Sprintf("%s is serving %s", strings.TrimRight(raw, "/"), entries[0].ID)
	if entries[0].Path != "" {
		detail = fmt.Sprintf("%s is serving %s (%s)", strings.TrimRight(raw, "/"),
			entries[0].ID, filepath.Base(entries[0].Path))
	}
	if _, port, err := server.ParseURL(raw); err == nil && server.IsLocalHost(host) {
		if spec, specErr := buildServerSpec(cfg, scanned, port, ""); specErr == nil {
			if matchErr := modelMatches(ctx, raw, spec); matchErr != nil {
				return Check{
					ID:     "asr_server",
					Status: "fail",
					Detail: matchErr.Error() + " (the service has not been restarted since the setting changed)",
					Fix:    "voiceime config apply",
				}
			}
		}
	}
	return Check{ID: "asr_server", Status: "ok", Detail: detail}
}
