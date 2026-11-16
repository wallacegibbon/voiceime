package cli

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/ibus"
)

// cmdApply is the whole of the machine's side of voiceime: make what is installed match
// config.toml.
//
// There used to be a verb per part - `server install`, `server restart`, `engine install
// --apply`, `config apply`, `config validate` - and each had its own idea of what was
// installed and in which order. The work is one decision ("it should look like the
// configuration"), so it is one command: idempotent, non-interactive, and safe to run
// after any change or as the last step of an install. `voiceime doctor` says what is wrong;
// this makes it right.
func cmdApply() *Command {
	return &Command{
		Name:    "apply",
		Summary: "Make the desktop use the engine: component file, input source, engine processes",
		Usage:   "voiceime apply",
		Flags:   func(set *flag.FlagSet) {},
		Run: func(ctx *Context, args []string) error {
			return runApply(ctx)
		},
	}
}

// cmdRemove is its opposite: take the parts this project installed away again. One command
// because they are one installation: the component file, the entry the desktop's switcher
// reads, the environment entry that makes a user-level install survive a login. Nothing else
// belongs to that install, so nothing else is looked at: what answers the address in the
// settings was not put there by this program, and this program has no name for it.
func cmdRemove() *Command {
	return &Command{
		Name:    "remove",
		Summary: "Remove the engine, its desktop entry, and the session entry that installed them",
		Usage:   "voiceime remove",
		Flags:   func(set *flag.FlagSet) {},
		Run: func(ctx *Context, args []string) error {
			return runRemove(ctx)
		},
	}
}

// applyReport is one line per thing this command installs: the file that makes the engine
// reachable, the desktop's list, the running engine processes. What answers the address is not
// one of them: this command neither runs it nor has a name for it, and an endpoint elsewhere
// needs nothing installed here at all.
type applyReport struct {
	Component string `json:"component"`
	// Desktop is the input-source list the desktop's own switcher reads. The entry the
	// engine needs there belongs to its name, so it is converged with everything else.
	Desktop string `json:"desktop"`
	Engine  string `json:"engine"`
}

func runApply(ctx *Context) error {
	// A configuration that cannot work is not applied halfway: the settings are checked
	// first so that a failure leaves every part of the installation reading the same file it
	// was reading before.
	if issue, bad := firstBlockingIssue(ctx.Config); bad {
		return PreconditionError(issue.Fix, "cannot apply the configuration: %s: %s", issue.Key, issue.Message)
	}

	var report applyReport
	var err error
	if report.Component, err = applyComponent(ctx); err != nil {
		return FailureError("%v", err)
	}
	// Not fatal on its own: an engine missing from this list still receives keystrokes,
	// it just cannot be shown by the desktop or switched to from its menu.
	report.Desktop, _ = addInputSource(ibus.EngineName)
	report.Engine = dropEngines()

	if !ctx.JSON {
		ctx.Print("component: %v", report.Component)
		ctx.Print("desktop:   %v", report.Desktop)
		ctx.Print("engine:    %v", report.Engine)
		ctx.Print("")
		ctx.Print("`voiceime doctor` re-checks all of it.")
	}
	return ctx.Emit("apply", report)
}

