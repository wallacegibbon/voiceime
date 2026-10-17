package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/wallacegibbon/voiceime/internal/asr"
	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/models"
	"github.com/wallacegibbon/voiceime/internal/server"
)

// serverUnitName is the systemd user unit that keeps the repair server running.
const serverUnitName = "voiceime-asr-server.service"

// cmdServer owns the resident half of the recognition pipeline: one service that
// holds both models, so a dictation session opens a connection instead of starting a
// process and waiting for weights.
//
// A systemd user unit rather than a process voiceime spawns, and that is the
// simplification: the models are loaded at startup, nothing on the request path ever
// waits for them, and no code has to remember whether it started something, warm it,
// or stop it again. The shape is the one the shortcut listener already uses.
func cmdServer() *Command {
	var port int
	var streamModel, repairModel string
	return &Command{
		Name:    "server",
		Summary: "Install, remove or inspect the resident recognition server",
		Usage:   "voiceime server <install|uninstall|status> [--port N] [--stream-model PATH] [--repair-model PATH]",
		Flags: func(set *flag.FlagSet) {
			set.IntVar(&port, "port", server.DefaultPort, "port the server listens on")
			set.StringVar(&streamModel, "stream-model", "", "GGUF for live partials (default: streaming_model, or the best installed streaming model)")
			set.StringVar(&repairModel, "repair-model", "", "GGUF for the offline repair (default: refine_model, or the streaming model)")
		},
		Run: func(ctx *Context, args []string) error {
			action := "status"
			if len(args) > 0 {
				action = args[0]
			}
			switch action {
			case "install":
				return runServerInstall(ctx, port, streamModel, repairModel)
			case "uninstall":
				return runServerUninstall(ctx)
			case "status":
				return runServerStatus(ctx)
			default:
				return UsageError("unknown server action %q", action)
			}
		},
	}
}

func serverUnitPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "systemd", "user", serverUnitName)
}

// serverConfigFile is where the generated server config lives. `server install` writes
// it and names it on the unit's command line, so changing what the server serves means
// running install again rather than editing this by hand.
func serverConfigFile() string {
	return filepath.Join(config.ConfigDir(), "asr-server.json")
}

func runServerInstall(ctx *Context, port int, streamFlag, repairFlag string) error {
	cfg := ctx.Config
	scanned, err := models.Scan(cfg, false, nil)
	if err != nil {
		return FailureError("%v", err)
	}
	spec, err := buildServerSpec(cfg, scanned, port, streamFlag, repairFlag)
	if err != nil {
		return PreconditionError("voiceime models refresh", "%v", err)
	}
	binary, err := cfg.FindServerBinary()
	if err != nil {
		return PreconditionError("voiceime config set asr_bin /path/to/audio.cpp/build/<backend>/bin/audiocpp_cli",
			"%v", err)
	}
	streamPath, repairPath := spec.StreamModel, spec.RepairModel
	streamFamily, repairFamily := spec.StreamFamily, spec.RepairFamily
	configPath := serverConfigFile()
	if err := server.WriteConfig(spec, configPath); err != nil {
		return FailureError("%v", err)
	}

	unitPath := serverUnitPath()
	if unitPath == "" {
		return FailureError("cannot determine where to write the unit file")
	}
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return FailureError("%v", err)
	}
	if err := os.WriteFile(unitPath, []byte(serverUnit(binary, configPath)), 0o644); err != nil {
		return FailureError("%v", err)
	}

	// Point voiceime at the service it just installed: a user who runs this wants
	// dictation to work, and `config set` is the documented way to undo it.
	baseURL := spec.BaseURL()
	changes := []struct{ key, value string }{
		{"asr_server_url", baseURL},
		{"asr_server_stream_model", spec.StreamIDOrDefault()},
		{"asr_server_model", spec.RepairIDOrDefault()},
	}
	for _, change := range changes {
		if err := cfg.Set(change.key, change.value); err != nil {
			return FailureError("config set %s: %v", change.key, err)
		}
	}
	if err := config.Save(cfg, ctx.ConfigPath); err != nil {
		return FailureError("write %s: %v", ctx.ConfigPath, err)
	}

	if !ctx.JSON {
		ctx.Print("wrote %s", configPath)
		ctx.Print("wrote %s", unitPath)
		ctx.Print("serving %s (%s) for partials and %s (%s) for repair",
			streamPath, streamFamily, repairPath, repairFamily)
		ctx.Print("config: asr_server_url = %s", baseURL)
	}
	// enable for the next login, restart for this one: `enable --now` starts a unit that
	// is stopped and does nothing to one that is running, which would leave a changed
	// model unserved while reporting success.
	commands := []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable " + serverUnitName,
		"systemctl --user restart " + serverUnitName,
	}
	applied := false
	if _, err := exec.LookPath("systemctl"); err == nil {
		for _, command := range commands {
			if err := runCommand(command); err != nil {
				// Not fatal: outside a systemd session the unit is still correct for
				// the next login, which is the same compromise the listener makes.
				if !ctx.JSON {
					ctx.Print("could not run `%s`: %v", command, err)
				}
				continue
			}
			applied = true
		}
	}
	if !applied && !ctx.JSON {
		ctx.Print("start it by hand with: %s --config %s", binary, configPath)
	}

	// `systemctl enable --now` returns before the service has done anything, so
	// reporting success on its exit status would be reporting a guess. The server has
	// to load its models before it answers, and the failures worth catching - a port
	// already taken, a model the build cannot load - only exist afterwards.
	if applied {
		probeCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		err := waitForEntries(probeCtx, baseURL, spec)
		cancel()
		if err != nil {
			return FailureError("the service was installed but is not serving: %v (see `systemctl --user status %s`)",
				err, serverUnitName)
		}
		if !ctx.JSON {
			ctx.Print("serving at %s", baseURL)
		}
	}
	return ctx.Emit("server", map[string]any{
		"config":       configPath,
		"unit":         unitPath,
		"binary":       binary,
		"stream_model": streamPath,
		"repair_model": repairPath,
		"url":          baseURL,
		"active":       applied,
	})
}

