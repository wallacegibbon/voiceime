package cli

import (
	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/model"
)

// cmdModel answers one question about the only checkpoint voiceime uses: what is it. The
// settings hold a path, and a path does not say whether it transcribes, how big it is or
// which audio.cpp loader will read it - this reads the file's own metadata and says so.
func cmdModel() *Command {
	return &Command{
		Name:    "model",
		Summary: "Show the checkpoint that is configured",
		Usage:   "voiceime model",
		Run: func(ctx *Context, args []string) error {
			if len(args) > 0 {
				return UsageError("model takes no arguments: the checkpoint is the one `model` names (see `voiceime config show`)")
			}
			path := ctx.Config.Model
			if path == "" {
				return PreconditionError(noModelFix, "%s", noModelAdvice())
			}
			expanded := config.Expand(path)
			info, err := model.Open(expanded)
			if err != nil {
				return PreconditionError("voiceime config set model /path/to/model.gguf", "%v", err)
			}
			if ctx.JSON {
				return ctx.Emit("model", map[string]any{
					"path":       info.Path,
					"family":     info.Family,
					"size_bytes": info.SizeBytes,
					"size":       model.Sizes(info.SizeBytes),
					"tasks":      info.Tasks,
					"says_tasks": info.Says(),
				})
			}
			ctx.Print("%s", describeCheckpoint(info))
			if !info.Says() {
				ctx.Print("(the file does not say which tasks it does; audio.cpp will refuse it if it cannot transcribe)")
			}
			return nil
		},
	}
}
