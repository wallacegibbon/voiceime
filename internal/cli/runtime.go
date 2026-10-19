package cli

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/models"
)

// absoluteConfigPath is the configuration file as a systemd unit has to name it.
//
// `--config` may be relative, and a unit has no working directory of its own: the
// service would read a different file from the one the install command was given.
func absoluteConfigPath(ctx *Context) string {
	if absolute, err := filepath.Abs(ctx.ConfigPath); err == nil {
		return absolute
	}
	return ctx.ConfigPath
}

// timeout bounds a subprocess-backed action. Named once so the durations are
// visible in a single place when tuning for a slow mirror or a large model.
func timeout(seconds int) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), time.Duration(seconds)*time.Second)
}

// resolveModel returns the model path to use. An explicit configuration always
// wins; otherwise discovery picks one, which is what keeps a fresh install
// working with no setup beyond installing a model package.
func resolveModel(ctx *Context, repair bool) string {
	// An empty refine_model means "reuse the streaming model": one model instead of
	// two resident ones (D22), and a repair that needs no extra configuration. The
	// 5.7s that used to justify this default was a process-per-utterance cost that the
	// resident service removed, so the choice is now memory against accuracy rather
	// than latency - and the user makes it. See docs/AGENT.md's model table.
	if repair {
		if ctx.Config.RefineModel != "" {
			return config.Expand(ctx.Config.RefineModel)
		}
		return resolveModel(ctx, false)
	}
	if ctx.Config.StreamingModel != "" {
		return config.Expand(ctx.Config.StreamingModel)
	}
	scanned, err := models.Scan(ctx.Config, false, nil)
	if err != nil {
		return ""
	}
	if picked := models.PickStreaming(scanned, ctx.Config.Language); picked != nil {
		return picked.Path
	}
	return ""
}

// newModelManager builds the download manager, turning each failure into the
// command that fixes it: a bad endpoint is a configuration mistake, everything
// else NewManager rejects is a missing prerequisite.
func newModelManager(ctx *Context) (*models.Manager, error) {
	manager, err := models.NewManager(ctx.Config)
	if err != nil {
		if errors.Is(err, models.ErrBadEndpoint) {
			return nil, PreconditionError(
				"voiceime config set download_mirror "+config.DefaultDownloadEndpoint, "%v", err)
		}
		return nil, PreconditionError("voiceime config set audio_cpp_dir /path/to/audio.cpp", "%v", err)
	}
	return manager, nil
}
