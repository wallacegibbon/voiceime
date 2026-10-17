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

**Refinement (measured later).** The rule needs one more clause at the *stream*
boundary, not the repair boundary: a window whose audio starts inside text that
has already been delivered is dropped instead of kept. A window is decoded as one
unit, so its text is a reading of audio the application already has, and there is
no way to split that text at the delivered boundary. Reproduced on the wire: after
a segment committed at 4.50 s, the next window covered [3.14 s, 6.16 s] and its
text re-entered the transcript, so the sentence the user had already seen appeared
underlined again as preedit and was delivered a second time - the transcript read
`... we meet at a time when it.We meet at a time when it is.Is easy ...`. Whether
that window arrives before or after the commit is a race, so the rule is enforced
in the transcript rather than by ordering calls in the session, and the drop is
reported as `skipped: a window overlapping already-delivered text was dropped`
rather than being silent.

**Known gap in the same area.** A whole-transcript *snapshot* (which only
revision-capable models emit) describes delivered audio too and carries no sample
range to attribute it to, so it is not filtered. No family voiceime selects has
been observed to emit one; a snapshot-emitting family needs its hypothesis trimmed
to the provisional range before it can be trusted after a commit.

**Refinement (found in the field, after the rule above shipped).** "A provisional
chunk that overlaps the range is dropped, and the decode is trusted" quietly
assumed the decode *covers* the chunk. It does not: a streaming chunk is decoded
as one unit and can reach past the endpointer's range at either end, while the
repair only re-decoded the range. The chunk's text for the overhang was therefore
deleted and not replaced - and the overhang is precisely where an energy detector
is least certain, because it misses a quiet onset and ends on the last frame above
the threshold. The reported symptom was "only part of what I said was recorded",
with the missing part at the start or the end of the utterance.

The fix is a single rule, now enforced before the audio is even cut: the repair's
range is the **union** of the endpointer's range and every provisional chunk the
repair will replace (`Transcript.RepairSpan`), so the offline pass re-decodes
everything it removes. Two consequences fall out of it. No text is discarded
without being re-read, and the committed span ends exactly where the streaming
position ended, so the "a window that starts inside delivered text is dropped"
rule above stops firing in the normal case: it is kept as a guard, but it was
firing because the repair's declared end ran ahead of the stream.

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
speech. The reference machine's room has since been measured and the defaults
tuned for it; see D16 for what that measurement found and fixed.

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
user's audio.cpp checkout, with `HF_ENDPOINT` set to the configured endpoint.

**Why.** That script owns the package registry, the pinned revision per package
and the mirror rules, and it is the code path upstream tests. A second
implementation here would drift from the registry silently, and drift in a
downloader is invisible until a user hits it. voiceime's contribution is policy:
which endpoint, which packages to recommend, and machine-readable results.

**Consequence.** Endpoint availability is per package, not global: a package that
one endpoint does not carry is reported with its id and the error text rather than
falling back silently, and `models sizes <pkg>` answers reachability without
downloading.

## D11 — While the engine is active, voiceime dictates.

**Decision.** Activation starts the dictation session; deactivation stops it and
commits whatever was still provisional. There is no global hotkey in this version,
and no push-to-talk.

**Why.** Switching input methods is already the user's way of saying "this is how I
want to type right now", so the engine state *is* the switch. It costs no extra
privilege or daemon, and it cannot get out of sync with the input method the way a
separate toggle can. A hotkey (push-to-talk, or toggling dictation while another
engine stays selected) is a genuine convenience on top, not a prerequisite, and is
deferred rather than half-built.

**Consequence.** Audio is captured whenever this engine is selected, so the engine
process is the one place that must not leak a capture stream: the session opens a
fresh capture per activation and stops it on deactivation.

## D12 — The engine process hosts the pipeline.

**Decision.** `voiceime ibus-engine` runs capture, endpointing, streaming
recognition, repair and text delivery in one process. There is no separate
dictation daemon and no IPC between the two halves.

