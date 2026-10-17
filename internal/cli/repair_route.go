package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/wallacegibbon/voiceime/internal/asr"
	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/models"
	"github.com/wallacegibbon/voiceime/internal/server"
)

// repairRoute decides how a repair decodes, and returns a route the session can
// bring up.
//
// Nil means "nothing preferred": the session builds the one-process-per-utterance
// repairer it has always built, which needs no daemon and is fast enough for a
// small model. A configured server is preferred instead, because a process pays
// the whole model load every utterance (~4.4s of CPU for a 1.15 GiB checkpoint on
// the reference machine, whatever the audio) while a server pays it once.
//
// Nothing is started here. The session starts it as dictation begins and stops it
// at the end, so the process it may spawn has the session's lifetime rather than
// the machine's; see asr.Routed and internal/server.
// repairModel is the checkpoint the CLI repair route will use for this session. It
// is passed in rather than read from the config because the front ends can override
// it per run (the engine resolves one, `stream --refine-model` names one), and the
// two routes have to serve the same model to be the same decode.
func repairRoute(cfg *config.Config, repairModel string) func(ctx context.Context) (asr.Transcriber, error) {
	raw := strings.TrimSpace(cfg.ASRServerURL)
	if raw == "" {
		return nil
	}
	model := strings.TrimSpace(repairModel)
	if model == "" {
		model = strings.TrimSpace(cfg.RefineModel)
	}
	modelID := strings.TrimSpace(cfg.ASRServerModel)
	if modelID == "" {
		modelID = server.DefaultID
	}
	return func(ctx context.Context) (asr.Transcriber, error) {
		return startServerRoute(ctx, cfg, raw, model, modelID)
	}
}

// startServerRoute brings the preferred route up: the server already listening on
// the configured address, or one this machine starts on demand.
func startServerRoute(ctx context.Context, cfg *config.Config, raw, model, modelID string) (asr.Transcriber, error) {
	transcriber := asr.ServerTranscriber{
		BaseURL:  raw,
		Model:    modelID,
		Language: cfg.Language,
		Timeout:  time.Duration(cfg.ASRServerTimeoutMS) * time.Millisecond,
	}

	host, port, err := server.ParseURL(raw)
	if err != nil {
		return nil, err
	}
	instance, err := server.Ensure(ctx, server.Spec{
		Binary:  func() (string, error) { return cfg.FindServerBinary() },
		WorkDir: cfg.AudioCPPDirPath(),
		Model:   model,
		Family:  func() (string, error) { return refineFamily(cfg, model) },
		ModelID: modelID,
		Host:    host,
		Port:    port,
		Backend: cfg.Backend,
		Threads: cfg.Threads,
		// The same options the CLI repair route passes, because the two routes
		// must decode identically to be comparable.
		RequestOptions: asr.RepairRequestOptions(cfg.RequestOptions),
		ConfigPath:     serverConfigPath(port),
		LogPath:        serverLogPath(port),
		Autostart:      cfg.ASRServerAutostart,
		StartTimeout:   30 * time.Second,
		// A server that outlives its session - the parent was killed outright, so
		// Stop never ran - releases its weights rather than holding them until the
		// machine is rebooted. It does not end the process.
		IdleUnloadMS: 120000,
	})
	if err != nil {
		return nil, err
	}

	// The model id is the mistake a user is most likely to have made, and the one
	// that would otherwise surface per utterance as a repair error. Checking it
	// costs one request per session.
	if err := transcriber.Check(ctx); err != nil {
		instance.Stop()
		return nil, err
	}
	// Best effort, and deliberately not fatal: a server that refuses this probe
	// audio is still worth using, and its first real repair is the better signal.
	// The point of warming is that the model is loaded before the user's first
	// sentence ends, which is why the route is started at the top of a session.
	_ = instance.Warm(ctx, 2*time.Minute)

	return &serverRoute{transcriber: transcriber, instance: instance}, nil
}

// serverRoute is the preferred repair route once a server is serving: decodes go
// over HTTP, and Stop ends the process if this machine started it.
type serverRoute struct {
	transcriber asr.ServerTranscriber
	instance    *server.Instance
}

func (r *serverRoute) Transcribe(ctx context.Context, pcm []byte) (string, error) {
	return r.transcriber.Transcribe(ctx, pcm)
}

// Stop ends a server this run started. One the user started is the user's, and
// Instance.Stop knows the difference.
func (r *serverRoute) Stop() { r.instance.Stop() }

// refineFamily reports the refine model's audio.cpp family, which the server
// requires in its config. It comes from the CLI's own inspection, cached against
// the file's size and mtime, because guessing it from a file name is the kind of
// guess this project does not make.
func refineFamily(cfg *config.Config, path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("asr_server_autostart needs a refine model for the server to serve")
	}
	scanned, err := models.Scan(cfg, false, nil)
	if err != nil {
		return "", err
	}
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

func serverConfigPath(port int) string {
	return filepath.Join(config.CacheDir(), fmt.Sprintf("asr-server-%d.json", port))
}

func serverLogPath(port int) string {
	return filepath.Join(config.CacheDir(), fmt.Sprintf("asr-server-%d.log", port))
}

// repairRouteDescription names the route in use, for a startup log line and for
// `stream --events`.
func repairRouteDescription(cfg *config.Config, route func(ctx context.Context) (asr.Transcriber, error)) string {
	if route == nil {
		return "repair: one audiocpp_cli process per utterance"
	}
	mode := "using the server already running"
	if cfg.ASRServerAutostart {
		mode = "started on demand if nothing is listening"
	}
	model := strings.TrimSpace(cfg.ASRServerModel)
	if model == "" {
		model = server.DefaultID
	}
	return fmt.Sprintf("repair: server at %s (model %s, %s), falling back to a process per utterance",
		strings.TrimRight(cfg.ASRServerURL, "/"), model, mode)
}
