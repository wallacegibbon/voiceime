package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/wallacegibbon/voiceime/internal/audio"
	"github.com/wallacegibbon/voiceime/internal/config"
	"github.com/wallacegibbon/voiceime/internal/daemon"
	"github.com/wallacegibbon/voiceime/internal/gnome"
	"github.com/wallacegibbon/voiceime/internal/ibus"
)

// engineInputSource is the desktop-level half of an install: the pair GNOME's own switcher
// and its indicator are built from.
func engineInputSource(name string) gnome.InputSource {
	return gnome.InputSource{Type: "ibus", ID: name}
}

// addInputSource makes the engine appear in the desktop's input-source list.
func addInputSource(name string) (string, map[string]any) {
	source := engineInputSource(name)
	report := map[string]any{"source": source.String()}
	if !gnome.InputSourcesAvailable() {
		report["supported"] = false
		return "no GNOME input-source list on this desktop", report
	}
	report["supported"] = true
	added, err := gnome.AddInputSource(source)
	if err != nil {
		report["added"] = false
		report["error"] = err.Error()
		return "could not add " + source.String() + " to the GNOME input sources: " + err.Error(), report
	}
	report["added"] = added
	report["listed"] = true
	if added {
		return "appended " + source.String() + " to the GNOME input sources; the sources already configured are untouched", report
	}
	return "already in the GNOME input sources", report
}

// removeInputSource takes the engine back out of that list.
func removeInputSource(name string) (bool, map[string]any) {
	source := engineInputSource(name)
	report := map[string]any{"source": source.String()}
	if !gnome.InputSourcesAvailable() {
		report["supported"] = false
		return false, report
	}
	report["supported"] = true
	removed, err := gnome.RemoveInputSource(source)
	if err != nil {
		report["error"] = err.Error()
		return false, report
	}
	report["removed"] = removed
	return removed, report
}

// engineRegistered asks the daemon whether it knows our engine. A failure to ask is reported
// apart from a "no": the two need different fixes, and only one of them is this program's.
func engineRegistered() (registered bool, why string, unreachable bool) {
	listing, err := probe("ibus", "list-engine")
	if err != nil {
		if ibusAnswerIsUnreachable(listing) {
			return false, "", true
		}
		return false, "ibus list-engine failed: " + err.Error(), false
	}
	if listEngineContains(listing, ibus.EngineName) {
		return true, "", false
	}
	return false, "not listed by ibus: run `voiceime apply`", false
}

// errIBusUnreachable marks the calls that never reached a daemon. It is a different situation
// from a daemon that answered "no": nothing that has to be asked of the daemon can be asked,
// and the reason is normally the shell the command was run from rather than the machine.
var errIBusUnreachable = errors.New("no ibus daemon could be reached")

// ibusAnswerIsUnreachable recognises the two shapes an ibus CLI answer takes when no daemon
// was reached: the tool's own sentence, and the GLib assertion about a bus object that is
// null. Both then print the same fallback sentence a real "nothing is selected" answer
// prints, which is what made an unreachable daemon read as a session with no input method.
//
// The answered case is deliberately not caught here: a daemon that is running and has no
// global engine answers with ibus_bus_call_sync and a DBus.Error.Failed about "No global
// engine", which is a bus talking.
func ibusAnswerIsUnreachable(answer string) bool {
	for _, line := range strings.Split(answer, "\n") {
		if strings.Contains(line, "Can't connect to IBus") || strings.Contains(line, "IBUS_IS_BUS") {
			return true
		}
	}
	return false
}

