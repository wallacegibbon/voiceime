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
file makes it fast enough that the button is unnecessary. (The command that writes the
cache is `voiceime models list --refresh`; see D35.)

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

## D23 — Delete what stopped paying for itself; the installation is the product.

**Decision.** The design is settled: a user configures which models are served and can
start and stop the service, and that is the whole surface. Anything that exists for another
reason is a candidate for deletion, and the test is not "is it useful" but "does it still
have a reason to exist *here*".

**What this removed, and why each had stopped earning its place:**

* `models.RepairIsRealtime`, `models.SuggestRepair`, the family table behind them,
  `doctor`'s `refine_model_advice`, and the cost sentence on the `refine_model` check. They
  measured the 5.7s a *process per utterance* paid to load 1.07 GiB and advised against the
  accurate model. With the service holding the weights, that trade is gone, so the advice
  argued against the reason the service exists. Advice that is merely stale is clutter;
  advice pointing the wrong way is a defect.
* `voiceime transcribe`. It decoded one file through the repair entry; `voiceime stream
  --file` runs the whole pipeline on the same file and prints more. Two commands for one
  job is one too many.
* `stream --chunk` and `stream --threads`. Both reached only a per-run copy of the
  configuration, and the values they set are read when the *service* starts - so they
  changed nothing. A flag that silently does nothing is worse than a missing feature: it
  invites a measurement that describes a pipeline nobody runs.
* `session_options`. Nothing read it once voiceime stopped being a CLI caller.
  `request_options` stays: it reaches the generated server config.
* `capture_rate`. It could only ever be 16000 - the models' VAD rejects everything else, and
  validation refused the rest - so it was a setting whose only power was to break a working
  installation. The rate is a constant now.
* `models search`, `sizes`, `installed`, `recommended`, and the `refresh` action. A
  deployment installs a named package from this document and lists what is there;
  everything else was a wrapper around the model manager's own verbs, and each one cost a
  command name, a JSON shape, and a help line. `list --refresh` keeps the one capability
  that was not otherwise reachable (forcing a re-inspection).
* `voiceime devices`. It listed capture sources for a setting `doctor` already explains
  in place - and `doctor` reports the source it would use beside the check that proves
  capture works, which is the question a user actually has.
* `scripts/asr-server-prototype/`. The probes that measured the on-demand design (D21),
  which no longer exists. Their numbers are quoted here and in measurements.md; keeping the
  scripts meant shipping a measurement rig for a deleted architecture.

**Why this is a stability argument, not tidiness.** This is a foundational component: it
runs in the background, it types into whatever is focused, and its failure is silent. Every
path that cannot be exercised is a path that will break unnoticed. A flag that does nothing,
a setting that can only be wrong, and a command that duplicates another are all surface with
no benefit and a maintenance cost - and each one had already produced a bug in this codebase
(in D22: four names for two models, an install that reported success without restarting, and
a config that could not say which model was running).

**What was deliberately kept, and the bar for keeping it.** The two-pass design (D20) -
deleting it removes live drafts, which is the feature. Two server entries - the server holds
a model per request; measured 10.9s of waiting otherwise. `asr_server_timeout_ms` - a safety
valve. `refine_on_segment_end` and `--refine` - the only switch that trades accuracy for
speed, and the tests use it to pin both behaviours. `voiceime stream` - the only view into a
session that does not need IBus, and what `make measure` runs.

## D24 — A failed stream is a failure, not a short transcript.

**Decision.** An SSE `error` event, a stall past the server's idle timeout, and a
connection that ends without the terminating chunk each fail the session: `Stream.Close`
returns the error, and the session withholds the draft that no offline decode reached
instead of committing it.

**Why.** audio.cpp refuses to answer `transcript.text.done` for a body that was not
terminated properly, precisely so that a truncated transcript cannot be mistaken for the
speaker stopping (`app/server/README.md`). The client then lost that signal twice: it
read the error's reason from the top level, while the server nests it under
`error.message`, and nothing in the program read `Result.ProcessErr` at all - only a test
did. A session that lost its server therefore ended like any other: `voiceime stream`
exited 0, and whatever was still provisional was committed as though the user had
finished the sentence.

