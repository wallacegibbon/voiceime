package cli

import (
	"context"
	"errors"
	"time"

	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/models"
)

// timeout bounds a subprocess-backed action. Named once so the durations are
// visible in a single place when tuning for a slow mirror or a large model.
func timeout(seconds int) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), time.Duration(seconds)*time.Second)
}

// resolveModel returns the model path to use. An explicit configuration always
// wins; otherwise discovery picks one, which is what keeps a fresh install
// working with no setup beyond installing a model package.
func resolveModel(ctx *Context, repair bool) string {
	// An empty refine_model means "reuse the streaming model", which is the
	// default for a measured reason: repairing with the streaming model costs a
	// fraction of the utterance (0.55s for a 4.2s segment on the reference
	// machine), while the strongest accurate model installed there costs 5.7s. A
	// repair that lags behind the speaker is worse than the boundary damage it
	// removes, so accuracy only wins when the user says so.
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