**Why.** The text has to reach a D-Bus object that only the engine process owns, so
splitting them would mean inventing a second protocol to say "here is a sentence,
now put it in the document" - and that protocol would need its own lifecycle,
supervision and failure modes. The daemon already supervises the engine process:
it spawns the `<exec>` when the engine is activated and the process exits when the
bus goes away. One process means one lifecycle, owned by the component XML.

**Consequence.** The session is a plain callable type (`daemon.Options.Run`) with
an injected sink, so `voiceime stream` drives the identical code with a printing
sink - which is why tuning per invocation and tuning what a user feels are the
same measurements. The cost is that the engine's logs have nowhere natural to go
(its stdout is discarded by the daemon), so it can write a trace file instead.

## D13 — The dictation trigger is a function key, never a modifier combination.

**Decision.** The default shortcut is `<F9>`: a tap starts dictation, a second tap
returns to the input method that was selected before. The key is configurable, but
a combination containing a modifier key should not be used.

**Why.** IBus's global-shortcut property consumes the registered key sequence,
including the release events. A modifier key is not "typed" - clients maintain a
modifier *state bit* from its KeyPress/KeyRelease pair - so a combination like
Ctrl+' lets the application see Ctrl go down (the grab only applies once the
combination is complete) and then swallows the Ctrl release. The application is
left believing Ctrl is still held, until the user taps Ctrl again. Measured on the
reference machine: with Ctrl+', Ctrl stayed latched after every tap, *even when the
engine switch was delayed 300ms past the release* to let the modifiers come up. A
plain function key cannot fail this way: the client sees neither the press nor the
release, so there is no state to leave latched.

**Consequence.** Function keys are not free either - applications claim them by
convention (F1 help, F5 refresh, F11 fullscreen, F12 devtools) - and the listener
consumes the chosen key globally, so an application that expects F9 will not see
it. No desktop-level binding on the reference machine uses F1-F12, so the choice is
about application habits; `voiceime hotkey status` reports the parsed keyval and
modifier mask so a typo is visible, and the key can be changed with one config
setting.

**Rejected.** Push-to-talk (hold to dictate, release to return): the mechanism
supports it - the daemon reports both edges - but holding the key produced ~200
repeat presses per second-long hold, and the switch it triggered mid-combination is
what latched the modifier. A tap is also easier to hit than a hold for a single
utterance.

## D14 — Releasing the shortcut cannot be a deregistration.

**Decision.** When the hotkey listener stops it registers an inert combination
(Ctrl+Alt+Shift+Super+F24) in place of the user's key, and `hotkey uninstall` runs
`ibus restart`.

**Why.** IBus offers no way to withdraw a registration: the property *is* the
registration, and an empty key list makes the daemon abort - libibus asserts
`size > 0`, the process core-dumps, systemd restarts it, and every engine process
it had spawned is killed with it. This was not a hypothesis: it happened in the
reference session, repeatedly, and the daemon log named the assertion. Leaving the
registration untouched was the alternative, and it is worse: the user's key stays
swallowed by a daemon that no longer has a listener, so F9 would simply stop
working in every application with no explanation.

**Consequence.** Stopping the listener releases the user's key, at the cost of the
daemon still holding a combination nobody can press. Only a daemon restart clears
the slot, which is why uninstall does it and why the listener says what it did.

## D15 — One configurable download endpoint, and no preset names.

**Decision.** `download_mirror` holds an absolute `http(s)` URL, passed to the
model manager as `HF_ENDPOINT`. The former preset values (`hf-mirror`,
`huggingface`, `modelscope`) and the `--source modelscope` override are removed.

**Why.** Three reasons, in order of weight.

1. *ModelScope was never load-bearing here.* All 373 packages in audio.cpp's
   registry declare `download.kind = huggingface_snapshot`; not one declares
   `modelscope_snapshot`. `--source modelscope` therefore never made an
   unreachable package reachable - it re-hosted the same Hugging Face spec on
   another server, and it does not work for the package voiceime actually
   recommends: `FunAudioLLM/SenseVoiceSmall-GGUF-audiocpp` does not exist on
   ModelScope ("record not found"), while the aggregate repository
   `audio-cpp/audio.cpp-gguf` does. The script keeps its ModelScope transport for
   the day a spec declares it; what was removed is an override no package needed.