**Consequence.** `stream` exits 1 carrying the server's own reason; the engine logs
`session ended: …` in its trace; withheld text is reported with its size, like the
no-speech case, so "the model died" and "the room was too quiet" stay distinguishable.
Utterances that closed before the failure are untouched - their offline decode was
committed already - so what is withheld is exactly the text the failure stranded. A
server that reports an error in a shape the client does not recognise now has its payload
quoted rather than replaced by a sentence that says nothing.

Both halves are pinned against a stand-in that sends the server's real shape:
`TestStreamCarriesTheServersErrorReason` and
`TestSessionReportsAStreamFailureAndWithholdsTheDraft`.

**Rejected.** Committing the draft anyway because the user watched it appear - preedit
visibility is not evidence that the decode is complete, and a truncated tail is the one
case the protocol explicitly refuses to dress up as a finished transcript. Keeping the
generic message and logging the server's reason only - the CLI's failure is read by a
person and by an agent, and both need the cause in the message. Falling back to the
`done` event's text when the stream fails - no `done` event is sent in that case, which
is the point.

## D25 — The decode outlives the session; audio handed to it is a copy.

**Decision.** A repair decodes on a context derived from the session's with
`context.WithoutCancel`, and the wait for queued repairs is bounded by
`repairDrainBudget` (5s). `RollingBuffer.Slice` returns a copy. Both are two-line
changes and both exist because the alternative produced wrong text silently.

**Why the context.** Stopping dictation is itself an endpoint: `Flush` closes the
utterance in progress and queues it for repair *after* `Run`'s context is cancelled,
which is exactly when a hotkey release, an engine switch or Ctrl-C happens. A decode
that inherited the cancellation therefore never left the process - measured on the
release path, zero repair requests for the sentence just spoken, so `Place` never ran
and the worker committed the streaming draft. Every session ended on the path D20
exists to prevent, and nothing said so: text appeared, it was simply the damaged
version. `TestSessionStopsCleanlyWhenCancelled` had a 1.5s timeout on a 1.3s clip, so
it never cancelled and could not see it.

**Why the bound.** The decode no longer ends when the session does, so something else
has to. The transcriber's own timeout is `asr_server_timeout_ms` (30s), and the
engine's `Disable` is a D-Bus call ibus-daemon is waiting on; a stop must not last
that long on a server that stopped answering. Repairs measure 0.5-0.8s, so 5s is an
order of magnitude of headroom, and a stop that gives up says so - the sentence was
delivered from the draft - instead of looking like a clean end.

**Why the copy.** `Slice` used to return a view of the buffer the capture loop keeps
filling. When a block is dropped the gap path restarts at offset zero of the same
array, so every byte of audio already handed to a queued repair was replaced -
measured, 32000 of 32000 bytes - and the next block was decoded and committed as the
corrected sentence. The failure needs a drop, so it is a loaded machine's failure, and
it is silent: the log line says audio was dropped, not that the repair read the wrong
audio. One copy per utterance is nothing. `Append` stopped reallocating when the window
is full in the same change (2 MiB per 20ms block, measured, now none), because a
copy-on-slice design that also copies the whole window 50 times a second would be a
strange trade.

**Consequence.** A session that ends early - stream open failure, capture start
failure, stop - now goes through one `finishRepairs`, so a failed activation no longer
leaves the two repair workers running (measured: 45 goroutines after 20 failed
activations, against a baseline of 5). A decode that overruns the budget is cancelled
by the deferred `stopDecoding`, so the process does not accumulate in-flight work
across activations.

**Rejected.** Inheriting the session context and treating a cancelled decode as a
failure - that is the bug, not the design. An unbounded wait - it moves the failure to
the input method, where a hung `Disable` is worse than a sentence delivered from the
draft. Returning the slice without a copy and documenting the aliasing - the repair
worker is asynchronous, the buffer is refilled by another goroutine, and a comment is
not a lifetime. A condition variable or pool for the audio buffers - a copy is 64 KiB
once per utterance.