// applyComponent writes the two files that make the engine reachable: the component file
// that names this binary, and the environment entry that makes the daemon look in the
// directory a user-level install uses. A missing one is written; a stale one - a moved
// checkout, a rebuilt binary, another build's engine name - is rewritten. Both are what
// "the engine is not found after I rebuilt" used to mean.
func applyComponent(ctx *Context) (string, error) {
	if ibus.SystemInstalled() {
		// A system-wide component is an administrator's file. This command does not rewrite
		// it, and no environment entry is needed for it.
		return ibus.SystemComponentPath() + " (system-wide, left alone)", nil
	}
	if config.FindExecutable("ibus") == "" {
		// The settings may be perfectly good and the machine have no desktop at all: a
		// headless box can hold a model for someone else's engine. This command's whole job is
		// the engine, so it says so rather than reporting success with nothing installed.
		return "", PreconditionError("sudo apt install ibus",
			"IBus is not installed, so there is no engine to register")
	}

	// The file is read once, both as bytes (to compare with what this version would write)
	// and parsed (to keep the parts of it that are not this build's business).
	raw, readErr := os.ReadFile(ibus.ComponentPath())
	installed, parseErr := ibus.ParseComponent(raw)
	// Every branch below decides both halves: which file is the one to write, and whether
	// it has to be written at all.
	var (
		component ibus.Component
		changed   bool
	)
	switch {
	case readErr != nil || parseErr != nil:
		// Nothing installed, or a file this version cannot read: write what a fresh
		// install writes, with the path of this binary in it.
		binary, err := os.Executable()
		if err != nil {
			return "", FailureError("cannot locate this binary: %v", err)
		}
		component = ibus.DefaultComponent(ctx.Config, binary+" ibus-engine --ibus")
		changed = true
	case componentNeedsRewrite(raw, withCurrentEngine(installed)):
		component = withCurrentEngine(installed)
		changed = true
	default:
		component = installed
	}

	// The search path lives in the session, so it has to be written down somewhere a login
	// reads. Skipping that is the install that works until the machine is rebooted.
	environmentWritten := false
	if !ibus.EnvironmentSet() {
		if _, err := ibus.WriteEnvironment(); err != nil {
			return "", FailureError("write %s: %v", ibus.EnvironmentFile(), err)
		}
		environmentWritten = true
	}

	if !changed && !environmentWritten {
		return ibus.ComponentPath() + " (unchanged)", nil
	}
	if changed {
		if _, err := ibus.Install(component); err != nil {
			return "", FailureError("%v", err)
		}
	}
	if environmentWritten {
		if err := runCommand("dbus-update-activation-environment --systemd " +
			ibus.ComponentPathVariable + "=" + ibus.EnvironmentPath()); err != nil {
			return "", FailureError("the component was written but this session was not told about it: %v", err)
		}
	}
	// The daemon caches the component registry beside the directories it watched when the
	// cache was written, so a changed file needs that cache gone as well as a restart
	// (measured; see ibus.RegistryCachePath and docs/ibus-protocol.md).
	if cache := ibus.RegistryCachePath(); cache != "" {
		_ = os.Remove(cache)
	}
	if err := runCommand("ibus restart"); err != nil {
		return "", FailureError("the component was written but IBus did not restart, so it is still reading the old one: %v", err)
	}
	if err := waitForIBus(); err != nil {
		return "", FailureError("IBus was restarted but did not come back: %v (run `ibus restart`, then `voiceime apply` again)", err)
	}
	switch {
	case changed:
		return ibus.ComponentPath() + " (written); IBus restarted", nil
	default:
		return ibus.ComponentPath() + " (unchanged); the search path was missing, so it was written", nil
	}
}

