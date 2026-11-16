package cli

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/wallacegibbon/voiceime/internal/asr"
	"github.com/wallacegibbon/voiceime/internal/config"
)

// unitInstalled reports whether a unit exists to start a model on this machine. It does not
// decide whether one is wanted - the settings' endpoint does that - it decides what a finding
// can say: "nothing here is installed to answer this" is a different situation from "the
// service is installed and not answering".
func unitInstalled() bool {
	path := unitPath()
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// endpointIsLocal answers the one question about an address that changes what a finding
// suggests: is the machine that has to answer this one the machine being looked at.
func endpointIsLocal(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return false
	}
	host := parsed.Hostname()
	switch host {
	case "localhost":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// asrEndpointCheck reports on the endpoint dictation posts to, and on the one thing that can
// make it unusable: no model id to name.
//
// The settings decide which kind of endpoint this is. A remote one names its model, and this
// command asks it nothing - a provider's /v1/models is a catalogue, not a promise about what
// this account accepts, and a working setup with a named model would fail a check that
// trusted the catalogue. A local one usually names nothing, and then the answer has to come
// from the endpoint, so the check asks it the question the engine will.
func asrEndpointCheck(cfg *config.Config, unitExists bool) Check {
	raw := strings.TrimSpace(cfg.ASRServerURL)
	if raw == "" {
		return Check{
			ID:     "asr_endpoint",
			Status: "fail",
			Detail: "no endpoint is configured, and dictation decodes through one",
			Fix:    `asr_server_url = "http://127.0.0.1:18080" in the settings file, then scripts/asr-service.sh /path/to/audiocpp_server /path/to/model.gguf`,
		}
	}
	base, err := asr.Endpoint(raw)
	if err != nil {
		return Check{ID: "asr_endpoint", Status: "fail", Detail: err.Error(), Fix: "asr_server_url in the settings"}
	}
	local := endpointIsLocal(raw)

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	model, err := asr.ModelToUse(ctx, base, cfg.Model, cfg.APIKey)
	if err != nil {
		fix := "voiceime transcribe --file <wav>"
		if local {
			switch {
			case unitExists:
				fix = "systemctl --user restart " + unitName
			default:
				fix = "scripts/asr-service.sh /path/to/audiocpp_server /path/to/model.gguf"
			}
		}
		return Check{
			ID:     "asr_endpoint",
			Status: "fail",
			Detail: fmt.Sprintf("%s: %v", base, err),
			Fix:    fix,
		}
	}
	detail := fmt.Sprintf("%s, and a request would name %q", base, model)
	if local {
		detail += " - a model on this machine, which scripts/asr-service.sh starts"
	}
	return Check{ID: "asr_endpoint", Status: "ok", Detail: detail}
}
