package cli

import (
	"path/filepath"
)

// absoluteConfigPath is the configuration file as a systemd unit has to name it.
func absoluteConfigPath(ctx *Context) string {
	if absolute, err := filepath.Abs(ctx.ConfigPath); err == nil {
		return absolute
	}
	return ctx.ConfigPath
}
