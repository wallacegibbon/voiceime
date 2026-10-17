# Agent guide

This document is written so that a program can install, configure and verify
voiceime without a human. Every command is non-interactive, idempotent and has a
`--json` mode. `voiceime capabilities` prints the same surface at runtime,
derived from the code, so it is the authoritative version if the two disagree.

## Building and installing

Deploying on a new machine end to end — prerequisites, configuration, model,
engine registration, shortcut listener, and what to assert at each step — is
[docs/DEPLOY.md](DEPLOY.md). This document is the command reference; that one is
the procedure.

The Makefile is the intended interface; every target is non-interactive.

```
make build                     # bin/voiceime, version stamped from git describe
make check                     # gofmt, go vet, unit tests, unreachable-code check
make install                   # ~/.local/bin (the default PREFIX); DESTDIR honoured
make measure AUDIO=speech.wav CHUNK=3
```

A plain `go build ./cmd/voiceime` also works and produces the same binary, with
the version reporting as `dev`. An installed copy is not required: running
`bin/voiceime doctor` straight from the checkout is the normal development loop,
and nothing in the program depends on being on `PATH`.

Installing does not restart anything: a process keeps the binary it was started
from, so rebuilt code runs only after the engine and the listener have been
restarted, and until then `doctor` reports the difference as a `warn` on
`engine_process` / `hotkey_process`. The procedure, and which of the restarts
systemd does for you, is
[DEPLOY.md § Updating an installed copy](DEPLOY.md#updating-an-installed-copy).

## Ground rules

* **No prompts, no GUI.** Nothing waits for input. If a command needs a decision,
  it is a flag or a config key.
* **`--json` may be placed anywhere** on the command line, before or after the
  subcommand: `voiceime doctor --json`, `voiceime --json doctor` and
  `voiceime transcribe f.wav --json` all work. Flags and positional arguments may
  also be interspersed.
* **One JSON object per invocation**, on stdout on success, on stderr on failure:
  `{"command": ..., "ok": true, "data": {...}}` or
  `{"command": ..., "ok": false, "error": {"kind": ..., "message": ..., "fix": ...},
  "exit_code": N}`.
* **Exit codes** are the primary machine signal:

  | Code | Meaning | What to do |
  |---:|---|---|
  | 0 | success | — |
  | 1 | the command ran and failed | read `error.message` |
  | 2 | the command line was wrong | fix the invocation, do not retry |
  | 3 | a prerequisite is missing | run `error.fix`, then retry |

* **`fix` is a runnable command.** Findings from `doctor`, `config validate` and
  precondition failures carry one.
* **Nothing is destructive without a flag.** `models install` writes into the
  models directory; `config init` refuses to overwrite without `--force`.

## Configure

The configuration file is the single source of truth. Read the whole surface with:

```
voiceime capabilities --json          # commands, flags, settings, detected environment
voiceime config show --json           # current values plus each key's description
```

`config init` writes the defaults, **discovers** an audio.cpp checkout when it finds
one (writing the real path into the file, and saying so), and then prints the
settings a new machine should look at rather than accept:

```
$ voiceime config init
wrote /home/u/.config/voiceime/config.toml
discovered an audio.cpp checkout at /home/u/src/audio.cpp
review audio_cpp_dir          ~/playground/audio.cpp  (the default names the checkout voiceime was developed in; ...)
review threads                4  (this machine has 8 physical cores; ...)
fix: voiceime config set threads 8
review language               zh  (the reference user's language; ...)
...
```

The same list is in `--json` under `data.review`, with a `fix` for the entries that
have one. Nothing is changed silently: a value is only written when the file did
not exist or `--force` was given.

Set keys with the generic setter — it converts types, writes atomically and
reports the new config's validity:

```
voiceime config set models_dir ~/models
voiceime config set streaming_model /home/u/models/SenseVoice-Small-GGUF/sensevoice-small-q8-audiocpp-v1.gguf
voiceime config set chunk_seconds 3
voiceime config set language zh
voiceime config set refine_on_segment_end true
```

Settings that matter, and the reasoning a caller needs to pick values:

| Key | Default | Notes |
|---|---|---|
| `models_dir` | `~/models` | Scanned for models; downloads land here. May contain unrelated models. |
| `audio_cpp_dir` | `~/playground/audio.cpp` | Locates `audiocpp_cli` and the model manager. The default is the reference machine's layout, so it is usually wrong elsewhere - discovery covers that: a checkout is also looked for in `~/audio.cpp`, `~/src/audio.cpp` and similar, and derived from `VOICEIME_ASR_BIN`/`AUDIOCPP_CLI`. |
| `asr_bin` | `""` | Explicit CLI path; empty means discover. |
| `backend` | `cpu` | `audio.cpp` backend: cpu, cuda, vulkan, hip, metal. `doctor` fails when the local `audiocpp_cli` was not built with it, and warns when a GPU driver is present while this says `cpu`. |
| `threads` | `4` | Worker threads for both the streaming engine and repairs. The default is the reference machine's 8-core setting; `doctor` warns when it exceeds this machine's physical cores, because the two passes compete for the same ones. |
| `streaming_model` | `""` | Empty means pick automatically. |
| `refine_model` | `""` | Empty means reuse the streaming model (fastest). |
| `asr_server_url` | `""` | Where the resident recognition server listens. `voiceime server install` sets it; empty means dictation cannot run, and `doctor` says so. Both passes go through this one service, so nothing is started per session and no model is loaded per request. |
| `asr_server_stream_model` | `stream` | The server entry that serves live partials. |
| `asr_server_model` | `refine` | The server entry the offline repair uses. It must differ from the streaming entry: the server holds a model for a whole live session, so a repair on the same id waits for the dictation to end (measured 10.9 s). |
| `language` | `zh` | Passed to the model; empty asks it to auto-detect. |
| `chunk_seconds` | `3` | The streaming window: how much audio a draft is decoded from. Below 1.5 measurably corrupts CJK output; validate warns. It does not gate when text is committed - a finished utterance is decoded offline and delivered when it closes - so it can be raised for accuracy without making delivery wait. |
| `refine_on_segment_end` | `true` | The pass that repairs window-boundary damage. |
| `capture_rate` | `16000` | Must stay 16000 (models' VAD). Validation error otherwise. |
| `capture_source` | `""` | PipeWire source name; empty uses the default input. |
| `silence_rms_threshold` | `600` | **Floor**, not a fixed threshold: the detector adapts up to twice the noise floor it measures in the stream, and never below this value. 600 clears the reference room's idle noise (20ms p90 = 436 RMS) and stays under normal speech; too high a value makes quiet speech silently ignored, so measure before raising it - see [measurements.md](measurements.md#endpointing-in-a-real-room). |
| `endpoint_min_speech_ms` | `300` | Speech shorter than this is not an utterance. Raise it in a room with impulsive noise (keystrokes, chair). A phrase shorter than it is treated as noise and is **not committed at all** when dictation stops — if it is the only thing in the session, nothing is delivered and the session reports how much it withheld. Lower it only if you dictate short words, and expect more noise to be taken for speech. |
| `download_mirror` | `https://hf-mirror.com` | Endpoint downloads go through, written to `HF_ENDPOINT`. Any Hugging Face protocol host works: `https://huggingface.co`, a company cache, a LAN address. |
| `session_options` / `request_options` | `{}` | Passed straight to `audiocpp_cli`. |

Validate after changing anything:

```
voiceime config validate --json       # exit 3 when a blocking issue exists
```

## Install a model

```
voiceime models recommended --json    # what to install, and why
voiceime models sizes <pkg> --json    # reachability and size, downloads nothing
voiceime models install <pkg>         # downloads into models_dir
voiceime models installed --json      # what is present locally, no network
```

Downloads go through the single endpoint in `download_mirror`, which must be an
absolute `http(s)` URL; `config validate` rejects anything else, and the value is
passed to the model manager as `HF_ENDPOINT`. Whether an endpoint works is a
property of the endpoint *and* the package: `models sizes` reports the failure per
package, so a caller that sees a 404 can set another endpoint and retry the same
package.

Packages to install, by machine (measured on the reference machine; see
[measurements.md](measurements.md) for the numbers and
[DEPLOY.md](DEPLOY.md#4-install-a-model) for the commands):

| Machine | `streaming_model` | `refine_model` | Package ids |
|---|---|---|---|
| **CPU only** | `sensevoice_small_q8` | empty - reuses the streaming model | `sensevoice_small_q8` |
| **With a GPU** | `sensevoice_small_q8` | `qwen3_asr_0_6b_q8_0` | both |

* `sensevoice_small_q8` (242 MiB) is the streaming model on both: multilingual,
  ~21x realtime on CPU, 0.3 s to load per session. On CPU it is also the repair
  model, because it repairs a finished sentence in 0.55 s.
* `qwen3_asr_0_6b_q8_0` (1.07 GiB) is the more accurate offline reader and the
  only reason to add a second model. It costs its memory all the time (the service keeps
  both entries resident: 1403 MB with the pair above) and saves the decode, which is the
  point of the service: a repair is a 0.5-0.8 s request instead of a 5.7 s process, and
  a dictation starts in microseconds. See docs/decisions.md D22.
* Because the streaming model is what loads on every activation, `streaming_model`
  should stay `""` (empty means "pick the best installed streaming model") unless a
  caller deliberately wants to pin a choice. `models recommended` prints the same
  candidates with their sizes.

## Verify

```
voiceime doctor --json                # every prerequisite, each with its fix
voiceime doctor --refresh --json      # re-inspect models instead of using the cache
voiceime devices --json               # capture sources and the default one
```

`doctor` covers: the config file, the `audiocpp_cli` binary and its version, the
model manager script, the models directory, model selection for streaming and
repair, capture tooling and default source, IBus engine registration, the settings
that are properties of the machine (`threads` against the physical core count,
`backend` against what the local binary was built with), the installed
artifacts (the component file's `<exec>` and the systemd unit's `ExecStart`, both of
which record an absolute path), whether the running engine and listener are the
binary running `doctor`, and optional notification/sound tooling. Exit code 3 means
at least one check failed; the `fix` of each failure is the next command to run.
Loop until it exits 0.

## Exercise the pipeline

Both of these print their own timings, which is what makes quality questions
answerable without a human listening:

```
voiceime transcribe speech.wav --model <path> --json
voiceime stream --file speech.wav --realtime --chunk 3 --json
voiceime stream --seconds 5                    # capture from the microphone (the default source)
```

`stream` accepts `--events` for one JSON object per event instead of human text.
Event kinds: `ready` (model loaded, with load time), `partial` (new text, with
the audio position at which it arrived), `segment` (an utterance closed), `vad`
(model-reported voice activity, when a family reports it), `repair` (offline
re-decode of a segment, with its cost), `skipped` (something was not done, with
the reason - `no speech detected; nothing committed` is the common one), `commit`
(text delivered), `summary`.

Text is committed only when the endpointer closes an utterance. An open utterance
is preedit, which applications render underlined, and pressing Escape discards it.
So "the text stays underlined" always means "the detector has not found a boundary
yet": check `silence_rms_threshold` and `endpoint_silence_ms` before anything else.

A session that ends without the endpointer accepting a single utterance commits
nothing, because streaming ASR produces plausible words for almost any input and
typing them into a focused application is worse than typing nothing. It is not a
silent loss: the text was visible as preedit, so the reason names how much of it
was withheld (`no speech detected; withheld N characters the recogniser produced
for audio the detector never called speech`). A `skipped` reason with a count means
the detector could not hear the speaker - the fix is the room measurement below. No
reason at all, with text on screen, means the session ended some other way.

With `--realtime`, the `fed_seconds` of a partial is also the time from the user
speaking to that text appearing, which is the latency number to compare against
`chunk_seconds`.

## Text delivery

The engine hosts the full pipeline: activating it starts capture, streams partial
text as an IBus preedit, repairs each finished utterance offline, and commits the
result into the focused application.

```
voiceime engine install              # write the component file, print what to run
voiceime engine install --apply      # also set the activation environment and restart IBus
voiceime engine install --system     # print the one-line root install instead
voiceime engine status --json        # is the engine registered, and where is it installed
voiceime engine uninstall
```

Two facts decide whether this works, and both were measured (see
[docs/ibus-protocol.md](ibus-protocol.md)):

* IBus reads `/usr/share/ibus/component` and whatever `IBUS_COMPONENT_PATH`
  names. It does **not** read `~/.local/share/ibus/component`, so a user install
  must set that variable in the activation environment and restart the daemon.
* `IBUS_COMPONENT_PATH` **replaces** the default search path. A value naming only
  voiceime's directory hides every system engine, which is why the value printed
  by `engine install` always includes `/usr/share/ibus/component`.
* The daemon's component registry is **cached** (`~/.cache/ibus/bus/registry`) and
  a cached run does not look at a newly configured directory, so `--apply`
  removes that file before restarting IBus. Without this the engine stays
  invisible even though the environment variable took effect.

Then either switch to the engine directly (`ibus engine voiceime`, and away again
to stop), or run the shortcut listener so one key does it:

```
voiceime config set hotkey "<F9>"     # a tap toggles dictation
voiceime hotkey install               # systemd --user unit, starts at login;
                                      # on X11 also a GNOME keybinding (see below)
voiceime hotkey status --json         # data.session, data.mechanisms, data.gnome_binding
voiceime hotkey toggle                # the one-shot toggle the keybinding runs
voiceime server install               # the resident recognition server (required for dictation)
voiceime server status --json         # data.unit_active, data.listening, data.entries_served
voiceime server uninstall             # stop and remove it
voiceime hotkey run --log-events      # foreground, prints every event the daemon sends
```

`hotkey install` picks the mechanism the session can actually use, and the commands
above are the same either way: IBus's global shortcut on Wayland (where the
compositor grabs the key and the listener receives it), and on X11 a GNOME custom
keybinding to `voiceime hotkey toggle`, because the IBus property is not grabbed
there. It reads the session from `XDG_SESSION_TYPE`, then `WAYLAND_DISPLAY`/`DISPLAY`;
when neither names a display server (a bare ssh or console shell) it installs only the
unit and returns a `warning` - re-run it from a terminal inside the session, or the
key will do nothing on X11. `hotkey status` reports which mechanism is live
(`data.session`, `data.mechanisms`, `data.gnome_binding`), and `hotkey uninstall`
removes both.

On X11 the key is run by `gsd-media-keys` as a command - once per key event,
auto-repeat included - so `hotkey toggle` ignores a second invocation within 700 ms.
A held key is one toggle rather than dozens of them (docs/decisions.md D19); the cost
is that a deliberate second tap closer than that is swallowed, so tap again.

Use a function key, not a modifier combination: IBus consumes the release of the
keys it grabs, so Ctrl+' leaves Ctrl latched in the focused application until it is
tapped again (measured; see docs/decisions.md D13).

Two diagnostics that need no microphone:

```
# Commit a fixed string on activation, proving delivery only.
voiceime ibus-engine --ibus --commit-on-enable "hello from voiceime"

# Run the real pipeline over a WAV: recognition, repair and delivery, no speech.
voiceime ibus-engine --ibus --audio-file speech.wav --trace /tmp/engine.log
```

`--trace` writes both the incoming D-Bus calls and the session log to a file,
which matters because ibus-daemon spawns the engine detached: its stdout is
discarded, and without the trace a silent failure looks identical to success.
Run the engine by hand (as above) to see the same log on stderr instead.

## Troubleshooting table

| Symptom | Cause | Action |
|---|---|---|
| `doctor` exits 3 with `audio_cpp` failing | `audiocpp_cli` not found | `voiceime config set audio_cpp_dir <checkout>` or `voiceime config set asr_bin <binary>` |
| `no speech models found` | empty or wrong models directory | `voiceime models install sensevoice_small_q8` |
| `models install` fails with 404 | that package is not on the configured endpoint | `voiceime config set download_mirror https://huggingface.co` (or any host that has it) and retry |
| `doctor` fails `download_mirror` | the value is not an `http(s)` URL, e.g. a preset name from an older config | `voiceime config set download_mirror https://hf-mirror.com` |
| Repair fails with `Silero VAD model path does not exist` | the CLI's bundled assets resolved relative to the working directory | voiceime already sets the working directory and disables chunking for repairs; if it recurs, set `audio_cpp_dir` correctly |
| Text is corrupted and windows are short | `chunk_seconds` below 1.5 | `voiceime config set chunk_seconds 3` |
| Model aborts with `only supports sample_rate=16000` | input is not 16 kHz | `voiceime config set capture_rate 16000` |
| `models list` shows unrelated files | the models directory is shared | expected; discovery filters to ASR models and reports the count it ignored |
| Text stays underlined and never commits | the detector has not closed an utterance, so it is still preedit; Escape discards preedit | measure the room and set `silence_rms_threshold` / `endpoint_min_speech_ms` (see [measurements.md](measurements.md#endpointing-in-a-real-room)); `stream --events` shows whether a `segment` event happens |
| Dictation produces nothing, and `stream --events` reports `no speech detected` | the threshold is above the speaker's voice | lower `silence_rms_threshold` and retry |
| Dictation produces nothing, and the session reports `withheld N characters` | the recogniser heard something but the endpointer never called it speech, so committing it would type the recogniser's guess | measure the room; lower `silence_rms_threshold` (a quiet speaker) or `endpoint_min_speech_ms` (short utterances). `N` is the size of what the room measurement got wrong |
| The service restarts forever with `status=203/EXEC` | the unit's `ExecStart` names a binary that no longer exists (the checkout was moved or renamed) | `voiceime hotkey install` then `voiceime doctor`; see [DEPLOY.md § Recovery](DEPLOY.md#recovery) |
| `ibus list-engine` lists `voiceime` but dictation does nothing | the running engine process was started from an older binary, or the component file names a path that no longer exists | `ibus engine xkb:us::eng && pkill -f "voiceime ibus-engine" && ibus engine voiceime` |
