# Decisions

Each entry records a decision, the reason, and what would change it. Numbers
referenced here are in [measurements.md](measurements.md).

## D1 — The interface is a configuration file and subcommands. There is no GUI.

**Decision.** voiceime has no graphical interface, and will not grow one. Settings
live in `~/.config/voiceime/config.toml`; every action is a subcommand.

**Why.** The requirement is that an automated agent can install, configure and
repair voiceime by reading documentation, with no human in the loop. A GUI cannot
be operated that way, and a GUI would also pull in a toolkit, a build dependency
on development headers and a windowing integration — the opposite of the
"size and distribution simplicity" requirement.

**Consequence.** Anything a dialog would have asked for must exist as a flag or a
config key. Transient feedback has no window to live in, so it has other homes:
IBus preedit shows uncommitted text at the caret of the application being typed
into, `notify-send` reports errors, and a short sound cue marks start and stop.

**What would change it.** Nothing foreseeable. A window would be a second front
end over the same daemon socket, but the project's value is in the pipeline.

## D2 — Every command is non-interactive, idempotent and machine-readable.

**Decision.** No prompts; `--json` on every command; distinct exit codes for
usage errors, missing prerequisites and genuine failures; validation findings
carry a `fix` field naming the command that resolves them.

**Why.** These properties are what make the surface agent-operable rather than
merely scriptable. An agent needs to distinguish "I typed it wrong" (2) from
"audio.cpp is not installed" (3) from "the model refused to load" (1) without
parsing prose, and it needs a next action rather than a diagnosis.

**Consequence.** `voiceime capabilities` prints the whole surface — commands,
flags, settings and detected environment — derived from live code so it cannot go
stale. `voiceime doctor` returns findings that each carry their own fix command,
so the loop is: doctor, run the fixes it printed, doctor again.

## D3 — Go, with no cgo.

**Decision.** The implementation language is Go, and the build must work with
`CGO_ENABLED=0`.

**Why.** Size and distribution are the stated priorities: Go produces one static
binary with no interpreter and no runtime to install, and it cross-compiles to the
future Windows and macOS targets without a toolchain per platform. The
alternative considered first was Python (fast iteration, ready IBus and GTK
bindings; audio.cpp's own model manager is Python) and it lost on distribution,
not on capability. GTK bindings were also rejected outright: the no-GUI decision
removes the only reason to link a toolkit, and with it every cgo build dependency.

**Consequence.** OS-specific work is confined to two small seams: audio capture
(a subprocess) and text delivery (IBus here; SendInput or the Windows text input
host later, CGEvent on macOS). `go build` is the whole build. The cost is the IBus
engine having to speak D-Bus directly — see D6.

## D4 — The streaming interface is the audio.cpp CLI over a pipe, not the C API.

**Decision.** voiceime spawns `audiocpp_cli --mode streaming --audio -` per
dictation session, writes raw PCM to stdin and parses the line protocol on stdout.
It does not link the C API or run the server.

**Why.** Three reasons, in order of weight:

1. It keeps the dependency on audio.cpp to "the binary the user installed",
   which is the relationship the project wants: users build audio.cpp themselves
   and may have several backend builds.
2. Model load is cheap enough that a per-session process needs no supervision:
   ~0.3 s to ready for the default model. There is no warm daemon to keep alive,
   restart or leak.
3. The line protocol exists precisely for this: when stdout is a pipe, the CLI
   emits `partial_text=` deltas, `event=` voice activity and a final `text_output=`
   plus `metrics.*`, so nothing is screen-scraped.

The C API is a per-file swap because the adapter's exports are narrow (Start,
Write, Events, Close), and it becomes worth doing when a model needs in-process
control, or when repairs must not pay a process start each.

**Rejected:** `audiocpp_server` with SSE. It works and keeps models warm, but it
adds a supervised process, a port and an HTTP dependency to buy something this
pipeline does not need at one session at a time.

## D5 — Audio capture is a subprocess, and the pipeline is 16 kHz mono end to end.

**Decision.** Capture runs `parec` (falling back to `pw-record`) and reads raw
s16le mono at 16 kHz from its stdout. No audio library, no cgo.

**Why.** PipeWire then does the resampling, follows the user's device routing, and
applies their echo cancellation and volume settings — all things a cgo binding to
ALSA would have to reimplement worse. The 16 kHz part is not a preference: the
models' bundled Silero VAD rejects anything else and aborts (measurement 1), so
the conversion has to happen before a model sees the audio, and the earliest
correct place is the capture client.

**Consequence.** `capture_rate` other than 16000 is a validation error rather than
a slow path. Dropped chunks are counted and reported, because the alternative
(unbounded buffering) would grow with session length; when a chunk is dropped the
rolling buffer restarts rather than splicing unrelated audio.

## D6 — Text delivery is an IBus engine.

