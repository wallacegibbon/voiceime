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
