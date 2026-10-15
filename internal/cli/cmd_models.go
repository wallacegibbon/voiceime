package cli

import (
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/models"
)

// cmdModels is the whole model-management surface the user asked for as a
// "refresh button in the UI": listing, refreshing, sizing and installing are
// subcommands, and the refresh cost is bounded by the inspection cache.
func cmdModels() *Command {
	return &Command{
		Name:    "models",
		Summary: "List, refresh, size and install model packages",
		Usage:   "voiceime models <list|refresh|installed|sizes|install|uninstall|search|recommended> [args]",
		Run:     runModels,
	}
}

func runModels(ctx *Context, args []string) error {
	if len(args) == 0 {
		return UsageError("models needs an action: list, refresh, installed, sizes, install, uninstall, search or recommended")
	}
	action, rest := args[0], args[1:]

	switch action {
	case "list", "refresh":
		force := action == "refresh"
		var progress func(string)
		if action == "refresh" && !ctx.JSON {
			progress = func(path string) { ctx.Print("inspecting %s", path) }
		}
		everything, err := models.Scan(ctx.Config, force, progress)
		if err != nil {
			return PreconditionError("voiceime config set audio_cpp_dir /path/to/audio.cpp", "%v", err)
		}
		scanned := models.ASRModels(everything)
		ignored := len(everything) - len(scanned)
		showAll := false
		for _, arg := range rest {
			if arg == "--all" {
				showAll = true
			}
		}
		if showAll {
			scanned = everything
			ignored = 0
		}
		sort.Slice(scanned, func(i, j int) bool { return scanned[i].Family < scanned[j].Family })

		streaming := models.PickStreaming(scanned, ctx.Config.Language)

		for _, model := range scanned {
			state := model.Family
			if model.Error != "" {
				state = "unusable: " + model.Error
			}
			repairHint := "slow-repair"
			if models.RepairIsRealtime(model) {
				repairHint = "realtime-repair"
			}
			ctx.Print("%-15s %-9s %-10s %-28s %s", state, models.Sizes(model.SizeBytes),
				repairHint, describeTasks(model), model.Path)
		}
		if len(scanned) == 0 {
			ctx.Print("no speech models found in %s", ctx.Config.ResolvedModelsDir())
			ctx.Print("install one with: voiceime models install %s", models.RecommendedPackages()[0].ID)
		}
		if ignored > 0 {
			ctx.Print("(%d other GGUF file(s) in %s are not audio models; `--all` lists them)",
				ignored, ctx.Config.ResolvedModelsDir())
		}

		refine := resolveModel(ctx, true)
		payload := map[string]any{
			"models":        scanned,
			"streaming":     pathOf(streaming),
			"refine":        refine,
			"refine_source": refineSource(ctx),
		}
		if err := ctx.Emit("models "+action, payload); err != nil {
			return err
		}
		if len(scanned) == 0 {
			return PreconditionError("voiceime models install "+models.RecommendedPackages()[0].ID,
				"no speech models are installed in %s", ctx.Config.ResolvedModelsDir())
		}
		return nil

	case "installed":
		manager, err := newModelManager(ctx)
		if err != nil {
			return err
		}
		probeCtx, cancel := timeout(60)
		defer cancel()
		statuses, err := manager.Installed(probeCtx)
		if err != nil {
			return FailureError("%v", err)
		}
		rows := []map[string]any{}
		for _, status := range statuses {
			if !status.Installed {
				continue
			}
			rows = append(rows, map[string]any{
				"id":       status.ID,
				"state":    status.VersionState,
				"endpoint": manager.Endpoint,
			})
			ctx.Print("%s", status.ID)
		}
		if err := ctx.Emit("models installed", map[string]any{"packages": rows}); err != nil {
			return err
		}
		return nil

	case "recommended":
		var rows []map[string]string
		for _, entry := range models.RecommendedPackages() {
			rows = append(rows, map[string]string{"id": entry.ID, "reason": entry.Reason})
			ctx.Print("%-32s %s", entry.ID, entry.Reason)
		}
		return ctx.Emit("models recommended", map[string]any{"packages": rows})

	case "search":
		needle := ""
		if len(rest) > 0 {
			needle = rest[0]
		}
		manager, err := newModelManager(ctx)
		if err != nil {
			return err
		}
		probeCtx, cancel := timeout(120)
		defer cancel()
		matches, err := manager.Search(probeCtx, needle)
		if err != nil {
			return FailureError("%v", err)
		}
		for _, pkg := range matches {
			ctx.Print("%-40s %-20s %-6s %s", pkg.ID, pkg.Family, pkg.Format, pkg.Precision)
		}
		return ctx.Emit("models search", map[string]any{"packages": matches, "count": len(matches)})

	case "sizes":
		if len(rest) == 0 {
			return UsageError("models sizes needs at least one package id")
		}
		manager, err := newModelManager(ctx)
		if err != nil {
			return err
		}
		probeCtx, cancel := timeout(300)
		defer cancel()
		statuses, err := manager.Sizes(probeCtx, rest...)
		if err != nil {
			return FailureError("%v", err)
		}
		for _, status := range statuses {
			detail := status.Message
			if status.SizeBytes != nil {
				detail = models.Sizes(*status.SizeBytes)
			}
			ctx.Print("%-40s %-8s %s", status.ID, status.State, detail)
		}
		return ctx.Emit("models sizes", map[string]any{"statuses": statuses, "endpoint": manager.Endpoint})

	case "install":
		if len(rest) == 0 {
			return UsageError("models install needs a package id; try `voiceime models recommended`")
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
		// A freshly installed model must become visible without the user knowing
		// that the inspection cache exists.
		if _, err := models.Scan(ctx.Config, false, nil); err != nil {
			ctx.Print("installed, but the model index could not be refreshed: %v", err)
		}
		return ctx.Emit("models install", map[string]any{
			"package":    packageID,
			"endpoint":   manager.Endpoint,
			"models_dir": manager.ModelsDir,
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

func pathOf(model *models.Model) string {
	if model == nil {
		return ""
	}
	return model.Path
}

// cmdTranscribe is the offline one-shot: useful for checking a model, and the
// exact code path the repair pass uses.
func cmdTranscribe() *Command {
	var model string
	var threads int
	var language string
	return &Command{
		Name:    "transcribe",
		Summary: "Transcribe a WAV file offline and print the text",
		Usage:   "voiceime transcribe <file.wav> [--model PATH] [--threads N] [--language CODE]",
		Flags: func(set *flag.FlagSet) {
			set.StringVar(&model, "model", "", "model path (default: the configured repair or streaming model)")
			set.IntVar(&threads, "threads", 0, "worker threads (default: from config)")
			set.StringVar(&language, "language", "", "language code (default: from config)")
		},
		Run: func(ctx *Context, args []string) error {
			if len(args) != 1 {
				return UsageError("transcribe needs exactly one WAV path")
			}
			target := model
			if target == "" {
				target = resolveModel(ctx, false)
			}
			if target == "" {
				return PreconditionError("voiceime models list", "no model available to transcribe with")
			}
			threadsValue := threads
			if threadsValue == 0 {
				threadsValue = ctx.Config.Threads
			}
			languageValue := language
			if languageValue == "" {
				languageValue = ctx.Config.Language
			}
			binary, err := ctx.Config.FindASRBinary()
			if err != nil {
				return PreconditionError("voiceime config set audio_cpp_dir /path/to/audio.cpp", "%v", err)
			}
			// Repairer is reused rather than duplicated: the offline decode is
			// the same operation whether the audio is a file the user named or a
			// segment voiceime captured a moment ago.
			from := config.Expand(args[0])
			wav, err := readPCM(from)
			if err != nil {
				return FailureError("%v", err)
			}
			repairer := newRepairer(ctx, target, binary, threadsValue, languageValue)
			transcribeCtx, cancelTranscribe := timeout(600)
			defer cancelTranscribe()
			text, err := repairer.Transcribe(transcribeCtx, wav)
			if err != nil {
				return FailureError("%v", err)
			}
			ctx.Print("%s", text)
			return ctx.Emit("transcribe", map[string]any{
				"file":     from,
				"model":    target,
				"language": languageValue,
				"text":     text,
			})
		},
	}
}