Pinned by `TestSessionStopsCleanlyWhenCancelled`, `TestFinishRepairsGivesUpAfterItsBudget`,
`TestFailedActivationLeavesNoRepairWorkers`, `TestSliceDoesNotAliasTheLiveBuffer` and
`TestRollingBufferAppendIsAllocationFreeWhenFull`.

## D26 — A written value is not a setting in force, and a write is atomic.

**Decision.** `config set` writes the value, then fails (`exit 3`) if the file it wrote is
unusable. It names `voiceime config apply` as the next step for *every* key. Every installed
systemd unit names the configuration file it was installed for, and the shortcut unit names
a key only when the caller asked for one. Files a command owns are replaced atomically, with
a generated temporary name.

**Why the exit code.** `config set threads 0` wrote the file, printed a finding to stderr
and exited 0. The human path was fine; the machine path was not, and this program's whole
surface is built for the machine path - `doctor` names a fix, an agent runs it and asks
again, and an exit code of 0 means "done". The value is still written, because staging a
setting before the thing it names exists is how a deployment is scripted (`refine_model`
before the download finishes, `asr_server_url` before the service is running); refusing to
write would have replaced one broken workflow with another. What changed is that the
failure is now visible where callers look, with the fix attached.

**Why every key names the same command.** The alternative was a list of "settings the
service reads at startup" beside `Config`, and it had gone stale in the way a second schema
always does: it named `session_options` and two `asr_server_*` keys that no longer existed,
and called `language` a service setting when it is sent per request - while `hotkey`,
`endpoint_silence_ms` and everything else the engine reads were reported as needing
nothing. Each resident process reads the configuration at its own start, so the honest
answer is the same for every key, and a uniform answer is one an agent can act on without
branching. `request_options` is settable as `key=value` for the same reason: it was the one
setting `config set` refused, which meant the one setting an agent had to write TOML by
hand for.

**Why the units name the file, and the key only on request.** A unit has no working
directory, so `server install --config <file>` used to install a service that served the
*default* file - the settings were written, the service read others. And the shortcut unit
baked the configured key into `ExecStart=… hotkey run --keys <F9>`; `hotkey run` prefers an
explicit `--keys` over the file, so `config set hotkey` followed by `config apply` (whose
documented job is "make the running system match the file") restarted the listener straight
back onto the old key. Named only when the caller passed `--keys`, the configuration decides.

**Why atomic writes.** The temporary name was `path + ".tmp"`, shared by every writer. Two
concurrent `config set` runs - two agents, or a `doctor` beside a `config set` - failed with
`rename … .tmp: no such file or directory` in 18 of 40 paired runs, because the first rename
removed the file the second was about to rename. `config.WriteFile` now creates a unique
temporary file in the same directory, syncs it, and renames it into place, so a reader sees
one whole version and no writer can break another. The model index uses it too, and its
mutex is documented for what it is: in-process serialisation.

An atomic replace is not the whole answer for that index, though: two processes each write
their own snapshot, so the loser's freshly inspected entries disappeared and the next scan
paid for them again - which the inspector's comment called "a few hundred milliseconds at
most" and which measured 5.95s per checkpoint on the reference machine. `saveIndex` now
reads the file again before writing and keeps every entry it did not inspect itself,
narrowing the window from the length of a scan to the microseconds between that read and
the rename. No lock: `flock` would make the second command wait for the first instead of
merging with it, and it does not exist on one of the four platforms this project builds
for.

**Rejected.** Refusing to write a value that fails validation - it breaks staging, which is
already the documented workflow. Keeping the per-key apply list and correcting it - a list
that has to be kept in step with what three processes read has already failed once.
`flock` around the model index - it buys a cache entry, at the price of a lock whose
failure mode (a stale lock file after a crash) is worse than a re-inspection. Deriving the
unit's `--keys` from the configuration at `config apply` time - re-rendering a unit to
apply a setting is a second code path that can disagree with the file, which is the shape
D22 removed for the server config.

