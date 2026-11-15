# Agent guide

Everything here is non-interactive and `--json` on request, so an agent can install,
configure and verify voiceime by reading this file and running commands.
`voiceime capabilities --json` prints the same surface at runtime, derived from the code
(machine state is `doctor`'s, never repeated here); if the two disagree, the runtime wins.

**Scope.** Commands and settings. The procedure for a new machine is
[DEPLOY.md](DEPLOY.md); the recognition path is [asr.md](asr.md); what the engine sends over
the bus, and what IBus does with it, is [ibus-protocol.md](ibus-protocol.md); measured
numbers are in [measurements.md](measurements.md).

## Ground rules

* **No prompts, no GUI.** A decision is a flag or a setting.
* **`--json` may be placed anywhere** on the command line. It is the switch from a report a
  person reads to the contract above: `data` on stdout, the error object on stderr, and no
  prose on either. One line, always: the object an agent parses and the line a shell greps
  for are the same thing, and `grep '"id":"asr_server","status":"ok"'` only works when a
  record does not span lines.
* **One JSON object per invocation**: `{"command":…,"ok":true,"data":{…}}` on stdout, or
  `{"command":…,"ok":false,"error":{"kind":…,"message":…,"fix":…},"exit_code":N}` on stderr.

| Exit | Meaning | What to do |
|---:|---|---|
| 0 | success | — |
| 2 | usage error | read `error.message` |
| 3 | a precondition is missing | run `error.fix` |
| 1 | a genuine failure | read `error.message` |

## Commands

```
voiceime apply                                   # make this machine match the configuration
voiceime remove                                  # take back what apply installed
voiceime config show|get|set
voiceime model                                   # the checkpoint the configuration names
voiceime doctor                                  # every prerequisite, machine-readable
voiceime transcribe [--file recording.wav | --seconds N]
voiceime capabilities                            # the surface: commands, settings, paths, exit codes
```

Two more commands are entry points for other programs, not commands to run by hand:
`ibus-daemon` spawns `voiceime ibus-engine --ibus`, and the systemd unit's `ExecStart` is
`voiceime server run`. Neither takes settings as flags: the port comes from
`asr_server_url` and the checkpoint from `model`, so neither can disagree with the file.

* voiceime **fetches nothing and scans nothing**. A model is one GGUF file and the `model`
  setting names it: there is no models directory, no candidate list, and no ranking deciding
  for you. Every "nothing named" answer says what to fetch and what it costs on disk, because
  choosing a family and a size is otherwise a guess.
* What a checkpoint *is* - the audio.cpp family that picks the loader, which the generated
  service configuration requires - is read from the file's own metadata
  (`audiocpp.model_spec.*`), a few hundred bytes at its start. Nothing is loaded, no
  subprocess is asked and nothing is cached, so there is nothing to refresh and nothing can
  go stale. `voiceime model` prints that answer for the file you named; a GGUF without a
  model spec in it is refused by name, together with what it does say it is.
* `apply` is the whole installed side, in one command: it writes and starts the systemd user
  unit for the recognition service, **appends** `('ibus', voiceime)` to GNOME's
  input-source list (which is what makes the desktop able to select and show the engine),
  writes the component file and the search path to
  `~/.config/environment.d/voiceime.conf`, restarts IBus, then drops the engine processes and
  selects the input method again. It writes `asr_server_url` and `model` into the
  configuration as it goes, waits until the configured checkpoint is actually resident, and
  refuses to change anything if the configuration cannot work. Running it twice is how you
  check it: the second run reports everything unchanged.
* The input-source list belongs to the user. Nothing already configured is removed or
  reordered, and the recent-use order is not written; `remove` takes out voiceime's own
  entry and leaves the others.
* `remove` is `apply`'s opposite: the unit, the generated service configuration, the
  component file, the `environment.d` file, and the input source this tool added. It leaves
  the model file - and the `model` setting that names it - alone, because that file was
  never the installation's.
* There is no `server install`, `server restart` or `engine install`: one command makes the
  machine match the file, and `doctor` answers "is it there" with findings. Asking the same
  question through several commands, each with its own idea of what "installed" means, is how
  an install ends up described differently in three places.

## Settings

| Key | Default | Notes |
|---|---|---|
| `asr_bin` | `""` | `audiocpp_cli`: the audio.cpp CLI, used to report which backends this build supports. `audiocpp_server` is taken from beside it. **A property of this machine.** `voiceime apply` records the build it finds when this is empty; nothing else searches the filesystem, and `PATH` is the only fallback. |
| `backend` | `cpu` | `audio.cpp` backend: cpu, cuda, vulkan, hip, metal. cpu works everywhere; a GPU needs an audio.cpp build with that backend. |
| `threads` | `4` | Worker threads for the server. **A property of this machine:** more threads than physical cores contend rather than help, and `doctor` reports it as the `threads` finding. |
| `model` | `""` | **Required.** The GGUF the service serves and the engine transcribes with. Nothing picks it for you: no scan, no download. |
| `language` | `zh` | **The user's, not the program's.** Passed to the model; empty asks it to auto-detect, which is usually worse for Chinese and better for most other languages. |
| `text_script` | `simplified` | Chinese output script: `simplified` or `preserve`. The default is what the reference user writes; `preserve` keeps whatever script the repair model produced. |
| `max_utterance_seconds` | `60` | A hold whose key never comes up stops recording here. |
| `asr_server_url` | `""` | The service root; `voiceime apply` writes it. Must be `http://`. |
| `asr_server_timeout_ms` | `30000` | Deadline for one transcription. |
| `capture_source` | `""` | PipeWire source name; empty uses the default input. |
| `push_to_talk` | `<Control>grave` | **A chord of your choosing.** Held down to dictate; the release transcribes. Needs a modifier, and must not collide with a shortcut the applications you use claim. |
| `request_options` | `{}` | Extra audio.cpp request options for the served model, `key=value`. |

Things every setting shares: `voiceime config set` writes the file atomically, validates
it, and names the command that puts it in force (`voiceime apply`). Hand-editing is
supported and needs the same command. Neither creates a step first: the file appears with the
first value written, and `voiceime config show` prints what is on record (or the defaults,
with `exists: false`, when there is no file).

## Verifying

```
voiceime doctor --json            # every finding carries a fix; data.checks[].id is stable
voiceime transcribe --seconds 5   # one decode from the microphone, with timing
```

`doctor` is the only state report. Which question is which finding:

| Question | Finding |
|---|---|
| is the checkpoint resident, and is it the one the settings name | `asr_server` |
| is the component installed, and does it name a binary that exists | `component_exec` |
| will the engine still be registered after the next login | `component_env` |
| do this session's keys reach the engine at all | `session_engine` |
| is an old build still running | `running_build` |
| is the service unit installed, and does it run this binary | `server_unit` |

A failing finding is exit 3 with a `fix` an agent can run - usually `voiceime apply`.

The engine's own log is `~/.cache/voiceime/engine.log` (ibus-daemon discards the process's
stdout). `ready: hold … to dictate` on selection, then `recording`, `decoded …` and
`committed N character(s)` per hold. The lines that are not routine are the ones worth
reading: `the press before this one never saw its release` (a key event went to another
input context), `discarded the recording in progress` (the engine was deselected mid-hold),
`recording stopped at the …s limit` (the key was never released) and `engine process ending`
(how this engine stopped, which matters because no engine process means no input method for
the session).

`voiceime ibus-engine --ibus --audio-file speech.wav --trace /tmp/engine.log` runs the real
path over a file, with no microphone and no keyboard.