// waitForEntries waits until the service serves the models it was just given.
//
// Waiting for /health would not be enough: a server that is still running the previous
// config answers it too, so install would report success while the old weights kept
// serving every request.
func waitForEntries(ctx context.Context, baseURL string, spec server.Spec) error {
	deadline := time.Now().Add(120 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		last = entriesMatch(ctx, baseURL, spec)
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

// entriesMatch reports whether the server serves the two checkpoints the spec names.
func entriesMatch(ctx context.Context, baseURL string, spec server.Spec) error {
	entries, err := server.Entries(ctx, baseURL)
	if err != nil {
		return err
	}
	byID := map[string]string{}
	for _, entry := range entries {
		byID[entry.ID] = filepath.Clean(entry.Path)
	}
	want := map[string]string{
		spec.StreamIDOrDefault(): filepath.Clean(spec.StreamModel),
		spec.RepairIDOrDefault(): filepath.Clean(spec.RepairModel),
	}
	for id, path := range want {
		served, found := byID[id]
		switch {
		case !found:
			return fmt.Errorf("the server does not serve an entry called %q", id)
		case served != path:
			return fmt.Errorf("entry %q is serving %s, not %s", id, filepath.Base(served), filepath.Base(path))
		}
	}
	return nil
}

// buildServerSpec decides which checkpoints the two entries serve. It is shared with
// doctor so the check can say what *should* be running.
func buildServerSpec(cfg *config.Config, scanned []models.Model, port int, streamFlag, repairFlag string) (server.Spec, error) {
	streamPath := modelPath(cfg, scanned, streamFlag, cfg.StreamingModel, false)
	if streamPath == "" {
		streamPath = modelPath(cfg, scanned, "", "", true)
	}
	repairPath := modelPath(cfg, scanned, repairFlag, cfg.RefineModel, true)
	switch {
	case streamPath == "" && repairPath == "":
		return server.Spec{}, fmt.Errorf("no model to serve: install one first")
	case streamPath == "":
		streamPath = repairPath
	case repairPath == "":
		repairPath = streamPath
	}
	streamFamily, err := modelFamily(scanned, streamPath)
	if err != nil {
		return server.Spec{}, err
	}
	repairFamily, err := modelFamily(scanned, repairPath)
	if err != nil {
		return server.Spec{}, err
	}
	return server.Spec{
		StreamModel:  streamPath,
		StreamFamily: streamFamily,
		RepairModel:  repairPath,
		RepairFamily: repairFamily,
		StreamID:     serverStreamModelID(cfg),
		RepairID:     serverModelID(cfg),
		Host:         "127.0.0.1",
		Port:         port,
		Backend:      cfg.Backend,
		Threads:      cfg.Threads,
		ChunkSeconds: cfg.ChunkSeconds,
		// The decode contract the repair route has always used, in the one place a
		// multipart request can carry it.
		RequestOptions: asr.RepairRequestOptions(cfg.RequestOptions),
	}, nil
}

// serverUnit renders the systemd user unit that keeps the server running.
//
// Two things in it are load-bearing: ExecStart names the generated config (a server
// started without one has no models to serve), and Documentation= names the agent
// guide beside the binary - `make install` puts the docs at <prefix>/docs/, and a path
// with nothing behind it is a dead link systemd cannot report.
func serverUnit(binary, configPath string) string {
	return fmt.Sprintf(`[Unit]
Description=voiceime recognition server (audio.cpp ASR, models resident)
Documentation=file://%s

[Service]
Type=simple
ExecStart=%s --config %s
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
`, filepath.Join(filepath.Dir(binary), "..", "docs", "AGENT.md"), binary, configPath)
}

func runServerUninstall(ctx *Context) error {
	unitPath := serverUnitPath()
	stopped := false
	if _, err := exec.LookPath("systemctl"); err == nil {
		if err := runCommand("systemctl --user disable --now " + serverUnitName); err == nil {
			stopped = true
		}
	}
	removed := []string{}
	for _, path := range []string{unitPath, serverConfigFile()} {
		if path == "" {
			continue
		}
		if err := os.Remove(path); err == nil {
			removed = append(removed, path)
		}
	}
	if _, err := exec.LookPath("systemctl"); err == nil {
		_ = runCommand("systemctl --user daemon-reload")
	}
	// Leave the configuration usable rather than pointing at a service that is gone.
	if ctx.Config.ASRServerURL != "" {
		if err := ctx.Config.Set("asr_server_url", ""); err == nil {
			_ = config.Save(ctx.Config, ctx.ConfigPath)
			removed = append(removed, "asr_server_url cleared")
		}
	}
	if !ctx.JSON {
		for _, entry := range removed {
			ctx.Print("removed %s", entry)
		}
		if !stopped {
			ctx.Print("the service was not running, or systemd is unavailable")
		}
	}
	return ctx.Emit("server", map[string]any{"removed": removed, "stopped": stopped})
}

func runServerStatus(ctx *Context) error {
	cfg := ctx.Config
	status := map[string]any{
		"unit":         serverUnitPath(),
		"config":       serverConfigFile(),
		"url":          cfg.ASRServerURL,
		"stream_entry": serverStreamModelID(cfg),
		"repair_entry": serverModelID(cfg),
		"unit_active":  false,
		"listening":    false,
	}
	if _, err := os.Stat(serverUnitPath()); err == nil {
		status["installed"] = true
	}
	if _, err := exec.LookPath("systemctl"); err == nil {
		if out, err := exec.Command("systemctl", "--user", "is-active", serverUnitName).Output(); err == nil {
			status["unit_active"] = true
			status["unit_state"] = strings.TrimSpace(string(out))
		}
	}
	base := cfg.ASRServerURL
	if base == "" {
		base = fmt.Sprintf("http://127.0.0.1:%d", server.DefaultPort)
	}
	probeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Probe(probeCtx, base); err == nil {
		status["listening"] = true
		served := []string{}
		for _, entry := range []string{serverStreamModelID(cfg), serverModelID(cfg)} {
			transcriber := asr.ServerTranscriber{BaseURL: base, Model: entry}
			if err := transcriber.Check(probeCtx); err != nil {
				status["model_error"] = err.Error()
				continue
			}
			served = append(served, entry)
		}
		status["entries_served"] = served
	}
	return ctx.Emit("server", status)
}

// modelPath resolves a model argument the way the rest of the CLI does: the flag, then
// the setting, then (optionally) the model auto-selection would pick.
func modelPath(cfg *config.Config, scanned []models.Model, flagValue, setting string, pick bool) string {
	for _, candidate := range []string{flagValue, setting} {
		if strings.TrimSpace(candidate) != "" {
			return config.Expand(candidate)
		}
	}
	if !pick {
		return ""
	}
	// "Empty means pick automatically", the same rule the rest of the CLI uses.
	if picked := models.PickStreaming(scanned, cfg.Language); picked != nil {
		return picked.Path
	}
	return ""
}

// modelFamily reports a model's audio.cpp family, which the server requires. It comes
// from the CLI's own inspection, cached against the file's size and mtime, because
// guessing it from a file name is the kind of guess this project does not make.
func modelFamily(scanned []models.Model, path string) (string, error) {
	found := models.Lookup(scanned, path)
	switch {
	case found == nil:
		return "", fmt.Errorf("%s is not a model voiceime can see; check models_dir", path)
	case found.Error != "":
		return "", fmt.Errorf("%s: %s", path, found.Error)
	case found.Family == "":
		return "", fmt.Errorf("%s: audio.cpp did not report a family", path)
	}
	return found.Family, nil
}
