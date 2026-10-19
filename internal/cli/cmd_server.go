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
		Usage:   "voiceime server <install|uninstall|status|restart|run> [--port N] [--stream-model PATH] [--repair-model PATH]",
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
			case "run":
				// What the unit starts: render the config from the settings, then be the
				// server. Kept separate from install so that a setting change needs only
				// a restart.
				return runServerForeground(ctx, port, streamModel, repairModel)
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

// serverConfigFile is where the generated server config lives. `server install` writes
// it and names it on the unit's command line, so changing what the server serves means
// running install again rather than editing this by hand.
func serverConfigFile() string {
	return filepath.Join(config.ConfigDir(), "asr-server.json")
}

func runServerInstall(ctx *Context, port int, streamFlag, repairFlag string) error {
	cfg := ctx.Config
	scanned, err := cachedModels(ctx)
	if err != nil {
		return err
	}
	spec, err := buildServerSpec(cfg, scanned, port, streamFlag, repairFlag)
	if err != nil {
		return err
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
	if err := os.WriteFile(unitPath, []byte(serverUnit(binary, absoluteConfigPath(ctx))), 0o644); err != nil {
		return FailureError("%v", err)
	}

	// Point voiceime at the service it just installed: a user who runs this wants
	// dictation to work, and `config set` is the documented way to undo it.
	baseURL := spec.BaseURL()
	changes := []struct{ key, value string }{
		{"asr_server_url", baseURL},
		{"streaming_model", spec.StreamModel},
		{"refine_model", spec.RepairModel},
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
		ctx.Print("recorded in config: streaming_model, refine_model, asr_server_url = %s", baseURL)
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
		server.DefaultStreamID: filepath.Clean(spec.StreamModel),
		server.DefaultID:       filepath.Clean(spec.RepairModel),
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
	// A flag overrides the setting it duplicates; the resolution itself is models.Resolve,
	// so the service serves what `config show` and `models list` report (D32).
	streamSetting, repairSetting := streamFlag, repairFlag
	if strings.TrimSpace(streamSetting) == "" {
		streamSetting = cfg.StreamingModel
	}
	if strings.TrimSpace(repairSetting) == "" {
		repairSetting = cfg.RefineModel
	}
	resolved := models.Resolve(scanned, cfg.Language, streamSetting, repairSetting)
	streamPath, repairPath := resolved.Stream, resolved.Repair
	switch {
	case streamPath == "" && repairPath == "":
		// "Install one first" is wrong when the models are there and nobody has looked at
		// them yet: nothing can be picked because nothing is classified, and the fix is the
		// refresh, not an install. The fix belongs here rather than at the call site: three
		// call sites each guessed one, and `server run` guessed `voiceime server install`,
		// which re-runs the command that had just failed.
		if pending := models.Uninspected(scanned); len(pending) > 0 {
			return server.Spec{}, PreconditionError("voiceime models list --refresh",
				"%d model file(s) have not been inspected yet", len(pending))
		}
		recommendation := models.RecommendedPackages()[0]
		return server.Spec{}, PreconditionError("voiceime models install "+recommendation.ID,
			"no model to serve: install one first   # %s", recommendation.Reason)
	case streamPath == "":
		// Nothing that can stream is installed, but a repair checkpoint is named: serve it
		// for both roles rather than refusing to start.
		streamPath = repairPath
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
// It starts *voiceime*, not audio.cpp directly, and that is the point: `server run`
// renders the server config from voiceime's settings and then runs the server. There is
// one source of truth for which models are served, so `systemctl --user restart` picks
// up a changed setting and a stale config is impossible - the alternative was a second
// file that had to be rewritten by hand (by `server install`) and could silently disagree.
func serverUnit(serverBinary, configPath string) string {
	self, err := os.Executable()
	if err != nil {
		self = "voiceime"
	}
	// The unit names the configuration file it was installed for. Without it the
	// service reads the default path, so `server install --config <path>` (or any
	// caller with its own file) would install a service that ignores the settings it
	// was just told to serve.
	return fmt.Sprintf(`[Unit]
Description=voiceime recognition server (audio.cpp ASR, models resident)
Documentation=file://%s

[Service]
Type=simple
ExecStart=%s server run --config %s
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
`, filepath.Join(filepath.Dir(serverBinary), "..", "docs", "AGENT.md"), self, shellQuote(configPath))
}

// runServerForeground is what the unit's ExecStart runs: write the config that matches
// the settings, then become the server.
//
// Rendering here rather than only in `install` is what removes the second source of
// truth. Changing a model is a setting, and a restart is all it takes for the service to
// serve it - there is no generated file that can disagree with the configuration.
func runServerForeground(ctx *Context, port int, streamFlag, repairFlag string) error {
	cfg := ctx.Config
	scanned, err := cachedModels(ctx)
	if err != nil {
		return err
	}
	spec, err := buildServerSpec(cfg, scanned, port, streamFlag, repairFlag)
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
		ctx.Print("serving %s for partials and %s for repair (config %s)",
			filepath.Base(spec.StreamModel), filepath.Base(spec.RepairModel), path)
	}

	cmd := exec.Command(binary, "--config", path, "--log")
	cmd.Stdout = ctx.Stdout
	cmd.Stderr = ctx.Stderr
	if err := cmd.Start(); err != nil {
		return FailureError("start %s: %v", binary, err)
	}
	// Forward a stop rather than dying and orphaning the child: systemd kills the whole
	// cgroup, but an operator running this by hand with Ctrl-C would otherwise leave a
	// server behind.
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

// runServerRestart applies a changed setting: restart the unit and wait until it serves
// what the configuration now names.
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
	spec, err := buildServerSpec(ctx.Config, scanned, port, "", "")
	if err != nil {
		return PreconditionError("voiceime server install", "%v", err)
	}
	probeCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	if err := waitForEntries(probeCtx, ctx.Config.ASRServerURL, spec); err != nil {
		return FailureError("%v", err)
	}
	if !ctx.JSON {
		ctx.Print("restarted; serving %s for partials and %s for repair",
			filepath.Base(spec.StreamModel), filepath.Base(spec.RepairModel))
	}
	return ctx.Emit("server", map[string]any{
		"restarted":    true,
		"stream_model": spec.StreamModel,
		"repair_model": spec.RepairModel,
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
		"stream_entry": server.DefaultStreamID,
		"repair_entry": server.DefaultID,
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
		// The files, not only the ids: "which model is streaming" is the question this
		// command exists to answer, and the ids are the same whatever they point at.
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

// modelFamily reports a model's audio.cpp family, which the server requires. It comes
// from the CLI's own inspection, cached against the file's size and mtime, because
// guessing it from a file name is the kind of guess this project does not make.
func modelFamily(scanned []models.Model, path string) (string, error) {
	found := models.Lookup(scanned, path)
	switch {
	case found == nil:
		return "", PreconditionError("voiceime models list",
			"%s is not a model voiceime can see; check models_dir", path)
	case !found.Inspected:
		// The cache has no answer and nothing but `models list --refresh` may ask: this is
		// a service that will not start, so the message has to carry the one command that
		// fixes it rather than spending six seconds here and hiding the missing step.
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
// command that creates it. The failure is the same at every call site, and so is the fix.
func cachedModels(ctx *Context) ([]models.Model, error) {
	cached, err := models.Cached(ctx.Config)
	if err != nil {
		return nil, PreconditionError("voiceime models install "+models.RecommendedPackages()[0].ID, "%v", err)
	}
	return cached, nil
}
