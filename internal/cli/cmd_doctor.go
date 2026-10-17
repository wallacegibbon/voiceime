package cli

import (
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/models"
)

// Check is one doctor finding.
//
// Fix is the field that makes doctor usable without a human: it names the exact
// command that resolves the finding, so an agent can iterate
// doctor -> run Fix -> doctor until the report is clean.
type Check struct {
	ID     string `json:"id"`
	Status string `json:"status"` // ok, warn, fail
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

func cmdDoctor() *Command {
	var refresh bool
	return &Command{
		Name:    "doctor",
		Summary: "Check every prerequisite and report machine-readable findings",
		Usage:   "voiceime doctor [--refresh]",
		Flags: func(set *flag.FlagSet) {
			set.BoolVar(&refresh, "refresh", false, "re-inspect every model instead of using the cache")
		},
		Run: func(ctx *Context, args []string) error {
			checks := runDoctor(ctx, refresh)
			failures := 0
			for _, check := range checks {
				if check.Status == "fail" {
					failures++
				}
			}
			if err := ctx.Emit("doctor", map[string]any{
				"checks":   checks,
				"failures": failures,
			}); err != nil {
				return err
			}
			if failures > 0 {
				return PreconditionError("run the `fix` commands above, then `voiceime doctor` again",
					"%d prerequisite(s) missing", failures)
			}
			return nil
		},
	}
}

func runDoctor(ctx *Context, refresh bool) []Check {
	cfg := ctx.Config
	var checks []Check
	report := func(check Check) {
		checks = append(checks, check)
		ctx.Print("%-4s %-18s %s", check.Status, check.ID, check.Detail)
		if check.Fix != "" {
			ctx.Print("     fix: %s", check.Fix)
		}
	}

	// 1. Configuration.
	issues := config.Errors(cfg.Validate())
	if len(issues) == 0 {
		report(Check{ID: "config", Status: "ok", Detail: ctx.ConfigPath})
	} else {
		var messages []string
		for _, issue := range issues {
			messages = append(messages, issue.Key+": "+issue.Message)
		}
		fix := ""
		if issues[0].Fix != "" {
			fix = issues[0].Fix
		}
		report(Check{
			ID:     "config",
			Status: "fail",
			Detail: ctx.ConfigPath + ": " + strings.Join(messages, "; "),
			Fix:    fix,
		})
	}

	// 2. audio.cpp CLI, which does all the actual inference.
	asrBinary, asrErr := cfg.FindASRBinary()
	if asrErr != nil {
		report(Check{
			ID:     "audio_cpp",
			Status: "fail",
			Detail: "audiocpp_cli not found",
			Fix:    "voiceime config set audio_cpp_dir /path/to/audio.cpp  (or set asr_bin)",
		})
	} else {
		detail := asrBinary
		if version, err := probe(asrBinary, "--version"); err == nil {
			detail += " (" + firstLine(version) + ")"
		}
		report(Check{ID: "audio_cpp", Status: "ok", Detail: detail})
	}

	// 3. Downloads: the model manager script and the endpoint it fetches from.
	if manager, err := cfg.FindModelManager(); err != nil {
		report(Check{
			ID:     "model_manager",
			Status: "fail",
			Detail: "model_manager_v2.py not found; downloads are unavailable",
			Fix:    "voiceime config set audio_cpp_dir /path/to/audio.cpp",
		})
	} else {
		report(Check{ID: "model_manager", Status: "ok", Detail: manager})
	}
	// The endpoint is checked for syntax only. Whether a host is reachable or
	// holds a package is what `voiceime models sizes` answers with a real
	// request; doctor never touches the network, so it stays usable offline and
	// cannot hang on an unreachable mirror.
	if endpoint, err := cfg.DownloadEndpoint(); err != nil {
		report(Check{
			ID:     "download_mirror",
			Status: "fail",
			Detail: err.Error(),
			Fix:    "voiceime config set download_mirror " + config.DefaultDownloadEndpoint,
		})
	} else {
		report(Check{ID: "download_mirror", Status: "ok", Detail: endpoint})
	}

	// 4. The models directory itself.
	modelsDir := cfg.ResolvedModelsDir()
	if info, err := os.Stat(modelsDir); err != nil || !info.IsDir() {
		report(Check{
			ID:     "models_dir",
			Status: "fail",
			Detail: modelsDir + " does not exist",
			Fix:    fmt.Sprintf("mkdir -p %s   # or: voiceime config set models_dir <dir>", modelsDir),
		})
	} else {
		report(Check{ID: "models_dir", Status: "ok", Detail: modelsDir})
	}

	// 5. Model selection. Scanning needs the CLI, so it is skipped rather than
	// reported as failed when the CLI is absent - one failure, not two.
	if asrErr == nil {
		modelChecks(ctx, refresh, report)
	}

	// 6. Capture.
	if tool := config.FindExecutable("parec", "pw-record"); tool == "" {
		report(Check{
			ID:     "capture",
			Status: "fail",
			Detail: "neither parec nor pw-record is installed",
			Fix:    "sudo apt install pipewire-bin pulseaudio-utils",
		})
	} else {
		detail := tool
		if source := defaultSource(); source != "" {
			detail += " (default source: " + source + ")"
		} else {
			detail += " (no default source reported; check `voiceime devices`)"
		}
		report(Check{ID: "capture", Status: "ok", Detail: detail})
	}

	// 7. Text delivery through IBus.
	state := ibusState(ctx)
	status := "ok"
	detail := "IBus is running"
	fix := ""
	if installed, _ := state["installed"].(bool); !installed {
		status, detail = "fail", "the ibus CLI is not installed; text delivery is impossible"
		fix = "sudo apt install ibus"
	} else if running, _ := state["daemon_running"].(bool); !running {
		status, detail = "fail", "ibus-daemon is not reachable"
		fix = "ibus-daemon -drx"
	} else if registered, _ := state["engine_registered"].(bool); !registered {
		status = "fail"
		detail = "the voiceime engine is not registered with IBus"
		fix = "voiceime engine install && ibus restart"
	}
	report(Check{ID: "ibus", Status: status, Detail: detail, Fix: fix})

	// 8. Optional feedback tools. Missing ones degrade UX, never function.
	var optional []string
	for id, tool := range map[string]string{
		"notify": config.FindExecutable("notify-send"),
		"sound":  config.FindExecutable("pw-play", "paplay", "canberra-gtk-play"),
	} {
		if tool == "" {
			optional = append(optional, id)
		}
	}
	if len(optional) > 0 {
		sort.Strings(optional)
		report(Check{
			ID:     "feedback",
			Status: "warn",
			Detail: "missing " + strings.Join(optional, ", ") + "; status cues will be silent",
			Fix:    "sudo apt install libnotify-bin pipewire-bin",
		})
	} else {
		report(Check{ID: "feedback", Status: "ok", Detail: "notifications and sound cues available"})
	}

	// 9. The settings whose right value is a property of the machine.
	hardwareChecks(ctx, report)

	// 10. Installed artifacts: the paths baked into the component file and the
	// systemd unit, and whether the running processes came from this binary. A
	// moved checkout breaks all three silently (see install_check.go).
	installChecks(ctx, report)

	return checks
}

// modelChecks reports which model will be used and whether the choice exists.
// Findings are pushed through report so the human and JSON renderings cannot
// drift apart.
func modelChecks(ctx *Context, refresh bool, report func(Check)) {
	cfg := ctx.Config
	everything, err := models.Scan(cfg, refresh, nil)
	scanned := models.ASRModels(everything)
	if err != nil {
		report(Check{
			ID:     "models",
			Status: "fail",
			Detail: err.Error(),
			Fix:    "voiceime models refresh",
		})
		return
	}
	if len(scanned) == 0 {
		recommendation := models.RecommendedPackages()[0]
		report(Check{
			ID:     "streaming_model",
			Status: "fail",
			Detail: "no speech models found in " + cfg.ResolvedModelsDir(),
			Fix:    fmt.Sprintf("voiceime models install %s   # %s", recommendation.ID, recommendation.Reason),
		})
		return
	}

	if cfg.StreamingModel != "" {
		found := models.Lookup(scanned, cfg.StreamingModel)
		switch {
		case found == nil:
			report(Check{
				ID:     "streaming_model",
				Status: "fail",
				Detail: cfg.StreamingModel + " is configured but was not found under " + cfg.ResolvedModelsDir(),
				Fix:    "voiceime models list",
			})
		case found.Error != "":
			report(Check{
				ID:     "streaming_model",
				Status: "fail",
				Detail: found.Path + " cannot be loaded: " + found.Error,
				Fix:    "voiceime config set streaming_model <path from `voiceime models list`>",
			})
		case !found.StreamingASR():
			report(Check{
				ID:     "streaming_model",
				Status: "fail",
				Detail: fmt.Sprintf("%s (%s) has no streaming ASR mode; live dictation needs one", found.Path, found.Family),
				Fix:    "voiceime config set streaming_model <path from `voiceime models list`>",
			})
		default:
			report(Check{
				ID:     "streaming_model",
				Status: "ok",
				Detail: fmt.Sprintf("%s (%s)", found.Path, found.Family),
			})
		}
	} else {
		picked := models.PickStreaming(scanned, cfg.Language)
		if picked == nil {
			recommendation := models.RecommendedPackages()[0]
			report(Check{
				ID:     "streaming_model",
				Status: "fail",
				Detail: "no installed model supports streaming ASR",
				Fix:    fmt.Sprintf("voiceime models install %s   # %s", recommendation.ID, recommendation.Reason),
			})
		} else {
			report(Check{
				ID:     "streaming_model",
				Status: "ok",
				Detail: fmt.Sprintf("%s (%s) - picked automatically", picked.Path, picked.Family),
				Fix:    "voiceime config set streaming_model " + picked.Path,
			})
		}
	}

	// The repair model is optional and defaults to the streaming model, which is
	// the interactive choice: measured on the reference machine, re-decoding a
	// 4.2s segment costs ~0.55s with SenseVoice against ~5.7s with the strongest
	// accurate model installed there. Doctor states that trade rather than
	// deciding it.
	if cfg.RefineModel != "" {
		found := models.Lookup(scanned, cfg.RefineModel)
		if found == nil || found.Error != "" {
			report(Check{
				ID:     "refine_model",
				Status: "fail",
				Detail: cfg.RefineModel + " is configured but not usable",
				Fix:    "voiceime models list",
			})
		} else {
			detail := fmt.Sprintf("%s (%s)", found.Path, found.Family)
			if !models.RepairIsRealtime(*found) {
				// The cost this warns about is the one a fresh process pays per
				// utterance, which is the whole reason a resident server exists. A
				// machine configured for one is not being advised to change model.
				if cfg.ASRServerURL != "" {
					detail += "; slower than real time as a fresh process on CPU, which is what asr_server_url avoids: the server pays its load once"
				} else {
					detail += "; measured cost: slower than real time on CPU, so repairs will lag behind speech"
				}
			}
			report(Check{ID: "refine_model", Status: "ok", Detail: detail})
		}
	} else if !cfg.RefineOnSegmentEnd {
		report(Check{
			ID:     "refine_model",
			Status: "warn",
			Detail: "repair is disabled: window-boundary damage in the stream text will be delivered as-is",
			Fix:    "voiceime config set refine_on_segment_end true",
		})
	} else {
		streamingFamily := ""
		if picked := models.PickStreaming(scanned, cfg.Language); picked != nil {
			streamingFamily = picked.Family
		}
		report(Check{
			ID:     "refine_model",
			Status: "ok",
			Detail: "reuses the streaming model (fastest repair)",
		})
		if better := models.SuggestRepair(scanned, streamingFamily); better != nil {
			detail := fmt.Sprintf("%s (%s, %s) is installed and is the more accurate reader; on CPU one repair costs ~1.3x real time, mostly loading the weights, so repairs would lag behind speech. With a GPU backend it is the recommended repair model",
				better.Path, better.Family, models.Sizes(better.SizeBytes))
			switch {
			case cfg.ASRServerURL != "":
				detail = fmt.Sprintf("%s (%s, %s) is installed and is the more accurate reader; as a process per utterance on CPU it would lag behind speech, but asr_server_url is set, so that load is paid once per dictation instead of once per sentence",
					better.Path, better.Family, models.Sizes(better.SizeBytes))
			case !models.RepairIsRealtime(*better):
				// The fix below is a single setting, so the resolution that makes it
				// affordable has to be named here or following it leads straight into
				// the warning above.
				detail += "; `voiceime config set asr_server_url http://127.0.0.1:18080` moves the load off the per-utterance path and makes this worthwhile on CPU"
			}
			report(Check{
				ID:     "refine_model_advice",
				Status: "warn",
				Detail: detail,
				Fix:    "voiceime config set refine_model " + better.Path,
			})
		}
	}

	// The optional resident server: the one part of a working installation nothing
	// else verifies, because dictation keeps working when it cannot start.
	report(asrServerCheck(cfg, scanned))
}