Pinned by `TestConfigSetFailsWhenTheValueBreaksTheConfig`,
`TestConfigSetNamesHowToApplyEverySetting`, `TestSetRequestOptionsAsKeyValue`,
`TestSaveIsSafeForConcurrentWriters`, `TestHotkeyUnitLetsTheConfigurationOwnTheKey` and
`TestServerUnitRunsVoiceimeWithSubcommand`.

## D27 — One activation is one input context, and the text follows the newest.

**Decision.** The IBus engine counts input contexts: the session starts with the first
`Enable`, delivers to the most recently enabled one, and stops when the last `Disable`
arrives. The per-engine object de-duplicates, so the handler sees one `Enable` and one
`Disable` per context even when the daemon repeats one.

**Why.** The handler had one sink, fixed when the session started, and it returned early
from every later `Enable` ("already dictating"). Two things followed, and neither reported
anything: a second window received no text, because delivery still went to the context that
started dictation, and a single `Disable` stopped the session, so moving between two
windows ended dictation mid-sentence and committed the fragment. `Enable` is documented in
this code as arriving per input context, and the daemon creates an engine object per
context (`docs/ibus-protocol.md`), so counting them is what the protocol already provides -
the bug was treating a count as a boolean.

**Why the de-duplication is in the engine object.** The handler cannot tell two contexts
apart: `Disable()` carries no argument, so a set of controls could be added but never
pruned correctly. The engine object is the only thing that knows which context a call
belongs to, so it is where "this one is already active" is decided. A repeated `Enable`
for an active context therefore reaches the handler zero times rather than being ignored
there, which is what keeps the count balanced - the failure mode of an unbalanced count is
dictation that never stops, which is worse than one that stops early.

**Rejected.** Stopping on any `Disable` and accepting a lost sentence - it is the user's
speech, and the commit already happened for what was spoken. Delivering to *every* active
context - one utterance typed into two windows is not a feature. Changing `ibus.Handler`
to pass a context identity through `Enable`/`Disable` - the daemon's own object already
has it, and widening the interface to fix an accounting bug in one implementation of it
would make every handler carry the distinction.

Pinned by `TestEngineHandlerFollowsTheNewestInputContext` and
`TestSwitchableSinkFollowsItsTarget`.

## D28 — What the client cannot do is a validation error, not a runtime surprise.

**Decision.** `asr_server_url` must be `http://` and is validated as such. A chunked
response that ends inside a chunk is `io.ErrUnexpectedEOF`, never `io.EOF`. Writes to the
streaming server carry a ten-second deadline. `hotkey --engine` is gone.

**Why.** The live stream is spoken over a raw socket, by hand, because `http.Client.Do`
does not return until the request body is finished (measured; see `internal/asr`). That
client cannot do TLS, while the repair path uses `net/http` and can - so an `https`
address used to pass validation, pass `doctor` (whose `/health` probe is `net/http`), and
then fail every dictation while repairs worked. A configuration where half the pipeline
functions is the most expensive kind: every check that exists says it is fine. The
syntactic check is in `config.Validate` next to `download_mirror`, so it stays offline and
reports the same shape `server.ParseURL` expects.

The reader had a smaller version of the same problem: `io.EOF` inside a chunk was swallowed
and could return `(0, nil)`, which a reader is entitled to retry, so a truncated transcript
surfaced as `bufio.Scanner: too many empty reads` - a complaint about the scanner instead
of about the server. And a write had no deadline, so a server that stopped reading blocked
the capture loop with nothing to report; ten seconds of a 640-byte write is not
back-pressure, and failing the session says something a user can act on.

`hotkey --engine` was declared, documented as "engine to switch to while dictating", and
never read: `ibus_engine_name` is the setting, and `config set` is how it is changed. D23's
rule applies to flags as much as to commands - a flag that silently does nothing invites a
measurement of a pipeline nobody runs.

**Rejected.** Implementing TLS in the streaming client - a second transport, a certificate
policy and a dependency, for a server that runs on this machine and is reached over
loopback. Accepting `https` in validation and failing at session start - that is the
behaviour being removed. Dropping the write deadline and relying on the server's idle
timeout - the client would be the only party that cannot tell a wedged server from a silent
one.

