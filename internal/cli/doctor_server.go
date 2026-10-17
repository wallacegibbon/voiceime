package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/wallacegibbon/voiceime/internal/asr"
	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/models"
	"github.com/wallacegibbon/voiceime/internal/server"
)

// asrServerCheck reports on the optional resident-server repair route.
//
// It exists because that route is the one part of a working installation that
// nothing else verifies: a machine configured for it but unable to start it still
// reports `doctor` clean, because dictation falls back to a process per utterance
// and keeps working. The fallback is the right behaviour and the wrong silence -
// the user asked for a faster route and would be paying for it with none of the
// benefit. The states worth distinguishing are "not in use" (the default, and not a
// problem), "up and offering the configured model", and "configured but cannot be
// started", which names the setting that is wrong.
//
// One function rather than a block inside runDoctor, so it can be tested against
// each of those states without a machine that has audio.cpp built on it.
func asrServerCheck(cfg *config.Config, scanned []models.Model) Check {
	raw := strings.TrimSpace(cfg.ASRServerURL)
	if raw == "" {
		return Check{
			ID:     "asr_server",
			Status: "ok",
			Detail: "off: one audiocpp_cli process per utterance (asr_server_url is empty)",
		}
	}
	modelID := strings.TrimSpace(cfg.ASRServerModel)
	if modelID == "" {
		modelID = server.DefaultID
	}

	host, _, err := server.ParseURL(raw)
	if err != nil {
		return Check{
			ID:     "asr_server",
			Status: "fail",
			Detail: err.Error(),
			Fix:    "voiceime config set asr_server_url http://127.0.0.1:18080",
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := server.Probe(ctx, raw); err == nil {
		// Something is listening, so the remaining question is whether it offers the
		// model a repair will ask for - the mistake a hand-written server config
		// makes, and one that otherwise surfaces per utterance as a repair that fell
		// back.
		if err := (&asr.ServerTranscriber{BaseURL: raw, Model: modelID}).Check(ctx); err != nil {
			return Check{
				ID:     "asr_server",
				Status: "fail",
				Detail: err.Error(),
				Fix:    "voiceime config set asr_server_model <the id that server offers>",
			}
		}
		detail := fmt.Sprintf("%s is serving model %s", strings.TrimRight(raw, "/"), modelID)
		if !server.IsLocalHost(host) {
			detail = "remote " + detail
		}
		return Check{ID: "asr_server", Status: "ok", Detail: detail}
	}

	if !cfg.ASRServerAutostart {
		return Check{
			ID:     "asr_server",
			Status: "warn",
			Detail: fmt.Sprintf("nothing is listening at %s and asr_server_autostart is off: repairs use a process per utterance", raw),
			Fix:    "voiceime config set asr_server_autostart true",
		}
	}
	if !server.IsLocalHost(host) {
		return Check{
			ID:     "asr_server",
			Status: "warn",
			Detail: fmt.Sprintf("nothing is listening at %s, and autostart only starts a server on this machine: repairs use a process per utterance", raw),
		}
	}

	// Local and autostarting: everything it needs has to be present before the first
	// session reaches for it.
	if _, err := cfg.FindServerBinary(); err != nil {
		return Check{
			ID:     "asr_server",
			Status: "fail",
			Detail: err.Error(),
			Fix:    "voiceime config set asr_bin /path/to/audio.cpp/build/<backend>/bin/audiocpp_cli",
		}
	}
	if strings.TrimSpace(cfg.RefineModel) == "" {
		return Check{
			ID:     "asr_server",
			Status: "fail",
			Detail: "asr_server_url is set but refine_model is empty: there is no model for the server to serve",
			Fix:    "voiceime config set refine_model /path/to/offline-model.gguf",
		}
	}
	found := models.Lookup(scanned, cfg.RefineModel)
	switch {
	case found == nil:
		return Check{
			ID:     "asr_server",
			Status: "fail",
			Detail: cfg.RefineModel + " is not a model voiceime can see, so the server cannot be started for it",
			Fix:    "voiceime models list",
		}
	case found.Error != "":
		return Check{
			ID:     "asr_server",
			Status: "fail",
			Detail: cfg.RefineModel + ": " + found.Error,
			Fix:    "voiceime models install " + models.RecommendedPackages()[0].ID,
		}
	case found.Family == "":
		return Check{
			ID:     "asr_server",
			Status: "fail",
			Detail: cfg.RefineModel + ": audio.cpp did not report a family, and the server requires one",
			Fix:    "voiceime models refresh",
		}
	}
	return Check{
		ID:     "asr_server",
		Status: "ok",
		Detail: fmt.Sprintf("started on demand at %s when dictation begins, serving %s (%s)", raw, found.Path, found.Family),
	}
}