2. *Preset names were the only thing making this an enum.* Two of the three values
   were nothing but two URLs, so the setting may as well hold a URL - which is
   strictly more capable, lets a caller point at a company cache or a LAN address,
   and makes `https://huggingface.co` a value rather than a special case.
3. *Fewer hosts, less drift.* The endpoint is part of an install's identity: the
   package manifest records source-specific etags, and the script's own help warns
   that cross-source checks may report updates. One endpoint per machine keeps
   `models installed` honest.

**Also decided, and why.** Old preset values are *not* aliased, and the key was
*not* renamed. An alias means one key with two spellings plus a table of magic
strings to maintain, test and document - the ambiguity this decision exists to
remove. Renaming the key was rejected for a measured reason: `config.Load` rejects
unknown keys before any command runs, so a renamed key makes *every* command fail,
including the `config set` that would repair it, leaving hand-editing as the only
way out. A rejected *value* keeps the file loadable, so `doctor` reports it with a
fix and `config set` applies that fix. Both properties are pinned by tests in
`internal/config/config_test.go`.

**Consequence.** The value is validated syntactically (`http` or `https`, a host,
no query string or fragment) and never resolved over the network, so `doctor` and
`config validate` keep working offline; reachability remains a question for
`models sizes`. `HF_ENDPOINT` is now always set explicitly rather than only for
the mirror, so an ambient value cannot silently decide where downloads come from.
The default stays `https://hf-mirror.com` because it is the endpoint measured to
work from a network where huggingface.co is unreachable.

## D16 — The detector must learn from the stream, and an utterance must be earned.

**Decision.** Four rules, all of them consequences of one measurement session in
the reference machine's own room (`docs/measurements.md` has the numbers):

1. The noise floor is learned from the audio stream itself. `Calibrate()` is not a
   prerequisite for adaptation.
2. A noise burst shorter than `minInterruptMS` (120 ms) does not interrupt the
   silence hangover and does not extend an utterance.
3. `Flush()` applies the same minimum-speech rule as a normal boundary.
4. A session that produced no *segment* commits nothing when it stops.

**Why.** Each rule was a live bug, not a precaution.

1. `session.go` had passed `Adaptive: true` since the detector was written, but
   `Threshold()` used the measured floor only after `Calibrate()`, and no code
   path in the pipeline calls it - a test called it, so the tests passed while the
   feature was dead. The detector ran on the static `silence_rms_threshold` of
   350, which in the reference room sits *inside* the noise distribution
   (idle p50 = 309, p90 = 436).
2. Any single frame above the threshold reset the hangover counter, and that room
   is spiky rather than merely loud: 1% of idle frames reach RMS 7992 (keystrokes,
   chair, fan) against a peak of 23873. One click per second was enough to keep
   the detector "speaking" indefinitely, which is how 20 s of an empty room
   produced 4.28 s, 9.90 s and 1.30 s utterances and committed `被。` and `嗯。`.
3. The last frames of a session are as likely to hold a keystroke as a word: a
   0.02 s "segment" was observed at the tail of a recording, and repairing it
   delivered the recogniser's hallucination from a click.
4. Streaming ASR produces plausible text for almost any input. Even with the
   endpointer silent, the *streaming* session hallucinates partials out of room
   noise, and the final commit used to deliver them: 20 s of an empty room
   committed `被。。。。。。嗯。`. Text the endpointer never heard is not text the
   user said.

**Consequence.** "It never commits" and "it never ends" are the same failure, and
`docs/measurements.md` now records how to measure a room in one command so the
threshold can be set from data rather than by guessing. A session with no detected
speech now reports `skipped: no speech detected; ...` (with the number of
characters it withheld, so the loss is at least visible) instead of typing
nonsense, so a threshold set too high shows up as silence rather than as
wrong words - the honest failure. The residual known gap is text that arrives
before the *first* boundary in a genuinely noisy room: it still belongs to a real
segment's commit. Removing it needs per-sample speech regions in the transcript, a
larger change than this fix.