Pinned by `TestValidateRejectsAServerAddressTheStreamCannotUse`,
`TestOpenStreamRejectsAnAddressItCannotSpeak` and
`TestChunkedReaderRejectsATruncatedChunk`.

## D29 — A requested stop is not a session failure, on the read side too.

**Decision.** A read error that happens because the session closed its own socket is not
recorded as `Result.ProcessErr`. `Stream` keeps its context and consults it in the reader.

**Why.** D25 recorded this rule for writes (a stop closes the socket under the capture
loop, so the next write fails) and D24 made a failed stream *withhold* the draft instead of
committing it. The reader was left out: cancelling closes the connections, the reader sees
`use of closed network connection`, and the session recorded a failure it had caused -
which then took D24's withholding branch instead of the end-of-session rule. Two user-
visible symptoms, both found by running the installed build rather than the tests:
`voiceime stream --seconds 3` in a quiet room printed a summary with no line explaining
why nothing was committed (the "no speech detected" report was never reached), and every
hotkey release ended in the log as `streaming session failed; withheld N characters`
with the cause being our own `Close`. A session that ends because the user stopped is a
session that ended; a session whose server went away is still a failure, and that one is
recorded while the context is still live.

**Rejected.** Treating every `ProcessErr` as suspicious when the context is done - a
server that fails *before* the stop is a real failure and must keep its draft withheld.
Detecting intent in `Close` (a "stopped" flag set by the caller) - the context already
carries exactly that information, and a second flag would have to be kept in step with it.

Pinned by `TestCancelIsNotAServerFailure` and
`TestStoppedSessionWithNoSpeechExplainsItself`.

## D30 — The engine writes down what it did, because nothing else can.

**Decision.** A dictation session appends its own account to
`~/.cache/voiceime/engine.log` (0600, timestamped, bounded to 1 MiB per engine start),
and it records the commit - the line that says text left the process. `--trace` still
sends the same log plus the incoming D-Bus calls to a file of the caller's choosing.
`voiceime engine status` names the log.

**Why.** The engine is spawned by ibus-daemon with its stdout and stderr on pipes the
daemon discards: measured, `/proc/<pid>/fd/1` and `/proc/<pid>/2` are pipes, and the
journal contains no line from the engine. So a dictation left no record of itself. That
was not theoretical while writing this: a user pressed the hotkey, spoke for twelve
seconds, activated and deactivated the engine, and there was no way to say whether the
session had heard anything, repaired anything, delivered anything - the service's log
showed a streaming window and an offline decode, the listener's log showed the toggle,
and the half in between, the half that decides what the user sees, was silent. `--trace`
existed for exactly this and was never enabled by the installed component, because it
writes D-Bus calls too and nobody wants every keystroke in a file by default.

**Why the commit is the line that matters.** "I pressed the key and nothing appeared"
has two causes with opposite fixes: nothing was recognised (endpointing, threshold,
microphone) or the text was delivered and the application did not show it (a delivery
bug, and not one in this repository - the engine's `CommitText` is the same signal every
IBus engine emits). Without a commit line the log could not tell them apart, so the one
piece of evidence that distinguishes the two was the one it did not write.

**What the same investigation established.** The delivery path itself works, end to end,
and was verified on the wire rather than by assertion: with a focused input context,
`dbus-monitor` on the IBus bus shows our engine (path
`/org/freedesktop/IBus/Engine/1`) emitting `UpdatePreeditText` and `CommitText`, and the
daemon forwarding both to the focused `org.freedesktop.IBus.InputContext` as
`UpdatePreeditTextWithMode` and `CommitText`. The one hop the README calls unverified -
text reaching a focused application - is therefore a property of IBus's forwarding, not a
gap in this code: what was never observed was a *focused window*, not a delivered signal.

**Rejected.** Writing the log to the journal (`systemd-cat`) - it adds a process per
session and a dependency for something a file does, and the engine has no systemd unit of
its own to attach to. Logging to stderr only and asking the user to run the engine by
hand - that is the state being removed: the failure is intermittent and the user is in
the middle of dictating, not in a terminal. Truncating the log on every session - it
would erase the record a user was about to read after the second attempt. Recording only
commit lengths and not the text - the text is what the user compares against what the
application showed, and the file is 0600 in their own cache directory.

