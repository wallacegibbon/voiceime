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

// asrServerCheck reports on the resident server, which is the one prerequisite that
// dictation cannot work without and that nothing else in the install verifies: without
// it there is no streaming pass and no repair, because voiceime no longer starts
// processes of its own.
//
// One function rather than a block inside runDoctor, so its states can be tested
// without a machine that has audio.cpp built on it.
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

	// Something answers. The remaining question is whether it serves the entries the
	// client will ask for - a wrong id otherwise shows up per utterance as a repair
	// that fell back, or as a stream that never starts.
	//
	// The ids are constants now (server install writes them), so the only way they can
	// be wrong is a service built by another version; the listing is what says so.
	streamEntry, repairEntry := serverStreamModelID(), serverModelID()
	entries, err := server.Entries(ctx, raw)
	if err != nil {
		return Check{ID: "asr_server", Status: "fail", Detail: err.Error(), Fix: "voiceime server install"}
	}
	offered := make([]string, 0, len(entries))
	for _, entry := range entries {
		offered = append(offered, entry.ID)
	}
	for _, entry := range []string{streamEntry, repairEntry} {
		found := false
		for _, served := range entries {
			if served.ID == entry {
				found = true
			}
		}
		if !found {
			return Check{
				ID:     "asr_server",
				Status: "fail",
				Detail: fmt.Sprintf("the server has no entry %q (it offers: %s)", entry, strings.Join(offered, ", ")),
				Fix:    "voiceime server install",
			}
		}
	}

	// What it serves, not only what it calls itself. Changing a model means changing a
	// setting and re-running install, and the entry ids stay the same either way - so a
	// service still serving the previous checkpoint is invisible unless the paths are
	// compared. That is also the case where `server install` used to report success
	// while the old weights kept answering.
	host2, port, err := server.ParseURL(raw)
	if err == nil && server.IsLocalHost(host2) {
		if spec, specErr := buildServerSpec(cfg, scanned, port, "", ""); specErr == nil {
			if matchErr := entriesMatch(ctx, raw, spec); matchErr != nil {
				return Check{
					ID:     "asr_server",
					Status: "fail",
					Detail: matchErr.Error() + " (the service has not been restarted since the setting changed)",
					Fix:    "voiceime config apply",
				}
			}
		}
	}
	detail := fmt.Sprintf("%s is serving %s (partials) and %s (repair)", strings.TrimRight(raw, "/"), streamEntry, repairEntry)
	if entries != nil {
		// Name the files as well as the ids: `streaming_model` may be empty (meaning
		// "pick one"), and this is the line a user reads to find out what it picked.
		named := make([]string, 0, len(entries))
		for _, entry := range entries {
			if entry.Path == "" {
				continue
			}
			named = append(named, fmt.Sprintf("%s=%s", entry.ID, filepath.Base(entry.Path)))
		}
		if len(named) == 2 {
			detail = fmt.Sprintf("%s is serving %s", strings.TrimRight(raw, "/"), strings.Join(named, " and "))
		}
	}
	return Check{ID: "asr_server", Status: "ok", Detail: detail}
}