**Refinement (found in the field, after the rules above shipped).** The four rules
above were necessary and not sufficient: the same reported symptom came back for a
speaker who starts talking the instant dictation is activated, with no quiet frame
first. Two more clauses were needed, and both are the same mistake seen from two
sides - a *level* was being treated as a *measurement*.

1. The adaptive threshold is not in force until a quiet frame has actually been
   measured. `NewSegmenter` seeded the floor with `Threshold`, and `Threshold()`
   returns twice the floor when that exceeds the configured value, so the detector
   opened at **2x** the configured threshold before it had measured anything at
   all - the seeding comment claimed it behaved like the plain threshold detector,
   and it did the opposite. A speaker between those two numbers is below the
   threshold in force, so their speech is never speech.
2. A frame at or above the configured threshold is candidate speech and is never
   adopted as the noise floor. This is what turned (1) into a *self-ratcheting*
   failure rather than a one-frame glitch: speech below the inflated threshold was
   learned as the floor, which doubled the threshold again, and the detector stayed
   deaf for as long as the user kept talking. When they stopped, the floor fell
   back to the room and the next utterance worked - hence "sometimes".

**Why the seed was wrong in a way a test could not see.** Both clauses are silent
in the other direction. With a quiet frame at the start - any room tone below the
configured value, which includes every test fixture that begins with silence and
every real session where the user pauses after activating - the floor drops to that
frame immediately and the threshold is correct. The failure only exists when the
very first frames are speech, which no unit test written against synthetic audio
had: `TestSegmenterLearnsTheNoiseFloorFromTheStream` begins with room tone, which
is exactly the case that hides it. There is now a test that begins with speech.

**Consequence.** The configured value is what a stream with no measurement uses, so
it is again the *plain threshold detector* until the room has been heard, and a
room whose own floor is above the configured value is no longer learned upward.
That is deliberate: `silence_rms_threshold` is specified as a floor above the
room's idle noise (step 6 of `docs/DEPLOY.md` measures it), and a room above it is
a misconfiguration that the same measurement step reports, whereas a speaker the
detector cannot hear is indistinguishable from a broken microphone. A room that is
noisy but *correctly configured* still adapts upward as before - the threshold is
twice the measured floor, and the measured floor is a real quiet frame.