Pinned by `TestEngineLogPathIsInTheCacheDir`, `TestAppendLogLineTimestampsAndRestricts`,
`TestRotateLogOnlyDropsWhatIsTooBig`, `TestEngineLogsWhatItCommits` and
`TestFirstRunesShortensWithoutSplittingCharacters`.

## D31 — Stop what you own, and never guess a keyboard.

**Decision.** `config apply` stops engine processes by pid, chosen by executable, and asks
them to exit with SIGTERM before insisting. The one-shot `hotkey toggle` refuses to switch
when it does not know the engine to go back to, instead of falling back to a layout.
`stream --seconds` applies to `--file` as well.

**Why the pid.** The stop was `pkill -f "voiceime ibus-engine"`, which matches any process
whose *command line* contains that string. Measured while deploying this work: it killed
the shell that ran it, twice, because the shell's own command line contained the pattern -
and it would as easily kill an editor with these docs open. The pattern also made the
command miss its real target when the binary lived somewhere else. `processesRunning`
already answered this question correctly for `doctor` (it matches the executable, which
only this binary can be), so the stop now uses it: SIGTERM, wait up to the drain budget,
then SIGKILL. SIGTERM first because the engine's shutdown path is what commits the
sentence in progress (D25); SIGKILL as the fallback because a process that will not exit
keeps running the old binary, which is the thing `apply` exists to prevent.

**Why no layout guess.** The X11 toggle path remembers the engine it switched away from in
a file, and when that file was missing it fell back to `xkb:us::eng`. On any machine whose
layout is not US English, "stop dictating" silently changed the user's keyboard - and the
only trace was a line saying "back to xkb:us::eng". The listener path already refuses this
case ("the previous engine is not known; not switching"); the toggle now refuses the same
way, with the reason and a fix a person can act on. A missing state file is a normal state
(the first toggle after a reboot), not an invitation to choose for the user.

**Why `--seconds` with `--file`.** The timer was armed only for the microphone, so
`stream --file x --seconds 5` replayed the whole file and said nothing about the flag. That
is the failure D23 names - a flag that silently does nothing - and the fix is the condition
it should always have had.

**Rejected.** `pkill -x voiceime` - it still matches by name, kills any voiceime process
including the listener and the server, and misses a binary that was renamed. Matching on
`/proc/<pid>/exe` and killing only pids whose command line also contains `ibus-engine` -
that is what `processesRunning` does. Remembering the previous engine in the toggle by
asking IBus - the only call that answers "what was selected before" is the one that now
returns `voiceime`, so there is nothing to ask; the listener's in-memory copy is the only
honest record, which is why the failure is reported rather than papered over.

Pinned by `TestProcessesRunningIgnoresProcessesThatMerelyMentionThePattern`. The other two
are conditions in code paths that need a live IBus daemon and a running server; both were
verified by hand on this machine (the shell that ran `config apply` survived, and a 1.6s
replay stopped at 1.0s with `--seconds 1`).

## D32 — Which model each role uses is one answer, in one place.

**Decision.** `models.Resolve` decides both checkpoints: an explicit path wins, an empty
`streaming_model` picks the best installed streaming model, an empty `refine_model` reuses
the streaming model. The generated server config, `models list` and `config show` all call
it.

**Why.** The rule was documented once and implemented three times, and the copies had
drifted into a defect: the server spec resolved repair on its own with the ranking, so a
user who chose a streaming model that was not the top-ranked one got a service repairing
with a *different* checkpoint than the configuration promised - `config show` said "empty
means reuse the streaming model: chosen.gguf" while the unit served best.gguf for repair.
Nothing reported it: both models exist, both load, and the symptom is text that differs
slightly from what a second pass would have produced. That is the exact failure mode
`config apply`'s readiness check was added for (D22), one layer down.

**Consequence.** `models list`'s `streaming` field now reports the model that will be used
rather than the auto-pick, which is what its name always claimed. A `--stream-model` or
`--repair-model` flag is still a one-off override; it is folded into the setting before
resolution, so the flag beats the setting and nothing else changes.

