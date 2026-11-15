package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/ibus"
	"github.com/wallacegibbon/voiceime/internal/server"
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
		Summary: "Make this machine match the configuration: service, component, desktop, engine",
		Usage:   "voiceime apply",
		Flags:   func(set *flag.FlagSet) {},
		Run: func(ctx *Context, args []string) error {
			return runApply(ctx)
		},
	}
}

// cmdRemove is its opposite: take the parts this tool installed away again. One command
// because they are one installation: a service unit, a component file, the entry the
// desktop's switcher reads, and the environment entry that makes a user-level install
// survive a login.
func cmdRemove() *Command {
	return &Command{
		Name:    "remove",
		Summary: "Remove the service, the engine and the desktop entry this tool installed",
		Usage:   "voiceime remove",
		Flags:   func(set *flag.FlagSet) {},
		Run: func(ctx *Context, args []string) error {
			return runRemove(ctx)
		},
	}
}

// applyReport is one line per thing that reads the configuration.
type applyReport struct {
	// ASRBin and OtherBuilds are present only on a machine that had no audio.cpp build
	// recorded: apply finds one, writes it down, and says which other builds it did not
	// choose. An install that already names a runnable file never sees these keys.
	ASRBin      string   `json:"asr_bin,omitempty"`
	OtherBuilds []string `json:"other_builds,omitempty"`
	Service     string   `json:"service"`
	Served      []string `json:"served,omitempty"`
	Component   string   `json:"component"`
	// Desktop is the input-source list the desktop's own switcher reads. The entry the
	// engine needs there belongs to its name, so it is converged with everything else.
	Desktop string `json:"desktop"`
	Engine  string `json:"engine"`
}

func runApply(ctx *Context) error {
	// A configuration that cannot work is not applied halfway: the settings are checked
	// first so that a failure leaves every part of the installation reading the same file it
	// was reading before.
	if issues := config.Errors(ctx.Config.Validate()); len(issues) > 0 {
		issue := issues[0]
		return PreconditionError(issue.Fix, "cannot apply the configuration: %s: %s", issue.Key, issue.Message)
	}

	var report applyReport
	if err := pointAtASRBuild(ctx, &report); err != nil {
		return err
	}
	var err error
	if report.Service, report.Served, err = applyService(ctx); err != nil {
		return err
	}
	if report.Component, err = applyComponent(ctx); err != nil {
		return FailureError("%v", err)
	}
	// Not fatal on its own: an engine missing from this list still receives keystrokes,
	// it just cannot be shown by the desktop or switched to from its menu.
	report.Desktop, _ = addInputSource(ibus.EngineName)
	report.Engine = dropEngines()

	if !ctx.JSON {
		if report.ASRBin != "" {
			ctx.Print("asr_bin:   pointed at %s", report.ASRBin)
			for _, other := range report.OtherBuilds {
				ctx.Print("           %s is also built here: choose it with", other)
				ctx.Print("           `voiceime config set asr_bin %s`", other)
			}
		}
		ctx.Print("service:   %v", report.Service)
		if len(report.Served) > 0 {
			ctx.Print("serving:   %s", strings.Join(report.Served, " and "))
		}
		ctx.Print("component: %v", report.Component)
		ctx.Print("desktop:   %v", report.Desktop)
		ctx.Print("engine:    %v", report.Engine)
		ctx.Print("")
		ctx.Print("`voiceime doctor` re-checks all of it, and `voiceime transcribe` proves the path.")
	}
	return ctx.Emit("apply", report)
}

// pointAtASRBuild settles the one fact a new machine's settings cannot know: where audio.cpp
// was built. It used to be a step of its own (`config init`), which meant a fresh machine
// needed two commands in the right order and every later command silently did nothing if the
// first was skipped. It belongs here because this is the command that makes a machine ready:
// the search runs once, the answer goes into the settings apply is about to act on, and the
// same report says what was chosen. Afterwards every command reads that recorded path, so a
// build tree appearing, moving or gaining a second sibling cannot change what a running
// machine does - see FindASRBinary.
//
// Nothing is looked for when the settings already name a runnable file: finding a build is
// not overwriting a decision. When there is more than one build, the first is chosen and the
// rest are reported rather than kept to itself: which one to run is a decision about the
// machine, and the caller is the one who knows it.
func pointAtASRBuild(ctx *Context, report *applyReport) error {
	if _, err := ctx.Config.FindASRBinary(); err == nil {
		return nil
	}
	found := config.DiscoverASRBinaries()
	if len(found) == 0 {
		return nil
	}
	ctx.Config.ASRBin = found[0]
	report.ASRBin, report.OtherBuilds = found[0], found[1:]
	if err := config.Save(ctx.Config, ctx.ConfigPath); err != nil {
		return FailureError("cannot record the audio.cpp build in %s: %v", ctx.ConfigPath, err)
	}
	return nil
}