// waitForIBus waits for the daemon to answer again after a restart. `ibus restart` returns
// while the old daemon is still going away, and the commands that follow this one - reading
// what the session uses, and selecting ours - are exactly the ones that fail in between.
// Measured: the first `ibus engine` after a restart answers `IBUS-CRITICAL …
// ibus_bus_set_global_engine: assertion 'IBUS_IS_BUS (bus)' failed`, which is what made
// `apply` report that it could not select the input method when it had just restarted the
// daemon that would let it. `ibus list-engine` is the same test doctor uses for a daemon
// that knows our engine.
func waitForIBus() error {
	deadline := time.Now().Add(ibusRestartWait)
	for {
		if _, err := probe("ibus", "list-engine"); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("no engine list from ibus within %s", ibusRestartWait)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// ibusRestartWait bounds that wait. A daemon that does not come back is an install with no
// text delivery at all, so the wait ends with a finding rather than with a hang.
const ibusRestartWait = 20 * time.Second

// componentNeedsRewrite compares the file on disk with what this version would write.
func componentNeedsRewrite(raw []byte, updated ibus.Component) bool {
	return string(raw) != updated.XML()
}

// withCurrentEngine is what the component file on disk would look like if this build wrote
// it: unchanged except for the engine identity, which is the one part of that file a new
// build can disagree with.
func withCurrentEngine(installed ibus.Component) ibus.Component {
	updated := installed
	updated.EngineName = ibus.EngineName
	return updated
}

// runRemove stops and deletes what `apply` created. It leaves the user's own input methods
// alone: only this engine's entry is taken out of the desktop's list. Nothing else on the
// machine is touched - what answers the settings' address is not something this command knows
// about, and stopping it is not what "remove the engine" asks for.
func runRemove(ctx *Context) error {
	removed := []string{}

	// The engine processes go first: a running one holds the session's input method, and
	// removing the component underneath it leaves the applications showing an input method
	// that no longer exists.
	stopped, _ := stopEngines()

	// The files are removed through the package that owns them, so "which file is
	// voiceime's" is written down once.
	if ibus.Installed() {
		if err := ibus.Uninstall(); err != nil {
			return FailureError("cannot remove %s: %v", ibus.ComponentPath(), err)
		}
		removed = append(removed, ibus.ComponentPath())
	}
	if existed, err := ibus.RemoveEnvironment(); err != nil {
		return FailureError("cannot remove %s: %v", ibus.EnvironmentFile(), err)
	} else if existed {
		removed = append(removed, ibus.EnvironmentFile())
	}
	if cache := ibus.RegistryCachePath(); cache != "" {
		_ = os.Remove(cache)
	}
	removedSource, inputSource := removeInputSource(ibus.EngineName)
	if removedSource {
		removed = append(removed, "the input source "+fmt.Sprint(inputSource["source"]))
	}
	// The settings file is left alone. What was configured may be an endpoint the user pays
	// for, and "uninstall the program" is not permission to delete what they wrote: nothing
	// in this program wrote that file, nothing in it prints the file back, and after this
	// command the file is still the whole record of the setup - the endpoint finding will go
	// on saying whether anything answers what is written in it.

	if !ctx.JSON {
		if len(removed) == 0 {
			ctx.Print("nothing was installed")
		}
		for _, entry := range removed {
			ctx.Print("removed %s", entry)
		}
	}
	document := map[string]any{
		"removed": removed, "engines_stopped": stopped, "input_source": inputSource,
	}
	return ctx.Emit("remove", document)
}

// dropEngines stops the engine processes, and puts the session's input method back.
//
// Stopping them is what makes a rebuilt binary take effect. It also does more than stop a
// process: the daemon clears the global engine when the bus name disappears, which leaves
// every application in the session showing the input method in the desktop's switcher while
// receiving no keys at all (measured: `ibus engine` answers "No engine is set." afterwards).
// Nothing puts it back by itself - the process is not respawned until an input method is
// selected again - so the person at the keyboard finds the workaround on their own, switches
// to another input method and back, and reasonably blames the engine for it.
func dropEngines() string {
	// What the session is using has to be read before anything is stopped: afterwards there
	// is nothing left to read.
	current, known := "", false
	if name, err := currentEngine(); err == nil {
		current, known = name, true
	}
	stopped, forced := stopEngines()

	outcome := "was not running"
	if stopped > 0 {
		outcome = fmt.Sprintf("%d dropped", stopped)
		if forced {
			outcome += " (one had to be killed)"
		}
	}
	if !reselectsEngine(current, known) {
		return outcome
	}
	if err := selectEngine(); err != nil {
		return outcome + "; the input method could not be selected again (" + err.Error() +
			"), so no application will receive keys until one is: ibus engine " + ibus.EngineName
	}
	return outcome + "; the input method was selected again"
}

// reselectsEngine says whether the session's input method should be put back after the
// engine processes are stopped: ours, or none at all, which is what stopping one leaves
// behind. Any other engine is the user's choice and is left alone.
func reselectsEngine(current string, known bool) bool {
	return known && (current == ibus.EngineName || current == "")
}