**Rule 4 was kept, and stopped being silent.** The symptom above is also what rule
4 looks like from the user's side - the text is visible as preedit, the session ends,
and nothing appears - so removing the rule is the tempting fix. It is the wrong one.
With the endpointer fixed, the only way to reach that state is audio the detector
judged *not to be speech*, and rule 4 exists because committing that audio types the
recogniser's guess: twenty seconds of an empty room committed `被。。。。。。嗯。`,
and a single click committed `。`. Whether the text is a real short phrase or a
transient of the same length cannot be told apart from one run of samples - the
reference room has a 340 ms transient, which is the length of a short word - so
weakening the rule trades a visible loss for an invisible corruption. What was
genuinely wrong was the silence, since the text *had* been shown at the user's
caret: the session now reports `withheld N characters`, which distinguishes "the
detector never heard me" from "the session was lost" and sizes what the room
measurement got wrong. `docs/AGENT.md` had promised the opposite ("a short phrase
is then committed when dictation stops"); that sentence was wrong and is corrected.

## D17 — A test is not a caller, and installed artifacts are checked.

**Decision.** Three rules, adopted after a session in which four separate bugs —
all of them invisible to the test suite — each cost real debugging time.

1. **Code that only tests call does not live in a shipped package.** `make check`
   runs `deadcode` over `cmd/` and `internal/` and fails on anything `main` cannot
   reach.
2. **Test fixtures belong to tests.** Where two packages need the same one, each
   declares its own rather than exporting it from the production package.
3. **`doctor` checks the installed artifacts**, not just the prerequisites:
   whether the component file's `<exec>` and the systemd unit's `ExecStart` still
   name an existing binary, and whether the running processes came from the binary
   that is running `doctor`.
4. **`make install` defaults to `$(HOME)/.local`**, because those artifacts record
   an absolute path and a per-user prefix needs no root.

**Why.** The bugs, in the order they were found:

* `Segmenter.Calibrate` was the only caller of the adaptive threshold, and nothing
  in the pipeline called it: the tests passed while the feature was dead code, and
  the visible symptom was that dictation never committed anything.
* `cli.containsLine` was called only by its own test. Production used it anyway
  until it was replaced, and it could never match the real `ibus list-engine`
  format — so `doctor` reported a correctly registered engine as missing.
* The component file and the systemd unit both named a binary from a checkout that
  had been renamed. The engine appeared to work from the already-running process
  while the listener looped on `status=203/EXEC`; nothing reported either.
* After every rebuild the running engine kept the old binary, so fixed code looked
  broken. Same class, different cause.

The common thread is that none of these are reachable from a unit test, because
they are properties of *how the program is installed and started* rather than of
what it computes.

**Consequence.** The deadcode check needs a pinned tool, and the newest release of
that tool requires a newer Go than this project does, so the Makefile installs
`golang.org/x/tools/cmd/deadcode@v0.36.0` into a throwaway GOBIN instead of adding
it to `go.mod` — a check should not raise the project's own toolchain floor.

The install checks are offline and cheap, and they now catch the two failures that
used to be guesswork: `component_exec` / `hotkey_unit` fail when a recorded path is
gone, and `engine_process` / `hotkey_process` warn when a running process is not
the binary running `doctor`. That last one is also the honest way to run the
development loop: `doctor` from the checkout against a deployment in
`~/.local/bin` reports the difference instead of pretending they are the same.

## D18 — Machine-specific values are discovered, reported or advised, never guessed silently.

**Decision.** Three mechanisms, each for a different kind of value.

1. **Discovered** when it can be derived: the audio.cpp checkout (`~/audio.cpp`,
   `~/src/audio.cpp` and similar, plus the ancestor of any named `audiocpp_cli`),
   the ASR binary, the model manager script, the default capture source, and the
   model itself (by inspecting what is installed and ranking it).
2. **Reported** at `config init`, which writes the discovery result into the file
   and prints a `review` list of the settings that are the development machine's
   rather than voiceime's: `audio_cpp_dir`, `threads`, `backend`, `language`,
   `silence_rms_threshold`, `download_mirror`.
3. **Advised** by `doctor`, for the two the machine can be asked about: `threads`
   warns when it exceeds the physical core count, `backend` fails when the local
   binary was not built with it and warns when a GPU driver is present while the
   setting says `cpu`. Both carry a runnable fix.

**Why.** Every value in `Default()` is a measurement or a path from the machine
voiceime was developed in (`~/playground/audio.cpp`, `threads = 4` on an 8-core CPU,
`language = zh`, `silence_rms_threshold` from one room, an endpoint chosen because
huggingface.co is unreachable from one network). Most of them are fine anywhere -
the capture rate is a hard constraint and the window policy is model behaviour - but
before this, an agent deploying on another machine had no way to tell which ones
were portable and which were the reference machine's.

**Why not auto-tune.** The obvious alternative - pick `threads` from the core count
and `backend` from the hardware - was rejected because both would be guesses
dressed as configuration. No GPU backend has been measured for this project, and the
split of four threads between streaming and repair has not been studied
(docs/measurements.md, "Not measured yet"), so writing `backend = vulkan` on a
machine with a render node would be a claim nobody has tested. A wrong guess also
fails in the direction that is hard to notice: the machine keeps working, just
slower or with a repair that lags. Discovery is restricted to the same principle -
it only ever fills in a path, and only where a checkout must be for voiceime to work
at all, and it reports what it found.

**Consequence.** A machine whose `audio_cpp_dir` is wrong is no longer a dead end:
the binary and the model manager are also looked for on `PATH` and in conventional
locations, and `doctor` reports the path it actually used. The review list is
deliberately short and bounded - six keys, each with a reason - because a list of
everything is a list nobody reads. Values that need a measurement stay in the
documented procedure rather than in a default: `silence_rms_threshold` is the
working example, and `docs/DEPLOY.md` step 6 is the procedure.

## D19 — A held key is one press on the command path too, not only in the listener.

**Decision.** `voiceime hotkey toggle` ignores an invocation that arrives within
`repeatGuard` (700 ms) of the press it last acted on, refreshing the marker so a
continuous hold stays a single press until it ends. State is one file in the cache
directory; its mtime is the clock.

**Why.** On X11 the trigger is a GNOME custom keybinding, and `gsd-media-keys` runs
that command once per key event - including *every auto-repeat tick of a held key*.
The listener guards a hold with its in-process `held` flag (D13), but the command
path has no process to hold state, so the guard had to move to the file system.
Measured on the reference machine: holding the key fired the command 52 times in two
seconds, and a 1.0 s hold 19 times (`xset`: delay 500 ms, rate 33/s). An even count
returns the engine to where it started, which is exactly what "F9 stopped working"
looks like from the outside - the key is delivered, the code runs, and the net
effect is nothing.

**Why the window exceeds the repeat delay.** A held key emits its *first* repeat
only after the auto-repeat delay, so any window shorter than that delay lets the
first repeat through, and that one repeat is enough to toggle straight back. The
delay is 500 ms here and 660 ms by xset default, so the window sits above both.

**Consequence.** Two deliberate presses closer together than the window collapse
into one. For a tap-to-toggle key that is normally seconds apart this is not a loss,
and it is the deliberate half of the trade: err large and a slow double-tap is
swallowed, err small and a held key toggles its way to a random parity. The window is
a constant, not a setting, because the value that matters is the X server's repeat
delay and nothing here can read it portably; a machine with a much larger delay would
need it to become one.

**Rejected.** Acting only on the key *release* - the keybinding path produces press
events, not a release to hook. Detecting the burst by looking ahead instead of by a
window - a held key's first repeat arrives a full repeat-delay after the press, which
makes it indistinguishable from a deliberate second tap without the window.

## D20 — Every accepted utterance is decoded offline; the streaming layer only drafts.

**Decision.** When the endpointer closes an utterance, the offline pass decodes the
audio for *that utterance*, and what it produces is what gets delivered. Streaming
text is a draft: it is never required for a commit, and whether a window happens to
cover the utterance changes nothing about the delivered text.

The range to decode depends on what the streaming layer covered:

* **A window covers the utterance.** Decode the union of the utterance's range and
  every chunk the decode will replace (`Transcript.RepairSpan`): the decode removes
  that chunk whole, so it has to re-read everything it removes.
* **Nothing covers it.** Decode the utterance itself, widened on each side by
  `endpoint_silence_ms / 2`.

**Why.** Two clocks run against each other. The endpointer closes an utterance about
`endpoint_silence_ms` after the speech ends; the recogniser emits a partial only
every `chunk_seconds`. A short utterance therefore closes before any text covers it,
and the old code read that as "the recogniser produced nothing, so there is nothing
to correct" and committed empty. The commit fires only when a segment closes or the
session ends, so the text then arrived with no boundary left to release it. Reported
as the text sitting in preedit forever; measured here as a 1.2 s utterance followed
by silence committing at 11.20 s - the end of the audio - eight seconds after the
text appeared. Holding the segment until a covering window arrived was built and
measured first; it bounds the wait, but it keeps the commit dependent on the
streaming layer and merges adjacent short utterances through the union.

Making the decode unconditional removes that dependency rather than bounding it, and
it is what the two layers were already for (D7): the streaming pass shows text while
the user speaks, it does not decide what is final.

**Why the pad is what it is.** Measured on the reference clip, decoding only the
endpointer's own range loses a quiet tail - 0.06 gain on the last half second and the
decode stops at `Some call me。` instead of `Some call me nature.` - and 300 ms of
widening recovers it. It cannot reach the next utterance: the pause before one is at
least the whole hangover, so half of it stops halfway. Widening was also measured
harmless where the range was already adequate: clean speech decodes identically at
every pad from 0 to 0.6 s.

**Consequence.** A commit no longer waits for a window. Measured on a three-sentence
clip: each sentence committed 0.43-0.49 s after it closed, against up to a window for
the held version, and the sentences stopped merging - four commits where the union
produced three. `chunk_seconds` stops gating when text becomes final, so it now
trades only draft smoothness and streaming accuracy. The cost: a window straddling a
commit is dropped, so a rapid short sentence has no draft to watch - it is committed
instead, about 0.44 s after it ends. Longer utterances are unaffected; their
in-progress draft is decoded before any commit and was identical between versions.

Two things are deliberately left alone. The end-of-session rule is unchanged, so a
hallucination in the tail is still committed when the session stops; retiring it
needs the per-sample accepted-speech regions this is the first half of, and is a
separate change. An empty decode is still a no-op (D7): removing hallucinations needs
a confidence signal this path does not have.

**Rejected.** Decoding without the pad - a quiet tail is lost, the failure D7 already
recorded. Always decoding the utterance's own range, ignoring the union - a decode
that replaces a chunk without re-reading the whole chunk deletes its text for the
overhang, D7's other recorded failure. Delaying the commit until a covering window
arrives - bounds the wait instead of removing the dependency, and still merges
adjacent utterances.

## D21 — A model that cannot keep up as a process per utterance is served resident, on demand.

**Decision.** The offline decode can run through a resident `audiocpp_server`
instead of a process per utterance, and voiceime starts that server itself when a
dictation session begins and stops it when the session ends. It is configured with
`asr_server_url`, off by default (`refine_on_segment_end` already covers the case
where a small refine model is enough).

**Why.** The repair pass re-decodes *every* accepted utterance (D20), so the cost
that matters is per utterance, and on the CLI route it is not the decode - it is
the model load, paid again each time. Measured here with Qwen3-ASR-0.6B, a 1.15 GiB
checkpoint: 4.87 s for a 0.5 s clip and 5.79 s for a 4.24 s one, and the CLI's own
report accounts for 463 ms of that in session time, so ~4.4 s is work before the
session starts. It is not I/O: the whole file reads out of page cache in 0.09 s.
Resident, the same decode costs 0.34 s and 1.05 s respectively.

Through the real engine, commit latency measured from the endpointer closing a
segment to its text landing, Qwen3-ASR-0.6B as the refine model both ways:

| | sentence 1 | sentence 2 | sentence 3 |
| --- | --- | --- | --- |
| process per utterance | 5.13 s | 8.45 s | 11.67 s |
| resident server | 0.56 s | 0.60 s | 0.59 s |

The CLI column accumulates because each repair queues behind the last, so the third
sentence arrives 11.7 s after it was spoken. The server column is flat, which is the
point: the repair keeps up with speech, so choosing a more accurate refine model
becomes a quality decision rather than a latency one. This is what makes the advice
in D18's spirit - "slower than real time on CPU" - a property of the route rather
than of the model.

**Lifetime.** On demand, not resident: spawned as a session starts, stopped after
the last repair lands. Kept warm between sentences, which is what the resident part
buys, and released between dictations, which is what an always-on server would not:
1.9 GB with both ASR entries loaded here. The stop runs *after* the repairs rather
than when the session's context ends, because the last sentences of a session are
decoded while the session is already shutting down.

**Why it is not a second entry for the streaming model.** The streaming layer stays
on the CLI's stdin route, so a server started here holds one offline entry. That
matters because the server holds a model for the whole length of a request: a repair
sent to an id a live dictation owns waits for that dictation to end. Measured, a
215 ms repair waited 9.7 s behind a 12 s session. Two entries with the same weights
would be two resident copies (+253 MB for SenseVoice Q8, measured), which is a price
worth paying only for a deployment that also moves streaming into the server.

**Consequence.** `doctor` still reports a refine family that cannot keep up on the
CLI route, but stops warning about lag when a server is configured, since the cost it
measures is the one the server removes. The decode contract has to be identical on
both routes or the comparison is meaningless: `asr.RepairRequestOptions` is now the
single place that decides them, because a multipart transcription upload has no field
for request options and the server route carries them in the entry's
`default_request_options` instead.

**Rejected.** Keeping the server resident across dictations - it holds a gigabyte for
a machine that is idle most of the day, and the on-demand start is overlapped with
the user's first sentence anyway (~0.8 s of a 2.4 s repair against 0.6 s for a warm
one). Spawning on the first repair rather than at session start - it puts the whole
model load on the first sentence's commit. Serving the streaming role from the same
server - it needs a second entry and therefore a second resident copy of the
streaming weights, with no gain while the CLI route already streams. Falling back
silently when the server cannot start - a user who configured it needs to know, so
the failure is reported once per session and the CLI route carries the utterance.