// applyService makes the recognition service exist and run what the settings say.
//
// "Installing" it is not a separate step: the unit runs `voiceime server run`, which renders
// audio.cpp's configuration from the settings at every start, so writing the unit when it is
// missing and restarting it when it is not are the same operation. The wait at the end is
// what makes "applied" a fact rather than a request: a service that is started but still
// loading, or still serving the previous model, has not applied anything.
func applyService(ctx *Context) (status string, served []string, err error) {
	cfg := ctx.Config
	baseURL := strings.TrimSpace(cfg.ASRServerURL)
	host, port := "127.0.0.1", server.DefaultPort
	if baseURL != "" {
		if host, port, err = server.ParseURL(baseURL); err != nil {
			return "", nil, FailureError("%v", err)
		}
		if !server.IsLocalHost(host) {
			// The settings name a server somewhere else: running one here would contradict
			// them, not complete them.
			return fmt.Sprintf("%s is served elsewhere, so nothing runs here", baseURL), nil, nil
		}
	}

	unitPath := serverUnitPath()

	spec, err := buildServerSpec(cfg, port)
	if err != nil {
		return "", nil, err
	}
	if _, err := cfg.FindServerBinary(); err != nil {
		// asr_bin names the CLI, and the server is found beside it: advice that told the
		// caller to point asr_bin at audiocpp_server would break the CLI lookup.
		return "", nil, PreconditionError("voiceime config set asr_bin /path/to/audio.cpp/build/<backend>/bin/audiocpp_cli",
			"the recognition server binary is not reachable: %v", err)
	}

	// Settings first, and before anything is started: a saved configuration must never
	// point at a unit that was never written, and the values below are what a machine
	// without a systemd user session needs in order to run the server some other way.
	for _, change := range []struct{ key, value string }{
		{"asr_server_url", spec.BaseURL()},
		{"model", spec.Model},
	} {
		if err := cfg.Set(change.key, change.value); err != nil {
			return "", nil, FailureError("config set %s: %v", change.key, err)
		}
	}
	if err := config.Save(cfg, ctx.ConfigPath); err != nil {
		return "", nil, FailureError("write %s: %v", ctx.ConfigPath, err)
	}

	if _, lookupErr := exec.LookPath("systemctl"); lookupErr != nil {
		return "no systemd user session: start it with `voiceime server run`", nil, nil
	}
	if unitPath == "" {
		return "no place to write a service unit (XDG_CONFIG_HOME is unset): start it with `voiceime server run`", nil, nil
	}

	wroteUnit := !fileExists(unitPath)
	if wroteUnit {
		if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
			return "", nil, FailureError("%v", err)
		}
		if err := os.WriteFile(unitPath, []byte(serverUnit(absoluteConfigPath(ctx))), 0o644); err != nil {
			return "", nil, FailureError("%v", err)
		}
		if err := runCommand("systemctl --user daemon-reload"); err != nil {
			return "", nil, FailureError("%v", err)
		}
		// Enabled so a login starts the model resident, whether or not anyone dictates.
		if err := runCommand("systemctl --user enable " + serverUnitName); err != nil {
			return "", nil, FailureError("%v", err)
		}
	}
	if err := runCommand("systemctl --user restart " + serverUnitName); err != nil {
		return "", nil, FailureError("systemctl --user restart %s: %v", serverUnitName, err)
	}

	probeCtx, cancel := context.WithTimeout(context.Background(), modelLoadTimeout)
	defer cancel()
	if err := waitForModel(probeCtx, spec.BaseURL(), spec); err != nil {
		return "", nil, FailureError("the service was started but is not serving %s: %v (see `systemctl --user status %s`)",
			filepath.Base(spec.Model), err, serverUnitName)
	}
	if wroteUnit {
		return fmt.Sprintf("installed and started (%s)", serverUnitName), []string{filepath.Base(spec.Model)}, nil
	}
	return "restarted", []string{filepath.Base(spec.Model)}, nil
}

// modelLoadTimeout bounds waiting for a model to become resident. Measured: the small
// checkpoint loads in seconds; the bound is there so a failed start reports a finding
// instead of hanging an install.
const modelLoadTimeout = 120 * time.Second

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
		// The other half of `apply` has already run, and it was worth running: a machine
		// with no desktop can still serve a model. What cannot be settled here is the
		// engine, so the finding says which half is missing and what installs it.
		return "", PreconditionError("sudo apt install ibus",
			"the recognition service is running, but IBus is not installed, so there is no engine to register")
	}

	// The file is read once, both as bytes (to compare with what this version would write)
	// and parsed (to keep the parts of it that are not this build's business).
	raw, readErr := os.ReadFile(ibus.ComponentPath())
	installed, parseErr := ibus.ParseComponent(raw)
	component, changed := ibus.Component{}, false
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
// alone: only this engine's entry is taken out of the desktop's list.
func runRemove(ctx *Context) error {
	removed := []string{}

	if _, err := exec.LookPath("systemctl"); err == nil {
		if fileExists(serverUnitPath()) {
			if err := runCommand("systemctl --user disable --now " + serverUnitName); err != nil {
				ctx.Print("could not stop %s: %v", serverUnitName, err)
			}
		}
	}
	// The engine processes go first: a running one holds the session's input method, and
	// removing the component underneath it leaves the applications showing an input method
	// that no longer exists.
	stopped, _ := stopEngines()

	// The files are removed through the package that owns them, so "which file is
	// voiceime's" is written down once. The unit and the generated audio.cpp config are
	// this command's own, so they are removed here.
	for _, path := range []string{serverUnitPath(), serverConfigFile()} {
		if path == "" {
			continue
		}
		if err := os.Remove(path); err == nil {
			removed = append(removed, path)
		}
	}
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
	if _, err := exec.LookPath("systemctl"); err == nil {
		_ = runCommand("systemctl --user daemon-reload")
	}
	removedSource, inputSource := removeInputSource(ibus.EngineName)
	if removedSource {
		removed = append(removed, "the input source "+fmt.Sprint(inputSource["source"]))
	}
	if ctx.Config.ASRServerURL != "" {
		if err := ctx.Config.Set("asr_server_url", ""); err == nil {
			_ = config.Save(ctx.Config, ctx.ConfigPath)
			removed = append(removed, "asr_server_url (from "+ctx.ConfigPath+")")
		}
	}

	if !ctx.JSON {
		if len(removed) == 0 {
			ctx.Print("nothing was installed")
		}
		for _, entry := range removed {
			ctx.Print("removed %s", entry)
		}
	}
	return ctx.Emit("remove", map[string]any{
		"removed": removed, "engines_stopped": stopped, "input_source": inputSource,
	})
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
