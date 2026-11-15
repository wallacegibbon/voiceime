package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/model"
)

// Check is one doctor finding.
type Check struct {
	ID     string `json:"id"`
	Status string `json:"status"` // ok, warn, fail
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"`
}

func cmdDoctor() *Command {
	return &Command{
		Name:    "doctor",
		Summary: "Check every prerequisite and report machine-readable findings",
		Usage:   "voiceime doctor",
		Run: func(ctx *Context, args []string) error {
			checks := runDoctor(ctx)
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

func runDoctor(ctx *Context) []Check {
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

	// 3. The checkpoint. It is named by a path and read from the file, so this needs no
	// CLI, no cache and no directory: it is reported whether or not audio.cpp is present.
	modelChecks(ctx, report)

	// 4. Capture.
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

	// 5. Text delivery through IBus.
	state := ibusState()
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
		fix = "voiceime engine install --apply"
	}
	report(Check{ID: "ibus", Status: status, Detail: detail, Fix: fix})

	// 6. Optional feedback tools. Missing ones degrade UX, never function.
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

	// 7. The settings whose right value is a property of the machine.
	hardwareChecks(ctx, report)

	// 8. Installed artifacts: the paths baked into the component file and the
	// systemd unit, and whether the resident processes run those paths. A moved
	// checkout breaks all three silently (see install_check.go).
	installChecks(report)
	serverUnitCheck(report)

	return checks
}

// modelChecks reports on the checkpoint that was named. There is no collection to scan and
// no cache that can be stale: one path, and what the file at the end of it says about itself.
func modelChecks(ctx *Context, report func(Check)) {
	cfg := ctx.Config
	path := strings.TrimSpace(cfg.Model)
	if path == "" {
		report(Check{ID: "model", Status: "fail", Detail: noModelAdvice(), Fix: noModelFix})
		report(asrServerCheck(cfg, serverUnitInstalled()))
		return
	}
	info, err := model.Open(config.Expand(path))
	switch {
	case err != nil:
		report(Check{
			ID:     "model",
			Status: "fail",
			Detail: err.Error(),
			Fix:    "voiceime config set model /path/to/model.gguf",
		})
	case info.Says() && !info.ASR():
		// Only when the file says what it does: some checkpoints embed a full spec and
		// some only a family, and a model that does not name its tasks is not a model
		// that refuses to transcribe.
		report(Check{
			ID:     "model",
			Status: "fail",
			Detail: fmt.Sprintf("%s says it does %s, not transcription", info.Path, strings.Join(info.Tasks, "/")),
			Fix:    "voiceime config set model /path/to/model.gguf",
		})
	default:
		report(Check{ID: "model", Status: "ok", Detail: describeCheckpoint(info)})
	}

	// The optional resident server: the one part of a working installation nothing
	// else verifies, because dictation keeps working when it cannot start.
	report(asrServerCheck(cfg, serverUnitInstalled()))
}
