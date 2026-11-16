package cli

import (
	"context"
	"errors"
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

// asrEndpointCheck reports on the endpoint dictation posts to: whether it can be reached at
// all, whether it accepts the credential, and what a request would name there.
//
// The settings decide which of the two questions about naming is asked. A setup that names a
// model is not cross-checked against the endpoint's list - a provider's /v1/models is a
// catalogue, not a promise about what this account accepts - but the endpoint is still asked,
// because its answer's status is the only evidence there is about the credential, and a
// refused credential is the failure a remote setup actually meets. A setup that names nothing
// has to be told, so the check asks the endpoint the question the engine will.
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
	named := strings.TrimSpace(cfg.Model)
	model, err := asr.ModelToUse(ctx, base, cfg.Model, cfg.APIKey)
	if err == nil && named != "" {
		// The model itself is not cross-checked with the endpoint's list, but the credential
		// it would travel with is tested, on the route the request would use.
		err = asr.CheckCredential(ctx, cfg.ASRServerURL, named, cfg.APIKey)
	}
	if err != nil {
		var refused *asr.CredentialError
		fix := fmt.Sprintf("curl -s %s/v1/models", base)
		switch {
		case errors.As(err, &refused):
			// The endpoint is there and said no to the credential: that is a settings file,
			// not a service.
			fix = "voiceime config set api_key <the token this endpoint gave you>"
		case local && unitExists:
			fix = "systemctl --user restart " + unitName
		case local:
			fix = "scripts/asr-service.sh /path/to/audiocpp_server /path/to/model.gguf"
		}
		return Check{
			ID:     "asr_endpoint",
			Status: "fail",
			Detail: fmt.Sprintf("%s: %v", base, err),
			Fix:    fix,
		}
	}
	detail := fmt.Sprintf("%s, and a request would name %q", base, model)
	if named == "" {
		detail += " - the endpoint itself named it"
	} else {
		detail += " - named by the settings, and its credential was accepted"
	}
	return Check{ID: "asr_endpoint", Status: "ok", Detail: detail}
}
