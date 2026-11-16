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
voiceime config show|get
voiceime doctor                                  # every prerequisite, machine-readable
voiceime transcribe [--file recording.wav | --seconds N]
voiceime capabilities                            # the surface: commands, settings, paths, exit codes
```

Two more names appear in the surface and neither is a command to run by hand:
`ibus-daemon` spawns `voiceime ibus-engine --ibus`, and a model service's unit runs `audio.cpp`
itself, on a config that service's own project wrote. voiceime names neither: it recognizes a
model service by the binary its unit runs (`audiocpp_server`) and reads the rest of that
service's story from the unit itself.

**Recognition is an endpoint.** The engine posts one recording to the address in the settings and
reads the text back; that is the whole of what it asks of the machine. That address is used
exactly as it is written - no route is appended to it, no model is guessed for it - and whether
it is a model on this box or a provider across a network is not the engine's concern and is not
written anywhere in it, so the two halves of a setup are separate and each is optional in its own
way:

```
voiceime apply                        # the desktop half: always needed, whatever answers
asr-service.sh <audiocpp_server> <model.gguf>   # a model on this machine: only if the endpoint is this machine
```

The second command is not part of this program. A model service is a separate project - one
script, its own unit name, its own audio.cpp config - and the only contact between the two is
the two settings lines it prints for this file.

* An endpoint elsewhere needs nothing installed here. The model service's script says so and
  starts nothing when `asr_server_url` is not a loopback address. What such a setup needs is
  in the settings and nowhere else: the address the provider gave you, whole, the `model` id
  that address is asked for, and a credential in `api_key`.
* A model on this machine is one GGUF file, and that script is what puts it where it will be
  served: it asks `audiocpp_gguf --inspect` for the family the file carries, asks the build
  which backends it has and the CPU how many workers fit, writes the two files that service
  consists of, and then prints the address it serves and the id it serves it under - two
  lines for the settings file, which it does not write.
  Nothing is fetched and nothing is scanned here: a checkpoint that is not named is not used.
* `apply` is the whole desktop side, in one command: it writes the component file and the
  search path to `~/.config/environment.d/voiceime.conf`, **appends** `('ibus', voiceime)` to
  GNOME's input-source list (which is what makes the desktop able to select and show the
  engine), restarts IBus, then drops the engine processes and selects the input method again.
  It refuses to change anything if the configuration cannot work. Running it twice is how you
  check it: the second run reports everything unchanged.
* The input-source list belongs to the user. Nothing already configured is removed or
  reordered, and the recent-use order is not written; `remove` takes out voiceime's own
  entry and leaves the others.
* `remove` is `apply`'s opposite: the component file, the `environment.d` file and the input
  source this tool added. A model service on this machine is not part of that install - it runs
  `audio.cpp`, not this program, and it may be answering clients other than this engine - so
  `remove` leaves it running, reports the unit it found, and names the command that stops it
  (`systemctl --user disable --now <unit>`). It leaves the checkpoint alone too: that file was
  never the installation's.
* There is no `server` command at all, and no `server install`, `server restart` or
  `engine install`: one command per half, and `doctor` answers "is it there" with findings.
  Asking the same question through several commands, each with its own idea of what "installed"
  means, is how an install ends up described differently in three places. And nothing that runs
  long re-derives a decision: the service is `audio.cpp` on a written config, so the only code
  that has to know which build this machine has is the script that installs it.

## Settings

| Key | Default | Notes |
|---|---|---|
| `asr_server_url` | `""` | The whole address a recording is posted to, used exactly as it is written: `http://127.0.0.1:18080/v1/audio/transcriptions` for a model on this machine, the provider's own URL for a model across a network. **Nothing is appended to it** - the path is part of the address, and is not guessed at. |
| `model` | `""` | The id named in the request. Nothing asks an address what it serves, so **an address and an id are one decision**: for a model on this machine the script that starts it prints both lines, and a provider's id comes from its documentation. A path in this key is refused: the file belongs to the service, not to the settings. |
| `api_key` | `""` | Sent as `Authorization: Bearer <value>`. Empty sends no header. A credential lives here rather than in an environment the engine never starts - and because it can live here, the file is one you keep readable only by yourself (`chmod 600`); a file other people can read is a `doctor` warning, never a reason to stop working. A wrong key is a failing `asr_endpoint` finding, never a silent fallback. |
| `language` | `zh` | **The user's, not the program's.** Passed to the model; empty asks it to auto-detect, which is usually worse for Chinese and better for most other languages. |
| `text_script` | `simplified` | Chinese output script: `simplified` or `preserve`. The default is what the reference user writes; `preserve` keeps whatever script the repair model produced. |
| `max_utterance_seconds` | `60` | A hold whose key never comes up stops recording here. |
| `asr_server_timeout_ms` | `30000` | Deadline for one transcription. |
| `capture_source` | `""` | PipeWire source name; empty uses the default input. |
| `push_to_talk` | `<Control>grave` | **A chord of your choosing.** Held down to dictate; the release transcribes. Needs a modifier, and must not collide with a shortcut the applications you use claim. |

A default configuration is valid: it names no address and no model id, and the finding that says
so is `asr_endpoint`, which knows whether the answer is a model on this machine or a provider.
Naming one of those two and not the other is a blocking finding of its own: an address is only
usable together with the id to ask it for, and nothing in the program goes looking for one.

