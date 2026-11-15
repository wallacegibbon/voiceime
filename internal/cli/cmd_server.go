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
	"github.com/wallacegibbon/voiceime/internal/model"
	"github.com/wallacegibbon/voiceime/internal/server"
)

// serverUnitName is the systemd user unit that keeps the recognition server running.
const serverUnitName = "voiceime-asr-server.service"

// serverConfigName is the audio.cpp config voiceime generates from the settings. It is an
// output, not an input: see serverConfigFile.
const serverConfigName = "asr-server.json"

// cmdServer is the recognition service's own process. `voiceime apply` starts it through a
// systemd user unit whose ExecStart is `voiceime server run`; this is that entry, and the
// way to run the service where there is no systemd user session.
//
// It has no install/restart/status actions: those were a copy of what `voiceime apply` and
// `voiceime doctor` already decide, with their own idea of what "installed" means.
// serverActions: the service is installed, restarted and inspected by `voiceime apply` and
// `voiceime doctor`. What is left is the process the unit runs.
var serverActions = []string{"run"}

func cmdServer() *Command {
	return &Command{
		Name:    "server",
		Summary: "Run the resident recognition server in the foreground (the service's ExecStart)",
		Actions: serverActions,
		Usage:   usageLine("server", serverActions, ""),
		Flags:   func(set *flag.FlagSet) {},
		Run: func(ctx *Context, args []string) error {
			if len(args) == 0 {
				return UsageError("server needs an action: %s", strings.Join(serverActions, ", "))
			}
			if args[0] != "run" {
				return UsageError("unknown server action %q; the service is installed and restarted by `voiceime apply`", args[0])
			}
			if len(args) > 1 {
				return UsageError("server run takes no arguments")
			}
			return runServerForeground(ctx)
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

// serverConfigFile is the generated audio.cpp config - an output, never an input. It is
// rewritten from the settings at every service start, and it lives in the runtime
// directory rather than the configuration directory so that the only file in
// `ConfigDir()` is the one a person or an agent edits. The unit does not name this path
// (it runs `server run`, which regenerates it), so the service cannot be pointed at a
// stale one; `voiceime doctor` reports what is served, which is how a caller confirms which
// model is actually running.
func serverConfigFile() string {
	return filepath.Join(config.RuntimeDir(), serverConfigName)
}

// writeServerConfig renders the audio.cpp config and returns where it wrote it, so the
// two commands that start a service resolve that path the same way.
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

// runServerForeground is what the unit's ExecStart runs: write the config that matches the
// settings, then become the server.
//
// The port comes from asr_server_url, because that is the address the engine dials: a second
// place to set it would be a second way for the two to disagree.
func runServerForeground(ctx *Context) error {
	cfg := ctx.Config
	port := server.DefaultPort
	if baseURL := strings.TrimSpace(cfg.ASRServerURL); baseURL != "" {
		_, parsed, err := server.ParseURL(baseURL)
		if err != nil {
			return PreconditionError("voiceime config set asr_server_url http://127.0.0.1:18080", "%v", err)
		}
		port = parsed
	}
	spec, err := buildServerSpec(cfg, port)
	if err != nil {
		return err
	}
	path, err := writeServerConfig(spec)
	if err != nil {
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

// buildServerSpec decides which checkpoint the service serves. Shared with doctor, so the
// check can say what should be running.
func buildServerSpec(cfg *config.Config, port int) (server.Spec, error) {
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
		Model:          path,
		Family:         info.Family,
		Port:           port,
		Backend:        cfg.Backend,
		Threads:        cfg.Threads,
		RequestOptions: asr.RequestOptions(cfg.RequestOptions),
	}, nil
}

// shellQuote quotes a value for a shell-style word splitter, which is what systemd and
// gsettings both use.
func shellQuote(value string) string {
	if !strings.ContainsAny(value, " \t\"'") {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}