## D22 — One resident service serves both recognition roles. (Supersedes D21.)

**Decision.** Both passes run against a single `audiocpp_server` that voiceime installs
as a systemd user unit (`voiceime server install`) with `lazy_load: false`, so its two
models are resident from startup. A dictation session opens a streaming connection and
posts one request per finished utterance; nothing is spawned and nothing is loaded.

**Why the earlier shape was wrong.** D21 made the server an on-demand process for the
repair pass only, and left the streaming pass as `audiocpp_cli --mode streaming` - a
fresh process per dictation. That is internally inconsistent in the way a user notices
immediately: the whole argument for the server was "load the weights once", and the
*first* thing every session did was load weights. It also put the load on the critical
path whenever the first utterance was short (measured: 1.20-1.49s to commit a 0.5s
sentence that closed before the model was ready, against 0.33s warm), which is why D21's
text needed a paragraph of caveats about overlapping the load with speech.

**Why one service can serve both.** The two roles are different *uses* of the model, not
different models: streaming decodes fixed windows and emits a draft, the offline pass
re-reads a finished utterance with its full context and decides what is final. That
distinction stays (D20). What changed is where they run. They cannot share one *entry*,
because the server holds a model for the whole length of a request: measured, a 215ms
repair sent to the id a live session owned waited 10.9s for that dictation to end. So
the config has two entries and the client uses two ids - and audio.cpp needs no
concurrency at all beyond that, because a dictation is one live request and the repair
pass is one request at a time by design.