**What is not here, and where it went.** Which audio.cpp runs, which checkpoint it serves,
which backend it was built for and how many workers that deserves are properties of the
machine, not choices, and they only matter when the endpoint is this machine. They belong to
the project that installs a model service, and none of that project's names - its unit, its
audio.cpp config - are settings here or constants in this program. What the engine knows is the
binary such a service runs, and everything else it reads out of the unit's own `ExecStart`. Ask
the machine, not a name:

```
voiceime doctor --json                             # names the unit it found, the binary it runs, and the config it was handed
systemctl --user cat <that unit>                   # the same facts, from the unit itself
curl -s http://127.0.0.1:18080/v1/models          # the service's own list of what it has loaded
```

Things every setting shares: one parser reads the file, and it is checked **where it becomes
something that runs** - the engine process, a `transcribe`, an `apply`. A file that reads
correctly and still cannot work - an address with no id beside it - is refused before a
microphone is opened or a request is sent: the key, the reason, and the line that would fix
it in the file it belongs to, all named. An engine does not start at all until it can do
what it is asked.

Nothing writes this file, and nothing needs to. It starts as a copy of `config.example.toml`
in this project; what a person changes after that is theirs. The sample carries every key
with its reason beside it and both worked endpoints commented out, and it is parsed by the
same `Load` the engine calls, by a test that fails when the sample and the defaults drift
apart - so the file a person copies is checked the way the code is. A comment in that file
can hold the endpoint someone switched away from instead of deleting it, which is the whole
reason the file is edited by hand rather than generated: a settings file is where a setup
keeps its reasons. `voiceime config show` prints the path and what is on record (the
defaults, with `exists: false`, when there is no file); `voiceime config get <key>` is the
only reader of one effective value, which is why the model service's script asks for an
address through this program instead of parsing the TOML itself. Neither writes anything, and
`voiceime apply` is what puts a changed value into processes that are already running.

A settings file holding only what a person owns is a settings file a person can keep by
hand - and one an agent can write from scratch, which is what the sample is for.

## Verifying

```
voiceime doctor --json            # every finding carries a fix; data.checks[].id is stable
voiceime transcribe --seconds 5   # one decode from the microphone, with timing
```

`doctor` is the only state report. Which question is which finding:

| Question | Finding |
|---|---|
| does the address answer, is the credential accepted, and is there an id to name in the request | `asr_endpoint` |
| is the component installed, and does it name a binary that exists | `component_exec` |
| will the engine still be registered after the next login | `component_env` |
| do this session's keys reach the engine at all | `session_engine` |
| is an old build still running | `running_build` |
| is a model service installed here, and does its unit name an audio.cpp and a config that are both there | `server_unit` |
| is the file holding a credential readable by other people | `config`, as a warning: it is reported and costs nothing, because nothing about it stops dictation working |

`server_unit` decides nothing about whether a model here is wanted - the endpoint does - so it
stays silent when the endpoint is a provider and no unit exists. It does speak about the two
cases that cost something: a unit whose recorded paths no longer exist, which restarts forever
in the background, and a valid unit here while the settings post elsewhere, which is a model
loaded into memory that nothing sends audio to.

`asr_endpoint` asks the question the engine asks, in the way the engine asks it: is the address
reachable, and would a request to it carry the model id and the credential the settings hold. A
model id missing next to an address is a failing finding whose fix is the key that completes the
pair. The address is contacted either way, because its status is the one thing an answer can
prove about a remote setup: 401 or 403 is the credential being refused, which is a failing finding
whose fix is a settings file. Nothing is ever believed about a model list: with an id named in the
settings, a 404, a 501 and a list of two hundred ids all mean the same nothing, because a
provider's catalogue is not a promise about what this account accepts - and the address is the
engine's whole say about where it posts, so asking somewhere else about it is asking a question
the settings did not ask.

A failing finding is exit 3 with a `fix` an agent can act on, which is either a command or
a line and the file it goes in: `voiceime apply` for the desktop half, `systemctl --user
restart <the unit the server_unit finding names>` for a model service this machine already
has, `write api_key = "…" in <the settings file>` for a credential the address refused,
`write model = "<id>" in <the settings file>` for an address with nothing to ask it for,
`curl -s <address>` for an address that did not answer at all. A fix names the file because
nothing in this program writes it. A model that is not installed yet is the model service's
own project, not a command here.

The engine's own log is `~/.cache/voiceime/engine.log` (ibus-daemon discards the process's
stdout). `ready: hold … to dictate` on selection, then `recording`, `decoded …` and
`committed N character(s)` per hold. The lines that are not routine are the ones worth
reading: `the press before this one never saw its release` (a key event went to another
input context), `discarded the recording in progress` (the engine was deselected mid-hold),
`recording stopped at the …s limit` (the key was never released), `not starting: the
settings cannot work: <key>: <reason> (<fix>)` (the settings the engine was started with
cannot do what they ask, so no engine was started), and `engine process ending`
(how this engine stopped, which matters because no engine process means no input method for
the session).

`voiceime ibus-engine --ibus --audio-file speech.wav --trace /tmp/engine.log` runs the real
path over a file, with no microphone and no keyboard.
