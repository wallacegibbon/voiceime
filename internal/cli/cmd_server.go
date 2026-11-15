package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/model"
	"github.com/wallacegibbon/voiceime/internal/server"
)

// serverUnitName is the systemd user unit that keeps the recognition server running.
const serverUnitName = "voiceime-asr-server.service"

// serverConfigName is the audio.cpp config voiceime generates when it installs the service.
// It is an output, not an input: nothing reads it back except audio.cpp.
const serverConfigName = "asr-server.json"

func serverUnitPath() string {
	dir := config.UserUnitDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, serverUnitName)
}

// serverConfigFile is the generated audio.cpp config. It lives in the data directory rather
// than the runtime one because the unit starts at login: a file in a directory emptied at
// logout would not exist when systemd asks for it, so the install would work only after
// someone ran `voiceime apply` again. It stays out of the configuration directory because the
// only file a person edits belongs there.
func serverConfigFile() string {
	return filepath.Join(config.DataDir(), serverConfigName)
}

// serverUnit runs audio.cpp directly, on the config that `voiceime apply` rendered.
//
// It used to run `voiceime server run`, which read the settings, found the binary, rendered
// the audio.cpp config and then exec'd it - so every start re-derived what deployment already
// knew, and a start at login could fail for reasons only a re-run of apply could fix. Naming
// the binary and the file here is what "deployment resolved it once" means: the unit is the
// record of that decision, and `voiceime doctor` compares it with the machine.
func serverUnit(asrBinary, generatedConfig string) string {
	documentation := ""
	if link := documentationURL(); link != "" {
		documentation = "Documentation=" + link + "\n"
	}
	return fmt.Sprintf(`[Unit]
Description=voiceime recognition server (audio.cpp ASR, model resident)
%s
[Service]
Type=simple
ExecStart=%s --config %s --log
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
`, documentation, shellQuote(asrBinary), shellQuote(generatedConfig))
}

// documentationURL names the manual installed beside this binary, or "" when it is not
// there: `make install` puts docs/ next to bin/, and a checkout has it at the repository
// root. A link to a file that does not exist is worse than no link, so it is tested rather
// than assumed.
func documentationURL() string {
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	path := filepath.Join(filepath.Dir(self), "..", "docs", "AGENT.md")
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return "file://" + path
}

// buildServerSpec decides what the service serves: the named checkpoint, read from its own
// metadata for the family audio.cpp requires, on the given port, with the backend and thread
// count deployment resolved. It is only ever called by `voiceime apply` - nothing else has to
// know what a server config would contain, and `doctor` compares what is running with the
// settings instead.
func buildServerSpec(cfg *config.Config, port int, backend string, threads int) (server.Spec, error) {
	configured := strings.TrimSpace(cfg.Model)
	if configured == "" {
		return server.Spec{}, PreconditionError(noModelFix, "%s", noModelAdvice())
	}
	path := config.Expand(configured)
	// The family is not something to remember or re-derive: audio.cpp wrote it into the
	// file, and the server requires it.
	info, err := model.Open(path)
	if err != nil {
		return server.Spec{}, PreconditionError("voiceime config set model /path/to/model.gguf", "%v", err)
	}
	return server.Spec{
		Model:   path,
		Family:  info.Family,
		Port:    port,
		Backend: backend,
		Threads: threads,
	}, nil
}

// writeServerConfig renders the audio.cpp config and returns where it wrote it, so the one
// command that starts a service and the check that compares the unit with it resolve that
// path the same way.
func writeServerConfig(spec server.Spec) (string, error) {
	path := serverConfigFile()
	if err := server.WriteConfig(spec, path); err != nil {
		return "", err
	}
	return path, nil
}

// waitForModel waits until the service serves the checkpoint it was just given. /health
// alone is not enough: a server still running the previous config answers it too.
func waitForModel(ctx context.Context, baseURL string, spec server.Spec) error {
	deadline := time.Now().Add(120 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		last = modelMatches(ctx, baseURL, spec)
		if last == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		time.Sleep(500 * time.Millisecond)
	}
	return last
}

func modelMatches(ctx context.Context, baseURL string, spec server.Spec) error {
	entries, err := server.Entries(ctx, baseURL)
	if err != nil {
		return err
	}
	if len(entries) != 1 {
		return fmt.Errorf("the server serves %d models, want 1", len(entries))
	}
	entry := entries[0]
	if entry.ID != server.EntryID(spec.Model) {
		return fmt.Errorf("the server publishes %q, want %q", entry.ID, server.EntryID(spec.Model))
	}
	if filepath.Clean(entry.Path) != filepath.Clean(spec.Model) {
		return fmt.Errorf("entry %q is serving %s, not %s",
			entry.ID, filepath.Base(entry.Path), filepath.Base(spec.Model))
	}
	return nil
}

// shellQuote quotes a value for a shell-style word splitter, which is what systemd and
// gsettings both use.
func shellQuote(value string) string {
	if !strings.ContainsAny(value, " \t\"'") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
