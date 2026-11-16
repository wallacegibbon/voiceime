# Agent guide

Everything here is non-interactive and `--json` on request, so an agent can install,
configure and verify voiceime by reading this file and running commands. The two lists below -
the commands and the settings - are the whole surface, and a test compares them with what the
build actually has, in both directions, because a document an agent follows has to be the program
it is running. What is true of *this machine* is never written here: `voiceime doctor` reports
that, with a fix beside each finding.

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
  `{"command":…,"ok":false,"error":{"kind":…,"message":…,"fix":…},"exit_code":N}` on stderr,
where `fix` is present exactly when there is something to run - which is why the table below
has rows that say "read the message" rather than "run the fix".

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
voiceime doctor                                  # every prerequisite, machine-readable
voiceime transcribe [--file recording.wav | --seconds N]
```

One name appears in the surface that is not a command to run by hand: `ibus-daemon` spawns
`voiceime ibus-engine --ibus`. Nothing else on the machine is named anywhere in the program. It
holds an address, posts one recording to it and reads the text back; what stands behind that
address is not something it can see, and not something it needs to.

**Recognition is an endpoint.** The engine posts one recording to the address in the settings and
reads the text back; that is the whole of what it asks of the machine. That address is used
exactly as it is written - no route is appended to it, no model is guessed for it - and whether
it is a model on this box or a provider across a network is not the engine's concern and is not
written anywhere in it, so the two halves of a setup are separate and each is optional in its own
way:

```
voiceime apply                        # the desktop half: always needed, whatever answers
scripts/asr-service.sh <audiocpp_server> <model.gguf>   # a model on this machine: only if the endpoint is this machine
```

The second is in this project and is not part of this program: a shell script in `scripts/`,
compiled into nothing, run by whoever deploys, from the checkout. It takes the binary, the
checkpoint and the port on its own command line and reads nothing of this program's settings; the
two halves of a setup meet at one string - the address - and neither knows how the other came by
it. It stays out of the program because an engine that knows one address needs no more, and it
says so itself: the unit's name, the audio.cpp config and the checkpoint are in two files it
writes and are not settings here.

* An endpoint elsewhere needs nothing installed here - and nothing here stops a model running
  anyway, because whether one is wanted is not a question this project asks either. What such a
  setup needs is in the settings and nowhere else: the address the provider gave you, whole, the
  `model` id that address is asked for, and a credential in `api_key`.
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
  source this tool added. Nothing else is part of that install, so nothing else is looked at: a
  model on this machine, a provider's address, a checkpoint on a disk - none of them were put
  there by `apply`, and `remove` has no opinion about whether they should still be there.
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
so is `asr_endpoint`, which asks the address one question and nothing about what stands behind
it.
Naming one of those two and not the other is a blocking finding of its own: an address is only
usable together with the id to ask it for, and nothing in the program goes looking for one.

**What is not here, and where it went.** Which audio.cpp runs, which checkpoint it serves,
which backend it was built for and how many workers that deserves are properties of the
machine, not choices, and they only matter when the endpoint is this machine. They belong to
`scripts/asr-service.sh`, and none of that service's names - its unit, its audio.cpp config -
are settings here or constants in this program. The program holds no name for it at all, so
those facts are asked of the machine:

```
systemctl --user cat audiocpp-asr.service          # what a model here was told to serve, from the unit itself
curl -s http://127.0.0.1:18080/v1/models           # the service's own list of what it has loaded
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
keeps its reasons.

Nothing reports them either. There is no command that prints the settings back, because the
file is the record and the only question worth asking about it - can this work? - is answered
where the answer matters: `voiceime doctor`'s `config` finding, the engine refusing to start,
`apply` refusing to change anything. A command that recites the settings is a second place
they would be kept. `voiceime apply` is what puts a changed value into processes that are
already running.

A settings file holding only what a person owns is a settings file a person can keep by
hand - and one an agent can write from scratch, which is what the sample is for.

## Verifying

```
voiceime doctor --json            # every finding; each one that is not ok carries its fix,
                                  # and data.checks[].id is stable
voiceime transcribe --seconds 5   # one decode from the microphone, with timing
```

`doctor` is the only state report. Which question is which finding:

