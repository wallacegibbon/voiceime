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
  for are the same thing, and `grep '"id":"asr_endpoint","status":"ok"'` only works when a
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
voiceime apply                                   # install the engine: component, desktop entry, session
voiceime remove                                  # take back what apply installed
voiceime config show|get|set
voiceime doctor                                  # every prerequisite, machine-readable
voiceime transcribe [--file recording.wav | --seconds N]
voiceime capabilities                            # the surface: commands, settings, paths, exit codes
```

Two more names appear in the surface and neither is a command to run by hand:
`ibus-daemon` spawns `voiceime ibus-engine --ibus`, and the systemd unit's `ExecStart` runs
`audio.cpp` itself, on the config `scripts/asr-service.sh` wrote.

**Recognition is an endpoint.** The engine posts one recording to
`POST /v1/audio/transcriptions` and reads the text back; that is the whole of what it asks of
the machine. Whether the endpoint is a model on this box or a provider across a network is not
the engine's concern and is not written anywhere in it, so the two halves of a setup are
separate and each is optional in its own way:

```
voiceime apply                        # the desktop half: always needed, whatever answers
scripts/asr-service.sh <audiocpp_server> <model.gguf>   # a model on this machine: only if the endpoint is this machine
```

* An endpoint elsewhere needs nothing installed here. `scripts/asr-service.sh` says so and
  starts nothing when `asr_server_url` is not a loopback address; the model that answers is
  named by the `model` setting, and a credential by `api_key`.
* A model on this machine is one GGUF file, and the script is what puts it where it will be
  served: it asks `audiocpp_gguf --inspect` for the family the file carries, asks the build
  which backends it has and the CPU how many workers fit, and writes the two files the
  service consists of - `~/.local/share/voiceime/asr-server.json` and the unit. voiceime
  **fetches nothing and scans nothing**: a checkpoint that is not named is not used.
* `apply` is the whole desktop side, in one command: it writes the component file and the
  search path to `~/.config/environment.d/voiceime.conf`, **appends** `('ibus', voiceime)` to
  GNOME's input-source list (which is what makes the desktop able to select and show the
  engine), restarts IBus, then drops the engine processes and selects the input method again.
  It refuses to change anything if the configuration cannot work. Running it twice is how you
  check it: the second run reports everything unchanged.
* The input-source list belongs to the user. Nothing already configured is removed or
  reordered, and the recent-use order is not written; `remove` takes out voiceime's own
  entry and leaves the others.
* `remove` is `apply`'s opposite: the component file, the `environment.d` file, the input
  source this tool added, and - if this machine has one - the model service and its two
  files. It leaves the checkpoint itself alone, because that file was never the
  installation's.
* There is no `server` command at all, and no `server install`, `server restart` or
  `engine install`: one command per half, and `doctor` answers "is it there" with findings.
  Asking the same question through several commands, each with its own idea of what "installed"
  means, is how an install ends up described differently in three places. And nothing that runs
  long re-derives a decision: the service is `audio.cpp` on a written config, so the only code
  that has to know which build this machine has is the script that installs it.

## Settings

| Key | Default | Notes |
|---|---|---|
| `asr_server_url` | `""` | The endpoint the recording is posted to: `http://127.0.0.1:18080` for a model on this machine, `https://…` for a provider. The route is appended to it, so it is the root, not `/v1`. |
| `model` | `""` | The model id to name in the request. **Empty asks the endpoint what it serves**, which is what a local single-model setup wants, and why a local setup names nothing here. A path in this key is refused: the file belongs to the service, not to the settings. |
| `api_key` | `""` | Sent as `Authorization: Bearer <value>`. Empty sends no header. A credential lives here rather than in an environment the engine never starts. |
| `language` | `zh` | **The user's, not the program's.** Passed to the model; empty asks it to auto-detect, which is usually worse for Chinese and better for most other languages. |
| `text_script` | `simplified` | Chinese output script: `simplified` or `preserve`. The default is what the reference user writes; `preserve` keeps whatever script the repair model produced. |
| `max_utterance_seconds` | `60` | A hold whose key never comes up stops recording here. |
| `asr_server_timeout_ms` | `30000` | Deadline for one transcription. |
| `capture_source` | `""` | PipeWire source name; empty uses the default input. |
| `push_to_talk` | `<Control>grave` | **A chord of your choosing.** Held down to dictate; the release transcribes. Needs a modifier, and must not collide with a shortcut the applications you use claim. |

A default configuration is valid: it names no endpoint, and the finding that says so is
`asr_endpoint`, which knows whether the answer is a model on this machine or a provider.

**What is not here, and where it went.** Which audio.cpp runs, which checkpoint it serves,
which backend it was built for and how many workers that deserves are properties of the
machine, not choices, and only matter when the endpoint is this machine: `scripts/asr-service.sh`
works them out once and records them where they are used - the binary in the unit's
`ExecStart`, the checkpoint, family and worker count in the audio.cpp config that unit names, at
`~/.local/share/voiceime/asr-server.json`. Ask the machine, not the file:

```
systemctl --user cat voiceime-asr-server.service      # which audio.cpp runs, and what it was told to serve
curl -s http://127.0.0.1:18080/v1/models             # what is actually being served, and under what id
voiceime doctor --json                                # and whether those files and that binary are still there
```

Things every setting shares: `voiceime config set` writes the file atomically, validates
it, and names the command that puts it in force (`voiceime apply`). Hand-editing is
supported and needs the same command. No step comes first: the file appears with the
first value written, and `voiceime config show` prints what is on record (or the defaults,
with `exists: false`, when there is no file).

A settings file holding only what a person owns is a settings file an agent can write by hand.

## Verifying

```
voiceime doctor --json            # every finding carries a fix; data.checks[].id is stable
voiceime transcribe --seconds 5   # one decode from the microphone, with timing
```

`doctor` is the only state report. Which question is which finding:

| Question | Finding |
|---|---|
| does the endpoint answer, and what would a request name there | `asr_endpoint` |
| is the component installed, and does it name a binary that exists | `component_exec` |
| will the engine still be registered after the next login | `component_env` |
| do this session's keys reach the engine at all | `session_engine` |
| is an old build still running | `running_build` |
| is a model service installed here, and does its unit name an audio.cpp and a config that are both there | `server_unit` |

`server_unit` decides nothing about whether a model here is wanted - the endpoint does - so it
stays silent when the endpoint is a provider and no unit exists. It does speak about the two
cases that cost something: a unit whose recorded paths no longer exist, which restarts forever
in the background, and a valid unit here while the settings post elsewhere, which is a model
loaded into memory that nothing sends audio to.

`asr_endpoint` asks the question the engine asks, in the way the engine asks it: with a model
named in the settings it reports that id and contacts nothing, because a provider's model list
is a catalogue rather than a promise about this account; with nothing named it asks the
endpoint, because that is the only answer, and a local service left on another checkpoint is
the failure that matters. A credential the endpoint refuses is a failing finding, not an ok one.

A failing finding is exit 3 with a `fix` an agent can run: `voiceime apply` for the desktop
half, `systemctl --user restart voiceime-asr-server.service` or `scripts/asr-service.sh` for a
model on this machine.

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
