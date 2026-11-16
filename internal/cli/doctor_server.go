package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/wallacegibbon/voiceime/internal/asr"
	"github.com/wallacegibbon/voiceime/internal/config"
)

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

// asrEndpointCheck reports on the address dictation posts to: can it be reached, does it
// accept the credential, and is there a model id to name in the request. Both of the last two
// come from the settings - the program never asks an address what it serves - so the finding
// can be read as what the engine would do, and the probe is the same request with ten
// milliseconds of silence in place of the audio.
//
// `service` is the model service installed on this machine, if any. It changes only what a
// failure suggests: an address on this machine that does not answer is a service to restart, and
// one that was never installed is a command to run.
func asrEndpointCheck(cfg *config.Config, service modelService) Check {
	const id = "asr_endpoint"
	raw := strings.TrimSpace(cfg.ASRServerURL)
	if raw == "" {
		return Check{
			ID:     id,
			Status: "fail",
			Detail: "no address is configured, and dictation posts one recording to one",
			Fix: cfg.FixLine("asr_server_url", `"http://127.0.0.1:18080/v1/audio/transcriptions"`) +
				"   # or " + installModelService + " for a model on this machine",
		}
	}
	url, err := asr.RequestURL(raw)
	if err != nil {
		return Check{ID: id, Status: "fail", Detail: err.Error(), Fix: cfg.FixLine("asr_server_url", `"the whole address, path included"`)}
	}
	named := strings.TrimSpace(cfg.Model)
	local := endpointIsLocal(raw)
	if named == "" {
		fix := cfg.FixLine("model", `"<id>"`)
		if local {
			fix += "   # or the id " + installModelService + " prints when it starts a model here"
		}
		return Check{
			ID:     id,
			Status: "fail",
			Detail: fmt.Sprintf("%s is configured, and nothing names the model to ask it for", url),
			Fix:    fix,
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := asr.CheckCredential(ctx, url, named, cfg.APIKey); err != nil {
		var refused *asr.CredentialError
		fix := fmt.Sprintf("curl -s %s", url)
		switch {
		case errors.As(err, &refused):
			// The address is there and said no to the credential: that is a settings file,
			// not a service.
			fix = cfg.FixLine("api_key", `"the token this endpoint gave you"`)
		case local && service.Unit != "":
			fix = "systemctl --user restart " + service.Unit
		case local:
			fix = installModelService
		}
		return Check{ID: id, Status: "fail", Detail: fmt.Sprintf("%s: %v", url, err), Fix: fix}
	}
	return Check{
		ID:     id,
		Status: "ok",
		Detail: fmt.Sprintf("%s, and a request would name %q - the address and the id are used exactly as configured, and the credential was accepted", url, named),
	}
}
