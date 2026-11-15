package cli

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

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

// engineRegistered asks the daemon whether it knows our engine.
func engineRegistered() (bool, string) {
	listing, err := probe("ibus", "list-engine")
	if err != nil {
		return false, "ibus list-engine failed: " + err.Error()
	}
	if listEngineContains(listing, ibus.EngineName) {
		return true, ""
	}
	return false, "not listed by ibus: run `voiceime apply`"
}

func stopEngines() (stopped int, forced bool) {
	engines := processesRunning("ibus-engine")
	for _, engine := range engines {
		if process, err := os.FindProcess(engine.PID); err == nil {
			_ = process.Signal(syscall.SIGTERM)
		}
	}
	// Give the engine the time its shutdown path needs to commit the sentence in
	// progress; the drain budget is 5s (D25), so six is enough.
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if len(processesRunning("ibus-engine")) == 0 {
			return len(engines), false
		}
		time.Sleep(100 * time.Millisecond)
	}
	for _, engine := range processesRunning("ibus-engine") {
		if process, err := os.FindProcess(engine.PID); err == nil {
			_ = process.Kill()
			forced = true
		}
	}
	return len(engines), forced
}

// currentEngine is the engine the daemon routes this session's keys through. An empty name
// with no error is the state where no engine is selected, which is a different thing from
// not being able to ask.
func currentEngine() (string, error) {
	answer, err := probe("ibus", "engine")
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
func parseEngineAnswer(answer string, err error) (name string, known bool) {
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
		return nil, PreconditionError(`voiceime config set push_to_talk "<Control>apostrophe"`, "%v", err)
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