**Decision.** voiceime registers itself as an IBus engine; uncommitted text is
shown with `UpdatePreeditText` and committed text with `CommitText`.

**Why.** On GNOME/Wayland there are only three ways to put text into the focused
application: be an input method, inject input events through `/dev/uinput`, or
paste. IBus is the framework this desktop already runs (`ibus-daemon` and
`ibus-engine-pinyin` are active; `QT_IM_MODULE=ibus` and `XMODIFIERS=@im=ibus`
are set), so being an engine needs no privileged setup, while `uinput` requires a
udev rule, a group membership and a helper daemon. The engine route also delivers
the "text appears as you speak" requirement for free: preedit is displayed at the
caret of the application itself, which is better than any floating window, and it
maps exactly onto the transcript model's provisional/committed split.

`DeleteSurroundingText` exists in the interface (confirmed by extracting the
interface definitions from `libibus`) so committed text *can* be rewritten where
an application supports it, but not every application does. The design therefore
does not depend on it: the point of committing only at an endpoint is that
committed text is final.

**Rejected:** `uinput`/ydotool typing. It covers every application, but needs root
setup, and direct Unicode typing requires keymap tricks that do not work reliably
for CJK. If it is ever wanted, the useful variant is clipboard plus an injected
Ctrl+V, which needs only ASCII from the input device — but that is a fallback for
applications that ignore IBus, not the primary path.

## D7 — Two layers of text, and repair may only rewrite the provisional one.

**Decision.** The transcript keeps committed and provisional text separately. The
streaming session produces provisional text; when the endpoint detector closes an
utterance, that segment is re-decoded offline and replaces the provisional text
produced for the same audio span.

**Why.** Window-based streaming decodes each window with no context, so its output
is damaged at every boundary (measurement: a spurious `。` and a split sentence on
a 5 s clip) and gets much worse as the window shrinks. Re-decoding the finished
segment with the offline path fixes exactly that damage, cheaply, if the pass keeps
up: 0.55 s for a 4.2 s segment with the streaming model.

**Consequence.** Repair defaults to the streaming model rather than the most
accurate installed one, because the accurate one needs 5.7 s for the same 4.2 s
segment — slower than the speaker. `doctor` reports the alternative as advice.
Repairs are conservative: an empty decode, or a range containing no provisional
text, is a no-op, so a decode that finishes late can neither duplicate nor delete
text the user has already seen.

## D8 — Endpointing lives in voiceime, not in the model.

**Decision.** A small energy-based detector with noise-floor adaptation decides
when an utterance starts and ends. The models' own voice activity output is
forwarded when present but never relied upon.

**Why.** The commitment boundary — the moment text stops being correctable — is a
product decision, and it has to exist even for models that report no voice
activity at all. SenseVoice's streaming session is one of those. An energy
detector also costs no memory, which matters when a repair model may hold a
gigabyte, and it is testable against synthetic audio, which is how its behaviour
is pinned down.

**Consequence.** Thresholds are configuration, and the detector raises its
threshold above a measured noise floor so a noisy room does not read as continuous
speech. Its defaults have not yet been tuned against a human talking into this
microphone; that is listed as unmeasured.

## D9 — Model discovery asks audio.cpp, and caches the answer.

**Decision.** Models are identified by running `audiocpp_cli --inspect` on each
candidate and parsing the report. Results are cached in
`~/.cache/voiceime/model_index.json`, keyed by file size and mtime.

**Why.** Guessing a model's family, tasks or modes from its file name would be
wrong the first time someone renames a file, and the answer must include whether a
model can stream. The cache exists because the original requirement was a
"refresh button" — the real problem was that scanning is slow, and one stat per
file makes it fast enough that the button is unnecessary, while
`voiceime models refresh` still forces a full re-inspection.

**Consequence.** The models directory may be shared with unrelated models (it is
on the reference machine: 13 of 15 GGUFs are text or vision models), so discovery
filters to ASR-capable models, reports how many files it ignored, and offers
`models list --all` for the rest.

## D10 — Downloads are delegated to audio.cpp's model manager.

**Decision.** `voiceime models install` runs `tools/model_manager_v2.py` from the
user's audio.cpp checkout, with `HF_ENDPOINT` set for the hf-mirror and
`--source modelscope` for ModelScope.

**Why.** That script owns the package registry, the pinned revision per package
and the mirror rules, and it is the code path upstream tests. A second
implementation here would drift from the registry silently, and drift in a
downloader is invisible until a user hits it. voiceime's contribution is policy:
which mirror, which packages to recommend, and machine-readable results.

**Consequence.** Mirror availability is per package, not global: SenseVoice's GGUF
is on HuggingFace only and fails on ModelScope with a 404, while the aggregate
repository is on both. voiceime reports the failing package and the message rather
than falling back silently, and `models sizes <pkg>` answers reachability without
downloading.
