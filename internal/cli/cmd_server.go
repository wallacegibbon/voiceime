package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/wallacegibbon/voiceime/internal/asr"
	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/models"
	"github.com/wallacegibbon/voiceime/internal/server"
)

// serverUnitName is the systemd user unit that keeps the recognition server running.
const serverUnitName = "voiceime-asr-server.service"

// cmdServer owns the resident half of the pipeline: one service that holds the model, so
// a dictation is one request instead of one model load.
func cmdServer() *Command {
	var port int
	var modelPath string
	return &Command{
		Name:    "server",
		Summary: "Install, remove or inspect the resident recognition server",
		Usage:   "voiceime server <install|uninstall|status|restart|run> [--port N] [--model PATH]",
		Flags: func(set *flag.FlagSet) {
			set.IntVar(&port, "port", server.DefaultPort, "port the server listens on")
			set.StringVar(&modelPath, "model", "", "GGUF to serve (default: the model setting, or the best installed one)")
		},
		Run: func(ctx *Context, args []string) error {
			action := "status"
			if len(args) > 0 {
				action = args[0]
			}
			switch action {
			case "install":
				return runServerInstall(ctx, port, modelPath)
			case "run":
				return runServerForeground(ctx, port, modelPath)
			case "restart":
				return runServerRestart(ctx)
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
	dir := config.UserUnitDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, serverUnitName)
}

// serverConfigFile is the generated audio.cpp config. `server run` rewrites it from the
// settings at every start, so it never has to be edited.
func serverConfigFile() string {
	return filepath.Join(config.ConfigDir(), "asr-server.json")
}

func runServerInstall(ctx *Context, port int, modelFlag string) error {
	cfg := ctx.Config
	scanned, err := cachedModels(ctx)
	if err != nil {
		return err
	}
	spec, err := buildServerSpec(cfg, scanned, port, modelFlag)
	if err != nil {
		return err
	}
	binary, err := cfg.FindServerBinary()
	if err != nil {
		return PreconditionError("voiceime config set asr_bin /path/to/audio.cpp/build/<backend>/bin/audiocpp_cli",
			"%v", err)
	}
	unitPath := serverUnitPath()
	if unitPath == "" {
		return FailureError("cannot determine where to write the unit file")
	}

	// Settings first, then the files that describe them: a failed save must not leave a
	// unit pointing at a configuration that never named the service.
	baseURL := spec.BaseURL()
	for _, change := range []struct{ key, value string }{
		{"asr_server_url", baseURL},
		{"model", spec.Model},
	} {
		if err := cfg.Set(change.key, change.value); err != nil {
			return FailureError("config set %s: %v", change.key, err)
		}
	}
	if err := config.Save(cfg, ctx.ConfigPath); err != nil {
		return FailureError("write %s: %v", ctx.ConfigPath, err)
	}

	configPath := serverConfigFile()
	if err := server.WriteConfig(spec, configPath); err != nil {
		return FailureError("%v", err)
	}
	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return FailureError("%v", err)
	}
	if err := os.WriteFile(unitPath, []byte(serverUnit(absoluteConfigPath(ctx))), 0o644); err != nil {
		return FailureError("%v", err)
	}

	if !ctx.JSON {
		ctx.Print("wrote %s", configPath)
		ctx.Print("wrote %s", unitPath)
		ctx.Print("serving %s (%s) as %q", spec.Model, spec.Family, server.EntryID(spec.Model))
		ctx.Print("recorded in config: model, asr_server_url = %s", baseURL)
	}
	commands := []string{
		"systemctl --user daemon-reload",
		"systemctl --user enable " + serverUnitName,
		"systemctl --user restart " + serverUnitName,
	}
	applied := false
	if _, err := exec.LookPath("systemctl"); err == nil {
		for _, command := range commands {
			if err := runCommand(command); err != nil {
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

	// The unit is started, not serving: waiting for the model to load is what turns
	// "installed" into a fact.
	if applied {
		probeCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		err := waitForModel(probeCtx, baseURL, spec)
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
		"config": configPath,
		"unit":   unitPath,
		"binary": binary,
		"model":  spec.Model,
		"entry":  server.EntryID(spec.Model),
		"url":    baseURL,
		"active": applied,
	})
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

// serverUnit renders the systemd user unit. It runs voiceime, not audio.cpp, so the
// server config is always rendered from the settings at startup.
func serverUnit(configPath string) string {
	self, err := os.Executable()
	if err != nil {
		self = "voiceime"
	}
	documentation := ""
	if link := documentationURL(); link != "" {
		documentation = "Documentation=" + link + "\n"
	}
	return fmt.Sprintf(`[Unit]
Description=voiceime recognition server (audio.cpp ASR, model resident)
%s
[Service]
Type=simple
ExecStart=%s server run --config %s
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
`, documentation, self, shellQuote(configPath))
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

// runServerForeground is what the unit's ExecStart runs: write the config that matches
// the settings, then become the server.
func runServerForeground(ctx *Context, port int, modelFlag string) error {
	cfg := ctx.Config
	scanned, err := cachedModels(ctx)
	if err != nil {
		return err
	}
	spec, err := buildServerSpec(cfg, scanned, port, modelFlag)
	if err != nil {
		return err
	}
	path := serverConfigFile()
	if err := server.WriteConfig(spec, path); err != nil {
		return FailureError("%v", err)
	}
	binary, err := cfg.FindServerBinary()
	if err != nil {
		return PreconditionError("voiceime config set asr_bin /path/to/audio.cpp/build/<backend>/bin/audiocpp_cli",
			"%v", err)
	}
	if !ctx.JSON {
		ctx.Print("serving %s (config %s)", filepath.Base(spec.Model), path)
	}

	cmd := exec.Command(binary, "--config", path, "--log")
	cmd.Stdout = ctx.Stdout
	cmd.Stderr = ctx.Stderr
	if err := cmd.Start(); err != nil {
		return FailureError("start %s: %v", binary, err)
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			return FailureError("%s exited: %v", filepath.Base(binary), err)
		}
		return nil
	case <-signals:
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
		}
		return nil
	}
}

// runServerRestart applies a changed setting.
func runServerRestart(ctx *Context) error {
	if _, err := os.Stat(serverUnitPath()); err != nil {
		return PreconditionError("voiceime server install", "no service is installed yet")
	}
	if err := runCommand("systemctl --user restart " + serverUnitName); err != nil {
		return FailureError("systemctl --user restart %s: %v", serverUnitName, err)
	}
	scanned, err := models.Cached(ctx.Config)
	if err != nil {
		return FailureError("%v", err)
	}
	_, port, err := server.ParseURL(ctx.Config.ASRServerURL)
	if err != nil {
		return PreconditionError("voiceime server install", "%v", err)
	}
	spec, err := buildServerSpec(ctx.Config, scanned, port, "")
	if err != nil {
		return PreconditionError("voiceime server install", "%v", err)
	}
	probeCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := waitForModel(probeCtx, ctx.Config.ASRServerURL, spec); err != nil {
		return FailureError("%v", err)
	}
	if !ctx.JSON {
		ctx.Print("restarted; serving %s", filepath.Base(spec.Model))
	}
	return ctx.Emit("server", map[string]any{
		"restarted": true,
		"model":     spec.Model,
		"entry":     server.EntryID(spec.Model),
	})
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
		"unit":        serverUnitPath(),
		"config":      serverConfigFile(),
		"url":         cfg.ASRServerURL,
		"unit_active": false,
		"listening":   false,
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
		if entries, err := server.Entries(probeCtx, base); err == nil {
			served := make([]map[string]string, 0, len(entries))
			for _, entry := range entries {
				served = append(served, map[string]string{
					"id":   entry.ID,
					"mode": entry.Mode,
					"file": filepath.Base(entry.Path),
					"path": entry.Path,
				})
			}
			status["served"] = served
		} else {
			status["model_error"] = err.Error()
		}
	}
	return ctx.Emit("server", status)
}

// buildServerSpec decides which checkpoint the service serves. Shared with doctor, so the
// check can say what should be running.
func buildServerSpec(cfg *config.Config, scanned []models.Model, port int, modelFlag string) (server.Spec, error) {
	configured := modelFlag
	if strings.TrimSpace(configured) == "" {
		configured = cfg.Model
	}
	resolved := models.Resolve(scanned, cfg.Language, configured)
	if resolved.Model == "" {
		if pending := models.Uninspected(scanned); len(pending) > 0 {
			return server.Spec{}, PreconditionError("voiceime models list --refresh",
				"%d model file(s) have not been inspected yet", len(pending))
		}
		recommendation := models.RecommendedPackages()[0]
		return server.Spec{}, PreconditionError("voiceime models install "+recommendation.ID,
			"no model to serve: install one first   # %s", recommendation.Reason)
	}
	family, err := modelFamily(scanned, resolved.Model)
	if err != nil {
		return server.Spec{}, err
	}
	return server.Spec{
		Model:          resolved.Model,
		Family:         family,
		Port:           port,
		Backend:        cfg.Backend,
		Threads:        cfg.Threads,
		RequestOptions: asr.RequestOptions(cfg.RequestOptions),
	}, nil
}

// modelFamily reports a model's audio.cpp family, which the server requires.
func modelFamily(scanned []models.Model, path string) (string, error) {
	found := models.Lookup(scanned, path)
	switch {
	case found == nil:
		return "", PreconditionError("voiceime models list",
			"%s is not a model voiceime can see; check models_dir", path)
	case !found.Inspected:
		return "", PreconditionError("voiceime models list --refresh",
			"%s has not been inspected yet", path)
	case found.Error != "":
		return "", PreconditionError("voiceime models list", "%s: %s", path, found.Error)
	case found.Family == "":
		return "", FailureError("%s: audio.cpp did not report a family", path)
	}
	return found.Family, nil
}

// cachedModels reads the inspection cache, turning a missing models directory into the
// command that creates it.
func cachedModels(ctx *Context) ([]models.Model, error) {
	cached, err := models.Cached(ctx.Config)
	if err != nil {
		return nil, PreconditionError("voiceime models install "+models.RecommendedPackages()[0].ID, "%v", err)
	}
	return cached, nil
}

// shellQuote quotes a value for a shell-style word splitter, which is what systemd and
// gsettings both use.
func shellQuote(value string) string {
	if !strings.ContainsAny(value, " \t\"'") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