func stopEngines() (stopped int, forced bool) {
	engines := processesRunning("ibus-engine", "voiceime")
	for _, engine := range engines {
		if process, err := os.FindProcess(engine.PID); err == nil {
			_ = process.Signal(syscall.SIGTERM)
		}
	}
	// Wait for them to be gone, because the next thing needs the bus name they hold:
	// an engine that registers while another still owns the name fails with "the name
	// org.freedesktop.IBus.voiceime is already taken". Nothing here is being given
	// time to finish a sentence - a signalled engine drops the recording in progress
	// (Run returns, Disable discards it), and measured on this machine a live engine
	// is gone 7ms after SIGTERM. Six seconds is not that path's budget; it is the
	// point past which waiting has stopped being useful and the process is only going
	// to be stopped by force.
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if len(processesRunning("ibus-engine", "voiceime")) == 0 {
			return len(engines), false
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, engine := range processesRunning("ibus-engine", "voiceime") {
		if process, err := os.FindProcess(engine.PID); err == nil {
			_ = process.Kill()
			forced = true
		}
	}
	return len(engines), forced
}

// currentEngine is the engine the daemon routes this session's keys through. An empty name
// with no error is the state where no engine is selected, which is a different thing from
// not being able to ask; a call that could not ask says so with errIBusUnreachable.
func currentEngine() (string, error) {
	answer, err := probe("ibus", "engine")
	if ibusAnswerIsUnreachable(answer) {
		return "", fmt.Errorf("%w: %s", errIBusUnreachable, firstLine(answer))
	}
	name, known := parseEngineAnswer(answer, err)
	if !known {
		return "", fmt.Errorf("ibus engine: %v: %s", err, firstLine(answer))
	}
	return name, nil
}

// parseEngineAnswer reads the answer to `ibus engine`. "Nothing is selected" comes back as a
// failed command whose output is a library warning plus a sentence, so it is recognised by
// what it says: the exit status alone cannot tell an unselected engine from an absent
// daemon, and the two have different meanings.
//
// Nor can the sentence. A CLI that never reached a daemon prints "No engine is set." too,
// with an assertion about a bus object that is null above it (measured: a shell with no
// DISPLAY and no IBUS_ADDRESS answers exactly as a session with no input method selected
// does). That is a question that was not answered, not an answer of "none", so it is said
// apart before the sentence is read.
func parseEngineAnswer(answer string, err error) (name string, known bool) {
	if ibusAnswerIsUnreachable(answer) {
		return "", false
	}
	for _, line := range strings.Split(answer, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "", strings.Contains(line, "IBUS-WARNING"), strings.Contains(line, "IBUS-CRITICAL"):
			continue
		case strings.Contains(line, "No engine is set"), strings.Contains(line, "No global engine"):
			return "", true
		default:
			return line, true
		}
	}
	if err != nil {
		return "", false
	}
	return "", true
}

// selectEngine makes the daemon route this session's keys through our engine, which is what
// also starts the engine process if it is not running.
func selectEngine() error {
	return runCommand("ibus engine " + ibus.EngineName)
}

