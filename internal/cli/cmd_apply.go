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
	Service string   `json:"service"`
	Served  []string `json:"served,omitempty"`
	// AudioCPP, Backend and Threads are what deployment settled for this machine. They are
	// reported because they are not in the settings any more, and a caller is entitled to
	// know which binary answers its requests.
	AudioCPP    string   `json:"audio_cpp,omitempty"`
	Backend     string   `json:"backend,omitempty"`
	Threads     int      `json:"threads,omitempty"`
	OtherBuilds []string `json:"other_audio_cpp,omitempty"`
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

	report, err := applyService(ctx)
	if err != nil {
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
		ctx.Print("service:   %v", report.Service)
		if len(report.Served) > 0 {
			ctx.Print("serving:   %s (%s, %d threads)", strings.Join(report.Served, " and "),
				report.Backend, report.Threads)
		}
		if report.AudioCPP != "" {
			ctx.Print("audio.cpp: %s", report.AudioCPP)
		}
		for _, other := range report.OtherBuilds {
			ctx.Print("           another build is here: %s", other)
		}
		ctx.Print("component: %v", report.Component)
		ctx.Print("desktop:   %v", report.Desktop)
		ctx.Print("engine:    %v", report.Engine)
		ctx.Print("")
		ctx.Print("`voiceime doctor` re-checks all of it, and `voiceime transcribe` proves the path.")
	}
	return ctx.Emit("apply", report)
}

// applyService fills the part of the report that describes the recognition service, and
// returns it whether or not it succeeded: what was settled before a failure is worth knowing.
//
// Everything machine-shaped is settled here, once, and written where it is used: which
// audio.cpp to run goes into the unit's ExecStart, and the backend and worker count go into
// the audio.cpp config that unit names. Nothing at start-up or run time looks any of it up
// again, so a service started at login cannot fail for a reason only another install could
// fix. Rewriting the unit when its contents differ is what heals a moved checkout or a new
// binary, which is what `doctor`'s stale findings used to be about.
//
// The wait at the end is what makes "applied" a fact rather than a request: a service that is
// started but still loading, or still serving the previous model, has not applied anything.
func applyService(ctx *Context) (applyReport, error) {
	var report applyReport
	cfg := ctx.Config
	baseURL := strings.TrimSpace(cfg.ASRServerURL)
	host, port := "127.0.0.1", server.DefaultPort
	if baseURL != "" {
		var err error
		if host, port, err = server.ParseURL(baseURL); err != nil {
			return report, FailureError("%v", err)
		}
		if !server.IsLocalHost(host) {
			// The settings name a server somewhere else: running one here would contradict
			// them, not complete them.
			report.Service = fmt.Sprintf("%s is served elsewhere, so nothing runs here", baseURL)
			return report, nil
		}
	}

	asrBinary, _, err := config.ASRServer()
	if err != nil {
		return report, PreconditionError(
			"build audio.cpp (cmake -B build && cmake --build build), then `voiceime apply`", "%v", err)
	}
	report.AudioCPP = asrBinary
	report.Backend, report.Threads, err = resolveEngine(asrBinary)
	if err != nil {
		return report, err
	}
	// A machine holding more than one build is reported rather than decided silently: the two
	// can be different binaries with the same banner (docs/measurements.md).
	if builds := config.DiscoverASRServers(); len(builds) > 1 {
		report.OtherBuilds = builds[1:]
	}

	spec, err := buildServerSpec(cfg, port, report.Backend, report.Threads)
	if err != nil {
		return report, err
	}

	// The address and the checkpoint go into the settings first, because that is where the
	// engine reads them: the file has to describe the service that is running, not the one
	// intended. Writing them before the generated config and the unit is what keeps a failed
	// apply from leaving a unit that names a settings file which never named it.
	for _, change := range []struct{ key, value string }{
		{"asr_server_url", spec.BaseURL()},
		{"model", spec.Model},
	} {
		if err := cfg.Set(change.key, change.value); err != nil {
			return report, FailureError("config set %s: %v", change.key, err)
		}
	}
	if err := config.Save(cfg, ctx.ConfigPath); err != nil {
		return report, FailureError("write %s: %v", ctx.ConfigPath, err)
	}

	generated, err := writeServerConfig(spec)
	if err != nil {
		return report, FailureError("%v", err)
	}
	report.Served = []string{filepath.Base(spec.Model)}

	unitPath := serverUnitPath()
	unit := serverUnit(asrBinary, generated)
	start := fmt.Sprintf("%s --config %s --log", asrBinary, generated)
	if _, lookupErr := exec.LookPath("systemctl"); lookupErr != nil {
		report.Service = "no systemd user session: start it with `" + start + "`"
		return report, nil
	}
	if unitPath == "" {
		report.Service = "no place to write a service unit (XDG_CONFIG_HOME is unset): start it with `" +
			start + "`"
		return report, nil
	}

	wroteUnit := false
	if current, readErr := os.ReadFile(unitPath); readErr != nil || string(current) != unit {
		wroteUnit = true
		if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
			return report, FailureError("%v", err)
		}
		if err := os.WriteFile(unitPath, []byte(unit), 0o644); err != nil {
			return report, FailureError("%v", err)
		}
		if err := runCommand("systemctl --user daemon-reload"); err != nil {
			return report, FailureError("%v", err)
		}
		// Enabled so a login starts the model resident, whether or not anyone dictates.
		if err := runCommand("systemctl --user enable " + serverUnitName); err != nil {
			return report, FailureError("%v", err)
		}
	}
	if err := runCommand("systemctl --user restart " + serverUnitName); err != nil {
		return report, FailureError("systemctl --user restart %s: %v", serverUnitName, err)
	}

	probeCtx, cancel := context.WithTimeout(context.Background(), modelLoadTimeout)
	defer cancel()
	if err := waitForModel(probeCtx, spec.BaseURL(), spec); err != nil {
		return report, FailureError("the service was started but is not serving %s: %v (see `systemctl --user status %s`)",
			filepath.Base(spec.Model), err, serverUnitName)
	}
	if wroteUnit {
		report.Service = fmt.Sprintf("installed and started (%s)", serverUnitName)
		return report, nil
	}
	report.Service = "restarted"
	return report, nil
}

// resolveEngine asks the binary what it supports and the machine how much room it has. A
// backend is not a preference: an audio.cpp built without cuda cannot do cuda, and the build's
// own banner is the only authority on that. cpu is preferred when a build offers it because
// cpu is the only backend measured for this project (docs/measurements.md); the others stay
// unavailable rather than becoming an unmeasured default.
func resolveEngine(binary string) (backend string, threads int, err error) {
	supported := supportedBackends(binary)
	if len(supported) == 0 {
		return "", 0, PreconditionError("voiceime apply with a working audio.cpp build",
			"%s did not report any backend it supports", binary)
	}
	backend = supported[0]
	for _, candidate := range supported {
		if candidate == "cpu" {
			backend = "cpu"
			break
		}
	}
	// One worker per physical core: the streaming decoder and the repair pass share them, so
	// more contend than help.
	if threads = physicalCores(); threads <= 0 {
		threads = 4
	}
	return backend, threads, nil
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
