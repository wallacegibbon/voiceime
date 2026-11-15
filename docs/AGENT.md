# Agent guide

Everything here is non-interactive and `--json` on request, so an agent can install,
configure and verify voiceime by reading this file and running commands.
`voiceime capabilities --json` prints the same surface at runtime, derived from the code;
if the two disagree, the runtime wins.

**Scope.** Commands and settings. The procedure for a new machine is
[DEPLOY.md](DEPLOY.md); the recognition path is [asr.md](asr.md); measured numbers are in
[measurements.md](measurements.md).

## Ground rules

* **No prompts, no GUI.** A decision is a flag or a setting.
* **`--json` may be placed anywhere** on the command line.
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
voiceime config init|show|get|set|apply|validate
voiceime model
voiceime server install|run|restart|status|uninstall [--port N] [--model PATH]
voiceime engine install [--apply]|status|uninstall
voiceime transcribe [--file recording.wav | --seconds N]
voiceime doctor
voiceime capabilities
```

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
* `server install` sets `asr_server_url` and `model` in the configuration and starts the
  unit; `server run` is what the unit executes.
* `engine install` writes the IBus component and **appends** `('ibus', voiceime)` to
  GNOME's input-source list, which is what makes the desktop able to select and show it.
  The list belongs to the user: nothing already configured is removed or reordered, the
  recent-use order is not written, and `engine uninstall` removes only voiceime's own entry.
  `--apply` also sets the activation environment, writes the same search path to
  `~/.config/environment.d/voiceime.conf` so a new login keeps the engine, and restarts
  IBus. `engine uninstall` removes that file too.
* `config apply` makes the running install match the file: restarts the service, rewrites
  the component when the engine name changed, converges the input source, and drops the
  engine so the next activation runs the new binary.

## Settings

| Key | Default | Notes |
|---|---|---|
| `asr_bin` | `""` | `audiocpp_cli`: the audio.cpp CLI, used to report which backends this build supports. `audiocpp_server` is taken from beside it. `config init` writes what it finds; nothing else searches the filesystem, and `PATH` is the only fallback. |
| `backend` | `cpu` | `audio.cpp` backend: cpu, cuda, vulkan, hip, metal. |
| `threads` | `4` | Worker threads for the server. |
| `model` | `""` | **Required.** The GGUF the service serves and the engine transcribes with. Nothing picks it for you: no scan, no download. |
| `language` | `zh` | Passed to the model; empty asks it to auto-detect. |
| `text_script` | `simplified` | Chinese output script: `simplified` or `preserve`. |
| `max_utterance_seconds` | `60` | A hold whose key never comes up stops recording here. |
| `asr_server_url` | `""` | The service root; `server install` writes it. Must be `http://`. |
| `asr_server_timeout_ms` | `30000` | Deadline for one transcription. |
| `capture_source` | `""` | PipeWire source name; empty uses the default input. |
| `push_to_talk` | `<Control>grave` | Held down to dictate; the release transcribes. Needs a modifier. |
| `request_options` | `{}` | Extra audio.cpp request options for the served model, `key=value`. |

Things every setting shares: `voiceime config set` writes the file atomically, validates
it, and names the command that puts it in force (`voiceime config apply`). Hand-editing is
supported and needs the same command.

## Verifying

```
voiceime doctor --json            # every finding carries a fix
voiceime server status --json     # data.unit_active, data.listening, data.served[]
voiceime engine status --json     # data.engine_registered, data.environment_set, data.gnome_input_source.listed
voiceime transcribe --seconds 5   # one decode from the microphone, with timing
```

The engine's own log is `~/.cache/voiceime/engine.log` (ibus-daemon discards the process's
stdout). `ready: hold … to dictate` on selection, then `recording`, `decoded …` and
`committed N character(s)` per hold.

`voiceime ibus-engine --ibus --audio-file speech.wav --trace /tmp/engine.log` runs the real
path over a file, with no microphone and no keyboard.
