package cli

import (
	"sort"
	"strings"

	"github.com/wallacegibbon/voiceime/internal/config"
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
	issues := cfg.Validate()
	if blocking := config.Errors(issues); len(blocking) > 0 {
		var messages []string
		for _, issue := range blocking {
			messages = append(messages, issue.Key+": "+issue.Message)
		}
		report(Check{
			ID:     "config",
			Status: "fail",
			Detail: ctx.ConfigPath + ": " + strings.Join(messages, "; "),
			Fix:    blocking[0].Fix,
		})
	} else if len(issues) > 0 {
		// A finding that costs something without stopping anything. The mode of a file
		// holding a token is the case: nothing here writes that file to get it right.
		warning := issues[0]
		report(Check{
			ID:     "config",
			Status: "warn",
			Detail: ctx.ConfigPath + ": " + warning.Key + ": " + warning.Message,
			Fix:    warning.Fix,
		})
	} else {
		report(Check{ID: "config", Status: "ok", Detail: ctx.ConfigPath})
	}

	// 2. The address the engine posts to. This program has one question about it - can a
	// request name an id and get an answer back - and it does not ask what kind of thing is
	// behind it, or who was meant to have started that thing.
	report(asrEndpointCheck(ctx.Config))

	// 3. Capture.
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
			detail += " (no default source reported; `pactl list short sources`, then " +
				cfg.FixLine("capture_source", `"<name>"`) + ")"
		}
		report(Check{ID: "capture", Status: "ok", Detail: detail})
	}

	// 4. Text delivery through IBus: whether the bus is there, and whether it knows this
	// engine.
	ibusCheck(report)

	// 5. Optional feedback tools. Missing ones degrade UX, never function.
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

	// 6. Installed artifacts: the paths baked into the component file, and whether the
	// resident process runs that path. A moved checkout breaks these silently (see
	// install_check.go).
	installChecks(report)

	return checks
}
