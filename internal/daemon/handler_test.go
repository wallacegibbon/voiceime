package daemon

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/wallacegibbon/voiceime/internal/config"
)

// recordingControl stands in for one input context: it records what the session tried
// to deliver, and which context received it.
type recordingControl struct {
	mu       sync.Mutex
	commits  []string
	preedits []string
}

func (c *recordingControl) Commit(text string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.commits = append(c.commits, text)
	return nil
}

func (c *recordingControl) Preedit(text string, cursor int, visible bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.preedits = append(c.preedits, text)
	return nil
}

func (c *recordingControl) DeleteSurroundingText(offset int32, chars uint32) error { return nil }

func (c *recordingControl) delivered() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.commits...)
}

// newTestEngineHandler wires the handler the way the engine process does - a resident
// server and a WAV instead of a microphone - so a session really starts and stops.
func newTestEngineHandler(t *testing.T, server *fakeASRServer) *EngineHandler {
	t.Helper()
	cfg := config.Default()
	cfg.ASRServerURL = server.url
	cfg.RefineOnSegmentEnd = true
	return &EngineHandler{
		Config:      cfg,
		ServerURL:   server.url,
		StreamModel: "stream",
		RepairModel: "refine",
		AudioFile:   speechWAV(t),
		Log:         func(string, ...any) {},
	}
}

func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The session must follow the input context the user is in, and only stop when the last
// one is gone.
//
// Enable is called once per context. The session used to be fixed to the first one - so a
// second window received nothing, because the text still went to the first - and any
// Disable stopped it, so moving between two windows ended dictation mid-sentence. Both
// were invisible from the outside: text appeared, just in the wrong place, or stopped
// appearing.
func TestEngineHandlerFollowsTheNewestInputContext(t *testing.T) {
	server := newFakeASRServer(t)
	handler := newTestEngineHandler(t, server)

	first := &recordingControl{}
	handler.Enable(first)
	waitFor(t, "the first session to open", func() bool { return server.liveCount() == 1 })

	// A second context: the same dictation, a new caret.
	second := &recordingControl{}
	handler.Enable(second)
	if got := handler.sink.target(); got != any(second) {
		t.Fatalf("the sink delivers to %v, want the newest input context", got)
	}
	if got := server.liveCount(); got != 1 {
		t.Errorf("opened %d dictation sessions for two input contexts, want 1", got)
	}

	// One context going away leaves dictation running in the other.
	handler.Disable()
	third := &recordingControl{}
	handler.Enable(third)
	if got := handler.sink.target(); got != any(third) {
		t.Fatalf("the sink delivers to %v after a disable and a re-enable", got)
	}
	if got := server.liveCount(); got != 1 {
		t.Errorf("opened %d sessions: one context leaving stopped the dictation", got)
	}

	// The last two going away stops it, and the next activation is a new session.
	handler.Disable()
	handler.Disable()
	waitFor(t, "the session to stop", func() bool { return handler.sink != nil && !handlerRunning(handler) })

	fourth := &recordingControl{}
	handler.Enable(fourth)
	waitFor(t, "the next session to open", func() bool { return server.liveCount() == 2 })
	handler.Disable()
}

// handlerRunning reports whether a session is in flight. It reads the same field Enable
// and Disable use, under the same lock.
func handlerRunning(h *EngineHandler) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cancel != nil
}

// The sink must reach whichever context is current, including text the session commits
// from its own goroutine after a rebind.
func TestSwitchableSinkFollowsItsTarget(t *testing.T) {
	sink := &switchableSink{}
	if err := sink.Commit("ignored"); err != nil {
		t.Fatalf("Commit with no target: %v", err)
	}
	if err := sink.Preedit("ignored", 0, false); err != nil {
		t.Fatalf("Preedit with no target: %v", err)
	}

	first, second := &recordingControl{}, &recordingControl{}
	sink.use(first)
	if err := sink.Commit("一"); err != nil {
		t.Fatal(err)
	}
	sink.use(second)
	if err := sink.Commit("二"); err != nil {
		t.Fatal(err)
	}
	if got := first.delivered(); len(got) != 1 || got[0] != "一" {
		t.Errorf("first context received %v, want only its own text", got)
	}
	if got := second.delivered(); len(got) != 1 || got[0] != "二" {
		t.Errorf("second context received %v, want only its own text", got)
	}
}

// The engine's log has to say that text left it.
//
// "I pressed the key and nothing appeared" has two very different causes: the session
// heard nothing, or the text was delivered and the application did not show it. The log
// could not tell them apart, because a commit was the one thing it did not record.
func TestEngineLogsWhatItCommits(t *testing.T) {
	server := newFakeASRServer(t)
	server.repairText = "记录下来的句子。"
	handler := newTestEngineHandler(t, server)

	var mu sync.Mutex
	var lines []string
	handler.Log = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}

	control := &recordingControl{}
	handler.Enable(control)
	waitFor(t, "a commit", func() bool { return len(control.delivered()) > 0 })
	handler.Disable()

	mu.Lock()
	logged := strings.Join(lines, "\n")
	mu.Unlock()
	if !strings.Contains(logged, "committed") {
		t.Errorf("the log does not record the commit:\n%s", logged)
	}
	if !strings.Contains(logged, "记录下来的句子") {
		t.Errorf("the log does not quote what was committed:\n%s", logged)
	}
	for _, want := range []string{"listening", "utterance", "repaired"} {
		if !strings.Contains(logged, want) {
			t.Errorf("the log does not record %q:\n%s", want, logged)
		}
	}
}

// A long sentence is shortened in the log, and never split mid-character.
func TestFirstRunesShortensWithoutSplittingCharacters(t *testing.T) {
	if got := firstRunes("你好世界", 10); got != "你好世界" {
		t.Errorf("firstRunes kept %q, want it unchanged", got)
	}
	got := firstRunes("一二三四五", 3)
	if got != "一二三…" {
		t.Errorf("firstRunes = %q, want 一二三…", got)
	}
	if !utf8.ValidString(got) {
		t.Errorf("%q is not valid UTF-8", got)
	}
}