| Question | Finding |
|---|---|
| does the address answer, is the credential accepted, and is there an id to name in the request | `asr_endpoint` |
| is there something to record with, and does PipeWire name an input to record from | `capture` |
| is the bus running, and does it know this engine is there | `ibus` |
| are the notification and sound tools installed | `feedback`, as a warning: without them the status cues are silent, which stops nothing from working |
| is the component installed, and does it name a binary that exists | `component_exec` |
| will the engine still be registered after the next login | `component_env` |
| do this session's keys reach the engine at all | `session_engine` |
| is an old build still running | `running_build` |
| can these settings work at all, is the file readable only by its owner, and if not which line | `config`: a settings file that cannot work is a failure naming the key, the reason and the line that would fix it, and the engine refuses to start on the same list, so a file that cannot work never reaches a microphone; a credential in a file other people can read is a warning, reported and costing nothing, because nothing about it stops dictation working |

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
a line and the file it goes in: `voiceime apply` for the desktop half, `write api_key = "…" in
<the settings file>` for a credential the address refused, `write model = "<id>" in <the settings
file>` for an address with nothing to ask it for, `write asr_server_url = "…" in <the settings
file>` for an address that is not written yet, `curl -s <address>` for an address that did not
answer at all. A fix names the file because nothing in this program writes it. An address that
did not answer is reported as an address that did not answer: what should have been listening
behind it is not something this program was told, so it names no command that would have started
it.

The engine's own log is `~/.cache/voiceime/engine.log`: one file per engine lifetime, every
line prefixed `session:`, because ibus-daemon discards the process's stdout. What a working hold
writes, and everything that can go wrong instead:

| The line begins | What it means |
|---|---|
| `server <address> (model <id>)` | once per engine: the address and the id this process will use, exactly as the settings hold them. It says nothing about whether anything is listening, and `asr_endpoint` is the finding that does |
| `ready: hold … to dictate` | this input context is armed. Selecting the engine records nothing |
| `recording` | the trigger was pressed |
| `diagnostic: dictating` | this process was started with `--audio-file`: it dictated that file instead of waiting for a key |
| `cannot dictate` | the same run could not read the file it was told to dictate |
| `decoded … of audio in …` | the answer came back, and the line carries the text - which is why a decode that never reached the screen is still here |
| `committed … character(s)` | the text was typed at the caret |
| `not starting: the settings cannot work:` | the settings this process was started with cannot do what they ask, so no engine was started; the key, the reason and the line that would fix it are in the line |
| `engine process ending:` | how this engine stopped, which matters because no engine process means no input method for the session |
| `cannot record:` | the capture client would not start - no microphone, or a `capture_source` naming no device |
| `capture ended:` | the stream died part-way through a hold |
| `no audio was recorded; nothing was sent` | the stream produced nothing, so there was no request to make |
| `capture dropped` | the capture client fell behind and audio is missing from what was sent; the line says how much |
| `transcription queue full;` | dictation came faster than the decoder could take it, and that recording was dropped rather than queued without bound |
| `recording stopped at the` | the key was never released, so the hold was cut at the `max_utterance_seconds` limit |
| `discarded the recording in progress` | the engine was deselected while the key was still down: the text would have had no caret to go to |
| `discarded the recording: the engine is no longer selected` | the same, noticed at the release |
| `discarded the recording: the key was released before it started` | the press never reached this input context |
| `the press before this one never saw its release; ending that recording and starting a new one` | a release went to another input context while a recording was still open. The audio up to here was spoken as part of the same dictation, so it is sent rather than dropped, and a new recording starts |
| `the press before this one never saw its release` | the same lost release, noticed on a press that found no recording open - it had already ended at the `max_utterance_seconds` cap, or never started. Only the belief that the key was down is cleared |
| `transcribe` | the request failed - the address refused it, the `asr_server_timeout_ms` deadline passed, the id names nothing that answers there - and the rest of the line is the reason. Nothing is typed |
| `the model heard no words in` | an empty answer: the audio went in and came back as nothing |
| `the model answered only punctuation` | an answer carrying no letter or a digit, which is what a recogniser returns for audio it could not read; nothing is typed |
| `deliver` | the text was decoded and could not be committed - which is why a `decoded` line with no `committed` line after it still means nothing appeared at the cursor |

A test compares this table with the calls that write that log, in both directions: a line the
engine can write and the table does not name, or a row naming a line this build cannot write,
fails `make check`.
the session).

`voiceime ibus-engine --ibus --audio-file speech.wav --trace /tmp/engine.log` runs the real
path over a file, with no microphone and no keyboard.
