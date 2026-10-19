package cli

import (
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/wallacegibbon/voiceime/internal/models"
)

// cmdModels is the whole model-management surface the user asked for as a
// "refresh button in the UI": listing, refreshing, sizing and installing are
// subcommands, and the refresh cost is bounded by the inspection cache.
func cmdModels() *Command {
	// --all is a declared flag for the same reason config's --force is: reading it
	// out of the positional arguments never worked, so `models list --all` - the
	// documented way to see the non-ASR GGUF files in a shared models directory -
	// failed with "flag provided but not defined".
	var all bool
	var refresh bool
	var force bool
	return &Command{
		Name:    "models",
		Summary: "List installed models and install or remove packages",
		Usage:   "voiceime models <list|install|uninstall> [args]",
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
		return UsageError("models needs an action: list, install or uninstall")
	}
	action, rest := args[0], args[1:]

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
				return PreconditionError("voiceime models list --refresh", "%v", err)
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

		// Which checkpoint each role uses: one answer, shared with the server spec and
		// `config show` (D32).
		resolved := models.Resolve(scanned, ctx.Config.Language, ctx.Config.StreamingModel, ctx.Config.RefineModel)

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
			ctx.Print("no speech models found in %s", ctx.Config.ResolvedModelsDir())
			ctx.Print("install one with: voiceime models install %s", models.RecommendedPackages()[0].ID)
		}
		if ignored > 0 {
			ctx.Print("(%d other GGUF file(s) in %s are not audio models; `--all` lists them)",
				ignored, ctx.Config.ResolvedModelsDir())
		}

		payload := map[string]any{
			"models":        scanned,
			"streaming":     resolved.Stream,
			"refine":        resolved.Repair,
			"refine_source": refineSource(ctx),
			"uninspected":   len(pending),
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
			return PreconditionError("voiceime models install "+models.RecommendedPackages()[0].ID,
				"no speech models are installed in %s", ctx.Config.ResolvedModelsDir())
		}

	case "install":
		if len(rest) == 0 {
			return UsageError("models install needs a package id, e.g. sensevoice_small_q8 (see docs/AGENT.md)")
		}
		packageID := rest[0]
		manager, err := newModelManager(ctx)
		if err != nil {
			return err
		}
		ctx.Print("installing %s from %s into %s", packageID, manager.Endpoint, manager.ModelsDir)
		lastPercent := -1
		installCtx, cancelInstall := timeout(3600)
		defer cancelInstall()
		err = manager.Install(installCtx, packageID, func(progress models.Progress) {
			if ctx.JSON {
				return
			}
			// Progress records carry byte counts; narration is printed as it
			// arrives. A terminal gets a one-line percentage, a pipe gets lines.
			if progress.TotalBytes > 0 {
				percent := int(float64(progress.Bytes) / float64(progress.TotalBytes) * 100)
				if percent != lastPercent {
					lastPercent = percent
					ctx.Print("  %s: %d%%", progress.File, percent)
				}
				return
			}
			if progress.Line != "" {
				ctx.Print("  %s", progress.Line)
			}
		})
		if err != nil {
			return FailureError("%v", err)
		}
		// The download did not register the model, and install does not write the cache:
		// `models list --refresh` is its only writer (D35). Naming the command here is what
		// keeps the required step from being a surprise two commands later, when `doctor`
		// reports the model as uninspected.
		next := "voiceime models list --refresh"
		ctx.Print("downloaded into %s; run `%s` so voiceime can identify it", manager.ModelsDir, next)
		return ctx.Emit("models install", map[string]any{
			"package":    packageID,
			"endpoint":   manager.Endpoint,
			"models_dir": manager.ModelsDir,
			"next":       next,
		})

	case "uninstall":
		if len(rest) == 0 {
			return UsageError("models uninstall needs a package id")
		}
		manager, err := newModelManager(ctx)
		if err != nil {
			return err
		}
		removeCtx, cancelRemove := timeout(120)
		defer cancelRemove()
		if err := manager.Uninstall(removeCtx, rest[0]); err != nil {
			return FailureError("%v", err)
		}
		return ctx.Emit("models uninstall", map[string]any{"package": rest[0]})
	}

	return UsageError("unknown models action %q", action)
}

// refineSource says where the repair model came from, so a caller can tell a
// deliberate choice from the default.
func refineSource(ctx *Context) string {
	if ctx.Config.RefineModel != "" {
		return "configured"
	}
	if ctx.Config.RefineOnSegmentEnd {
		return "same-as-streaming"
	}
	return "disabled"
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