**Rejected.** Keeping the three implementations and adding a test that they agree - the
test would have to know all three, and the next setting would be added to two of them
again. Resolving repair to "streaming, unless empty, in which case rank" - that is the old
behaviour, and the setting's own description says otherwise.

Pinned by `TestResolveReusesTheConfiguredStreamingModel`,
`TestResolvePicksOneModelForBothRoles`, `TestResolveHonoursAnExplicitRefineModel`,
`TestResolveBreaksATieOnLanguage`, `TestResolveWithNothingInstalled`,
`TestResolveExpandsHome` and the four `TestServerSpec*` tests.

## D33 — The IBus component is one of the things `config apply` applies.

**Decision.** `config apply` reads the installed component, changes the two fields the
configuration owns - the engine name and its language - and, if anything changed, rewrites
the file, drops IBus's registry cache and restarts the daemon. Everything else in the file
is carried through unchanged, the exec line above all.

**Why.** `ibus_engine_name` was the one setting `apply` silently ignored. The name lives in
the component XML, which `engine install` writes, while the shortcut listener and every
`ibus engine <name>` call read it from the configuration - so changing it produced exactly
the failure this command exists to prevent: every other setting applied, the selector
pointed at an engine IBus had never heard of, and `doctor` reported "not registered" with
no reason. D26 established that a written value is not a setting in force; this is the last
place where that was still true.

**Why the exec line is preserved.** `apply` may be run from a checkout while the component
names an installed binary, which is a normal development sequence here. Re-rendering the
component from `os.Executable()` would silently repoint IBus at the checkout, where it
works until the checkout moves - and `install_check` already reports that situation as its
own finding rather than having it introduced by the command that is supposed to fix things.

**Why only when it changed.** Applying the component means `ibus restart`, which resets the
selected input method for every application. A command run after every settings change must
not do that when nothing about the component moved.

**Consequence.** The installed component survives a round trip through the reader; that was
not free. The `<symbol>` was stored as the XML entity `&#x1F3A4;`, which the parser decodes
to the character and the writer then escaped again - a rewrite would have replaced the
microphone with the literal text. It is stored as the character now, and pinned by
`TestComponentRoundTripsThroughTheInstalledFile`.

**Rejected.** Having `apply` run `voiceime engine install` - it writes the exec line from
`os.Executable()`, which is the hazard above, and it re-registers a component that was
already correct. Failing with "run `engine install --apply`" instead of applying - the
command's whole contract is that the configuration file is the only thing a user has to
edit, and this is a setting like any other.

Pinned by `TestWithConfiguredEngineAppliesOnlyWhatTheConfigOwns`,
`TestComponentRoundTripsThroughTheInstalledFile` and
`TestReadComponentRejectsAFileWithoutAnEngine`.

**Addendum (the installed files already in the wild).** The symbol repair only helps a
file that `config apply` is willing to rewrite, and a file written by the buggy writer is
*stable* under a naive round trip: it carries the literal text `&#x1F3A4;`, which parses to
that text, which escapes back to the same bytes. So `ReadComponent` decodes a numeric
character reference on the way in (`decodeCharacterReference`), which makes the re-render
differ, which is what lets apply heal an installation that already has the wrong symbol
instead of reporting it as unchanged.

## D34 — The engine to return to is one record, written where it outlives the process.

**Decision.** Both paths that toggle dictation - the resident listener and the one-shot
`hotkey toggle` an X11 keybinding runs - read and write the same file,
`~/.cache/voiceime/hotkey-previous`. Neither guesses a layout when it is missing; both
refuse with the reason.

**Why.** The listener kept the previous engine in memory and the toggle kept it in a file,
which are two answers to one question: after a reboot the listener knew nothing while the
toggle did, and the toggle's fallback was `xkb:us::eng` - so on any machine whose layout is
not US English, "stop dictating" silently changed the keyboard. Then D31 made both refuse
instead of guessing, which left the same fact recorded twice for no reason. A file, because
the fact has to survive the process that recorded it: the listener is restarted by
`systemctl --user` and by `config apply`, and the toggle is a different process entirely.