func runCommand(line string) error {
	parts := strings.Fields(line)
	if len(parts) == 0 {
		return nil
	}
	output, err := exec.Command(parts[0], parts[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %v: %s", line, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func cmdIBusEngine() *Command {
	var (
		address       string
		tracePath     string
		componentName string
		audioFile     string
	)
	return &Command{
		Name:    "ibus-engine",
		Summary: "Run the IBus engine process (spawned by ibus-daemon)",
		Usage:   "voiceime ibus-engine --ibus [--address unix:path=...]",
		Flags: func(set *flag.FlagSet) {
			set.Bool("ibus", false, "accepted and ignored; ibus-daemon passes it")
			set.StringVar(&address, "address", "", "IBus bus address (default: IBUS_ADDRESS or the registry file)")
			set.StringVar(&tracePath, "trace", "", "append incoming D-Bus calls and the session log to this file")
			set.StringVar(&audioFile, "audio-file", "",
				"diagnostic: dictate this WAV when the engine is activated, instead of waiting for the key")
			set.StringVar(&componentName, "name", ibus.DefaultComponentName,
				"bus name to own; must equal the component XML <name> of the file that spawned this process")
		},
		Run: func(ctx *Context, args []string) error {
			if tracePath != "" {
				// Enabled through the environment so the tracing code stays out of
				// the engine's own plumbing.
				os.Setenv("VOICEIME_IBUS_TRACE", tracePath)
			}
			return runIBusEngine(ctx, engineRunOptions{
				address:       address,
				componentName: componentName,
				audioFile:     audioFile,
				tracePath:     tracePath,
			})
		},
	}
}

// engineRunOptions is the engine process's whole configuration. It is a struct
// because the argument list had grown past the point where position is readable.
type engineRunOptions struct {
	address       string
	componentName string
	audioFile     string
	tracePath     string
}

func runIBusEngine(ctx *Context, options engineRunOptions) error {
	address, componentName := options.address, options.componentName
	audioFile, tracePath := options.audioFile, options.tracePath
	if address == "" {
		resolved, err := ibus.ResolveAddress(ibus.Ping)
		if err != nil {
			return PreconditionError("ibus restart", "%v", err)
		}
		address = resolved
	}
	component := ibus.DefaultComponent(ctx.Config, "voiceime ibus-engine --ibus")
	if componentName != "" {
		component.Name = componentName
	}

	runCtx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	logPath := tracePath
	if logPath == "" {
		logPath = engineLogPath()
	}
	if logPath != "" {
		// One file per engine lifetime, bounded: a log that grows across months of
		// dictation is a disk leak with a useful name.
		rotateLog(logPath)
	}
	logf := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		if logPath != "" {
			appendLogLine(logPath, "session: "+line)
		}
		if file, ok := ctx.Stderr.(*os.File); ok && isTerminalFile(file) {
			fmt.Fprintf(ctx.Stderr, "voiceime: %s\n", line)
		}
	}

	// The settings are checked here - where a settings file becomes an engine - and not where
	// they were written. The file may have been edited by hand since, and the two values that
	// make one request can have been pulled apart since. The reason goes into this session's
	// log, the one file that says what an engine did, and the process then refuses to be an
	// engine: starting and answering nothing is harder to notice than not starting and saying
	// why, and `doctor` reports the missing engine with its own fix.
	if issue, bad := firstBlockingIssue(ctx.Config); bad {
		logf("not starting: the settings cannot work: %s: %s (%s)", issue.Key, issue.Message, issue.Fix)
		return PreconditionError(issue.Fix, "the settings cannot work: %s: %s", issue.Key, issue.Message)
	}

	// Open the microphone once before anybody holds the key - see audio.WarmUpCapture. It
	// settles what the capture client must be told about its output, answers whether there is
	// a client at all, and pays the sound server's first open of the device (1.09s measured on
	// this machine, and paid by the first dictation after a login otherwise) here rather than
	// between a key coming down and the first sample. The audio it captures is thrown away.
	// The file diagnostic has no microphone to open.
	if audioFile == "" {
		if tool, took, err := audio.WarmUpCapture(ctx.Config); err != nil {
			logf("nothing to record with: %v", err)
		} else {
			logf("capture ready: %s (first sample after %v)", tool, took.Round(time.Millisecond))
		}
	}

	// One pipeline, whether the audio comes from the microphone or from a file: the
	// diagnostic run is the real path with a different source, not a shorter path that
	// proves less.
	pipeline, err := newPushToTalk(ctx, logf, audioFile)
	if err != nil {
		return err
	}
	ctx.Print("engine started on %s; hold %s to dictate", address, pipeline.TriggerSpec())
	engineErr := ibus.Run(runCtx, component, pipeline, address)
	if engineErr != nil {
		// The last line this session's log gets, and the one that says why the engine
		// ended: an engine that is not running is an input method that answers nothing, and
		// the ways it can end have different causes.
		logf("engine process ending: %v", engineErr)
		return FailureError("%v", engineErr)
	}
	// Run returns nil only when a signal ended it, which is `voiceime apply`
	// releasing the process, or the session closing.
	logf("engine process ending: a signal ended it")
	return ctx.Emit("ibus-engine", map[string]any{"address": address, "stopped": true})
}

func newPushToTalk(ctx *Context, logf func(string, ...any), audioFile string) (*daemon.PushToTalk, error) {
	if strings.TrimSpace(ctx.Config.ASRServerURL) == "" {
		return nil, PreconditionError("voiceime apply",
			"dictation needs the recognition server: nothing is configured to decode audio")
	}
	handler, err := daemon.NewPushToTalk(ctx.Config, logf)
	if err != nil {
		return nil, PreconditionError(ctx.Config.FixLine("push_to_talk", `"<Control>apostrophe"`), "%v", err)
	}
	logf("%s", serverDescription(ctx.Config))
	if audioFile != "" {
		// Validated here rather than at first use: a typo in the path should fail
		// when the engine starts, not silently later.
		expanded := config.Expand(audioFile)
		if _, err := os.Stat(expanded); err != nil {
			return nil, FailureError("--audio-file %s: %v", expanded, err)
		}
		handler.AudioFile = expanded
	}
	return handler, nil
}

func appendLogLine(path, line string) {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	// Timestamped: the point of this file is to line a dictation up with the journal of
	// the service it talked to, and "session: listening" alone cannot be placed in time.
	// 0600 because the lines quote what the user said.
	fmt.Fprintf(file, "%s %s\n", time.Now().Format(time.RFC3339), line)
}

// isTerminalFile reports whether writes go to a terminal rather than a pipe or a file.
// A character device is the test: /dev/null is one too, and writing to it is harmless.
func isTerminalFile(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func engineLogPath() string { return filepath.Join(config.CacheDir(), "engine.log") }

// maxEngineLogBytes bounds that file. 1 MiB is thousands of dictations; the cap is there
// so a long-lived install cannot fill a disk with the record of what it said.
const maxEngineLogBytes = 1 << 20

// rotateLog keeps the session log bounded: the cap is a property of the file, not of the
// caller, so there is one value for it.
func rotateLog(path string) {
	info, err := os.Stat(path)
	if err != nil || info.Size() <= maxEngineLogBytes {
		return
	}
	_ = os.Truncate(path, 0)
}
