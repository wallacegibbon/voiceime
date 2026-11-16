package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/wallacegibbon/voiceime/internal/asr"
	"github.com/wallacegibbon/voiceime/internal/config"
)

// asrEndpointCheck reports on the address dictation posts to: is there one, is there a model id
// to name in the request, can it be reached, does it accept the credential. The address is all
// the engine has, and this finding is the same request with ten milliseconds of silence in place
// of the audio, so what it says can be read as what the engine would do.
//
// What it does not ask is what kind of thing is behind the address, or who was meant to have
// started it. An address that does not answer is reported as an address that does not answer:
// the program has no way to know whether anything was supposed to be listening, and inventing a
// cause would be a guess about someone else's machine.
func asrEndpointCheck(cfg *config.Config) Check {
	const id = "asr_endpoint"
	raw := strings.TrimSpace(cfg.ASRServerURL)
	if raw == "" {
		return Check{
			ID:     id,
			Status: "fail",
			Detail: "no address is configured, and dictation posts one recording to one",
			Fix:    cfg.FixLine("asr_server_url", `"the whole address, path included"`),
		}
	}
	url, err := asr.RequestURL(raw)
	if err != nil {
		return Check{ID: id, Status: "fail", Detail: err.Error(), Fix: cfg.FixLine("asr_server_url", `"the whole address, path included"`)}
	}
	named := strings.TrimSpace(cfg.Model)
	if named == "" {
		// An address and a name to ask it for are one decision: nothing here asks an address
		// what it serves, so an address on its own cannot be used.
		return Check{
			ID:     id,
			Status: "fail",
			Detail: fmt.Sprintf("%s is configured, and nothing names the model to ask it for", url),
			Fix:    cfg.FixLine("model", `"<id>"`),
		}
	}

	// The same ceiling a dictation would be given, from the same setting. A first request to a
	// model that is not loaded pays for loading inside itself - measured here at 4.12 s for one
	// checkpoint and 1.3 s for another - so a probe with a shorter patience of its own would
	// call an address broken at exactly the moment a dictation would have worked. An address
	// with nothing behind it still answers in milliseconds; only a stalled one costs the wait.
	ctx, cancel := context.WithTimeout(context.Background(), cfg.ASRServerTimeout())
	defer cancel()
	if err := asr.CheckCredential(ctx, url, named, cfg.APIKey); err != nil {
		var refused *asr.CredentialError
		fix := fmt.Sprintf("curl -s %s", url)
		if errors.As(err, &refused) {
			// The address is there and said no to the credential: that is a settings file.
			fix = cfg.FixLine("api_key", `"the token this endpoint gave you"`)
		}
		return Check{ID: id, Status: "fail", Detail: fmt.Sprintf("%s: %v", url, err), Fix: fix}
	}
	return Check{
		ID:     id,
		Status: "ok",
		Detail: fmt.Sprintf("%s, and a request would name %q - the address and the id are used exactly as configured, and the credential was accepted", url, named),
	}
}