**Why not `--parallel-jobs`.** It exists, but its own contract calls it experimental
with incomplete coverage, and "a model without a parallel factory therefore supports one
slot" - most real checkpoints get no extra slot. Nothing here needs one.

**Measured on the reference machine** (SenseVoice streaming + Qwen3-ASR-0.6B repair,
models resident: 1403 MB RSS at startup, 1907 MB once both have run):

| | on-demand (D21) | one service (D22) |
| --- | --- | --- |
| session start | spawn + 325ms model load | **144µs** (connect only) |
| first repair of a session | 0.71-0.86s (waits for the spawn) | 0.84s for a 2.38s span, 0.54s for 1.68s |
| second dictation | same again | same, with nothing loaded |
| CPU at idle | none | none (the service sleeps) |

The streaming side is not a downgrade: the server's live endpoint emits deltas *while*
audio is arriving - measured, the first at 2.01s into a 12s clip, then about one a
second - so the draft appears during speech exactly as the CLI's did.

**Consequence.** `asr_server_url` is no longer optional: with no process to fall back to,
`doctor` fails and says `voiceime server install`, and `stream` refuses to start. That is
the trade for deleting the fallback - one dependency, one failure mode, one message -
and it is why the unit is installed by the same command shape as the shortcut listener.
`chunk_seconds` and the model paths are read by the service at startup rather than per
request, which initially made changing one a three-step ritual (`config set`, then
`server install` to rewrite a generated file, then hope the service was restarted - it was
not, because `systemctl enable --now` does nothing to a running unit). The unit now runs
`voiceime server run`, which renders the server config from the settings before exec'ing
audio.cpp, so the settings are the only source of truth: `config set` names
`voiceime server restart` as the next step, and that is all it takes. `doctor` compares the
paths being served with the paths configured, so a service left on the previous checkpoint
is reported by name instead of showing up as slightly different text.

**Rejected.** Keeping the CLI for streaming (D21) - it pays a model load per session and
keeps two transport implementations alive. Two entries for one model id - the live
session would block repairs, measured above. Putting the server behind an on-demand
lifecycle - the complexity D21 accumulated (spawn, health wait, warm-up, process
ownership, orphan cleanup, a bounded wait on the repair path) existed only to avoid
holding a process, and it did not even achieve that for streaming.