**Consequence.** A recorded engine survives the listener restarting mid-dictation, so the
engine a user comes back to is the one they left - which the in-memory copy could not
promise. The file is removed on the way back, so a stale name cannot be reused after the
user changes their input method by hand; the next toggle records whatever is current.

**Rejected.** Asking IBus for the previously selected engine - no such call exists; the one
that answers "what is selected" returns `voiceime` once dictation started, which is the
reason the record exists at all. Keeping the in-memory copy as a fast path in front of the
file - it would be a second answer again, and reading a one-line file is not a cost worth an
inconsistency.

Pinned by `TestPreviousEngineIsRecordedOnceForBothPaths`.

## D35 — One command writes the model cache; everything else reads it or says it cannot.

**Decision.** `voiceime models list --refresh` is the only writer of the inspection cache.
`models.Cached` (read-only, runs nothing) is what every other command uses; `models.Refresh`
is what `--refresh` runs. A file the cache cannot answer for is reported as `inspected:
false`, not with its previous answer and not as "not a speech model". `doctor` reports such
files as a finding with `--refresh` as its fix, and `server install` / `server run` refuse to
serve a model they cannot identify, naming the same command. `models install` no longer
refreshes: it prints the next command instead. `doctor --refresh` is gone.

**Why.** The cache had no owner. `Scan` read the directory, asked audio.cpp, and wrote the
cache, and every command called it - so `doctor`, a diagnostic, wrote files, and the write
was the thing that could fail: measured, with a read-only cache directory, `doctor` reported
`models: fail`, exit 3, on a machine where all fifteen models were fine - and its fix was
`voiceime models refresh`, a command deleted in D23. Nothing about that was a model problem.

**Why a single writer, not a lock.** With one writer there is nothing to serialise: the
"two commands scanning at once" race disappears rather than being merged around. It also
makes the price explicit. Identifying a checkpoint measured 5.95s on this machine, so a
command that inspects silently is a command that sometimes takes a minute - and a user who
never asked for that has no way to know why. A command that says "3 files have not been
inspected; run `models list --refresh`" is slower by one deliberate step and faster every
other time.

**Why install does not refresh.** It was the obvious exception, and it is the wrong one:
files arrive by routes voiceime never sees (a hand-copied GGUF, audio.cpp's own model
manager, a restored backup), so "install refreshes" only covers the case where a user typed
our command. One rule with no exceptions is what a user can hold in their head: refreshing
is a command, and every message names it.

**Consequence.** A fresh install is three commands, not two: `models install`, `models list
--refresh`, then `server install` - and the sequence is enforced rather than assumed, because
`server install` refuses a model it cannot identify and prints the missing step. `server run`
never inspects, so a service start cannot hide six seconds of work per unknown file; if a
model was copied in without a refresh, the service says so and exits rather than serving a
guess. A stale cache entry (the file was replaced) is treated as no entry at all, because
reporting the old family would be a lie - the previous version did, which is how a replaced
checkpoint kept its old capabilities until someone happened to run a full refresh.

**Rejected.** Keeping `doctor --refresh` - two writers again, and a diagnostic that changes
what the next diagnostic sees. Having readers inspect but not write - every command then pays
six seconds for the same file forever, which is worse than either alternative. A lock around
the read-modify-write (D26's merge) - it makes a shared cache safe; it does not make an
unowned one clear. Refreshing from `server install` - it would cover exactly the files the
user named, leaving the rest of the directory unclassified, so the next `models list` would
still be half-unknown.

Pinned by `TestCachedRunsNothingAndWritesNothing`, `TestRefreshInspectsOnceAndRemembers`,
`TestCachedDoesNotServeAStaleAnswer`, `TestRefreshDropsDeletedModels`,
`TestModelsListDoesNotInspectOrWrite`, `TestModelsListRefreshIsTheOnlyWriter`,
`TestModelsListRejectsForceWithoutRefresh` and
`TestDoctorReportsUninspectedModelsWithoutTouchingTheCache`.
