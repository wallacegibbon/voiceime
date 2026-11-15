package cli

import (
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/wallacegibbon/voiceime/internal/models"
)

func cmdModels() *Command {
	var all bool
	var refresh bool
	var force bool
	return &Command{
		Name:    "models",
		Summary: "List the speech models on this machine",
		Usage:   "voiceime models list [--refresh] [--force] [--all]",
		Flags: func(set *flag.FlagSet) {
			set.BoolVar(&refresh, "refresh", false,
				"list: inspect files the cache cannot answer for, and record the answer (the only command that writes the cache)")
			set.BoolVar(&force, "force", false,
				"list: with --refresh, re-inspect files that look unchanged (after upgrading audio.cpp)")
			set.BoolVar(&all, "all", false, "list: include files that are not speech models")
		},
		Run: func(ctx *Context, args []string) error {
			return runModels(ctx, args, all, refresh, force)
		},
	}
}

func runModels(ctx *Context, args []string, showAllModels, refreshModels, forceModels bool) error {
	if len(args) == 0 {
		return UsageError("models needs an action: list")
	}
	action := args[0]

	switch action {
	case "list":
		if forceModels && !refreshModels {
			return UsageError("--force re-inspects files that look unchanged, so it needs --refresh")
		}
		var progress func(string)
		if refreshModels && !ctx.JSON {
			progress = func(path string) { ctx.Print("inspecting %s", path) }
		}
		var everything []models.Model
		var err error
		if refreshModels {
			// The cache's only writer (D35). Everything else reads it.
			everything, err = models.Refresh(ctx.Config, forceModels, progress)
		} else {
			everything, err = models.Cached(ctx.Config)
		}
		if err != nil {
			if !refreshModels {
				// The only reason this reads is a models directory that is not there.
				return PreconditionError(modelsDirFix, "%v", err)
			}
			return PreconditionError("voiceime config set audio_cpp_dir /path/to/audio.cpp", "%v", err)
		}
		pending := models.Uninspected(everything)
		scanned := models.ASRModels(everything)
		ignored := len(everything) - len(scanned) - len(pending)
		showAll := showAllModels
		if showAll {
			scanned = everything
			ignored = 0
		}
		sort.Slice(scanned, func(i, j int) bool { return scanned[i].Family < scanned[j].Family })

		resolved := models.Resolve(scanned, ctx.Config.Language, ctx.Config.Model)

		for _, model := range scanned {
			state := model.Family
			switch {
			case !model.Inspected:
				state = "not inspected"
			case model.Error != "":
				state = "unusable: " + model.Error
			}
			ctx.Print("%-15s %-9s %-28s %s", state, models.Sizes(model.SizeBytes),
				describeTasks(model), model.Path)
		}
		if len(pending) > 0 {
			// Said rather than acted on: inspecting one checkpoint costs about six seconds,
			// and only `--refresh` is allowed to do it (D35).
			ctx.Print("%d file(s) have not been inspected yet; run `voiceime models list --refresh` to identify them",
				len(pending))
		}
		if len(scanned) == 0 && len(pending) == 0 {
			ctx.Print("%s", noModelAdvice(ctx.Config.ResolvedModelsDir()))
		}
		if ignored > 0 {
			ctx.Print("(%d other GGUF file(s) in %s are not audio models; `--all` lists them)",
				ignored, ctx.Config.ResolvedModelsDir())
		}

		payload := map[string]any{
			"models":      scanned,
			"model":       resolved.Model,
			"uninspected": len(pending),
		}
		if err := ctx.Emit("models "+action, payload); err != nil {
			return err
		}
		switch {
		case len(scanned) > 0:
			return nil
		case len(pending) > 0:
			return PreconditionError("voiceime models list --refresh",
				"%d model file(s) in %s have not been inspected yet, so none can be classified",
				len(pending), ctx.Config.ResolvedModelsDir())
		default:
			return PreconditionError(noModelFix, "%s", noModelAdvice(ctx.Config.ResolvedModelsDir()))
		}

	}

	// Naming the valid action is what makes a wrong guess one retry instead of two.
	return UsageError("models has one action: list (unknown action %q)", action)
}

func describeTasks(model models.Model) string {
	var parts []string
	for _, task := range model.Tasks {
		parts = append(parts, fmt.Sprintf("%s(%s)", task.Name, strings.Join(task.Modes, "/")))
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ",")
}
