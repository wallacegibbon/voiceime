# Deploy on a new machine

GNOME on Linux. Nothing needs root: everything this project writes - the binary, the engine's
component file, a model service - lives in the user's home.

Set `BIN` to the binary for the rest of this page:

```sh
BIN=~/.local/bin/voiceime
```

## 1. Build and install

```sh
make check        # gofmt, vet, tests, unreachable code, unused code, doc links
make install      # ~/.local/bin/voiceime, ~/.local/docs
"$BIN" doctor
```

`doctor` on a fresh checkout reports what is missing, each finding with the command that
fixes it. The rest of this page is those commands, in order.

## 2. Configuration

```sh
"$BIN" config show
```

`config show` prints the file's path and every setting with the value on record. On a fresh
machine there is no file yet: it prints the defaults and says `exists: false`. Nothing needs
to create it - the first `config set`, and `apply` below, write it.

What is in that file belongs to the person using it: `language`, `text_script`,
`push_to_talk`, and the safety limit on one recording. The defaults are one machine's answers
- Simplified Chinese, `` Ctrl+` `` - so change what does not fit. What each one is for is the
settings table in [AGENT.md](AGENT.md):

```sh
"$BIN" config set language en
```

A setting takes effect when the process that reads it next starts: the engine picks up a new
key when it is next spawned, and a model service when it is next started. Nothing about the
machine is asked here - which audio.cpp to run, which backend it was built for, how many
workers it deserves - because those are answered by looking at the machine once, in the step
that starts a model. What a wrong setting would do is `doctor`'s `config` finding, which names
the setting and the command that corrects it.

## 3. Where recognition comes from

The engine needs one thing: an address that answers a POST with the recording. That address is
used exactly as it is written, so a provider whose route is `/api/paas/v4/audio/transcriptions`
and a model on this machine at `/v1/audio/transcriptions` are the same kind of setting. The
settings say which of the two a machine is in - `asr_server_url` and the `model` id to name, plus
`api_key` when the address asks for a credential.

### An endpoint elsewhere

Nothing is installed here. Write the three keys and skip the rest of this section:

```toml
asr_server_url = "https://api.example.com/v1/audio/transcriptions"  # the whole address: nothing is appended
model          = "gpt-4o-mini-transcribe"    # what to ask for, by id
api_key        = "sk-…"                      # sent as Authorization: Bearer
```

`asr_endpoint` is the finding that says whether that address can be used at all: it reports an
address with no id to ask it for, and it asks the address itself whether the credential is
accepted - a refused credential is a failing finding rather than an ok one, and a wrong token and a
missing token are reported differently, with the address's own reason kept. The file holds that
token, so `config set` writes it `0600`; hand-editing it is fine and needs nothing extra. The
proof is the same command as ever:

```sh
"$BIN" transcribe --file sample_16k.wav
```

### A model on this machine

voiceime fetches nothing and scans nothing: a checkpoint is one GGUF file, and you say which
one. What it is - the audio.cpp family that picks the loader - is inside it, and audio.cpp's
own tool reads it out:

```sh
find ~ -maxdepth 4 -name '*.gguf' -size +1M
# assert: the checkpoints this machine already holds

~/playground/audio.cpp/build/*/bin/audiocpp_gguf --inspect <that file>
# assert: model_spec_family=<the family>, in milliseconds; a checkpoint audio.cpp never
#         produced says nothing on that line, and is not a model voiceime can serve
```

Build audio.cpp (`cmake -B build && cmake --build build`), then one command turns a
checkpoint into a service:

```sh
scripts/asr-service.sh \
  ~/playground/audio.cpp/build/linux-cpu-release/bin/audiocpp_server \
  ~/models/SenseVoice-Small-GGUF/sensevoice-small-q8-audiocpp-v1.gguf
# assert: wrote:  /home/<user>/.local/share/voiceime/asr-server.json
#         wrote:  /home/<user>/.config/systemd/user/voiceime-asr-server.service
#         serving: sensevoice-small-q8-audiocpp-v1 (cpu, 8 threads) from <the checkpoint>
#         settings: asr_server_url = http://127.0.0.1:18080/v1/audio/transcriptions, model = sensevoice-small-q8-audiocpp-v1
```

The last two lines are the assertion, and the first half of it is the same request anyone can
repeat afterwards with no tool of ours:

```sh
curl -s http://127.0.0.1:18080/v1/models
# assert: data[0].id is the entry named above
```

That is the service's own list, not something the engine reads: the engine posts to the address
and names the id the script just recorded.

Four things about that script belong here:

* It writes two files and two settings keys, and starts them. The audio.cpp to run, the backend
  its own banner reports, the worker count the CPU's topology allows (one per physical core:
  `nproc` counts siblings and would ask the engine to queue half its threads) and the checkpoint go
  into those files; the address and the id go into the settings, because a program that never asks
  an address what it serves has to be told. Nothing looks any of them up again - a service started
  at login cannot fail for a reason only another run could fix.
* It asks, rather than assumes: the family from the checkpoint, the backend from the build, the
  cores from the kernel. A build without `cpu` in its banner is refused by name, and a
  checkpoint with no model spec in it is refused by name - the answer is to convert it with
  audio.cpp, not to point voiceime at it harder.
* It is idempotent, and says what it changed: running it again after editing a checkpoint into
  the settings prints `unchanged:` twice, and rewriting the unit is what heals a moved
  checkout or a newer build.
* It starts nothing when the address is not this machine: `asr_server_url = "https://…"` makes it
  report that and leave, which is the whole of "the local model is optional".

The unit is where those answers ended up, so reading it back is the assertion that they were
written down at all:

```sh
systemctl --user cat voiceime-asr-server.service
# assert: ExecStart=<the audio.cpp named above> --config
#         /home/<user>/.local/share/voiceime/asr-server.json --log
#         - and no voiceime in that line: nothing re-derives the answer at start-up
```

## 4. The desktop half: `voiceime apply`

One command makes the engine reachable: the IBus component file, the search path a login needs,
the desktop's input source, and the running engine processes. It is idempotent - a second run
reports everything unchanged - and it refuses halfway: a configuration that cannot work is not
applied at all. It does not start a model: that is the section above, and a setup whose
endpoint is a provider has no model here to start.

```sh
"$BIN" apply
# assert: component: /home/<user>/.local/share/voiceime/component/voiceime.xml (written);
#                    IBus restarted
#         desktop:   appended ('ibus', 'voiceime') to the GNOME input sources; the sources
#                    already configured are untouched
#         engine:    was not running
```

What it writes, and why each of those files is needed, is [AGENT.md](AGENT.md); the
measurements behind the search path and the component registry cache are in
[ibus-protocol.md](ibus-protocol.md). Two things about running it belong here:

* It restarts IBus, and that is asynchronous: `apply` waits for `ibus list-engine` to answer
  before it reads or sets the session's engine, which is why the desktop assertion below is
  worth running a second later rather than immediately.
* It ends by dropping the engine processes, so that the next activation runs the new binary,
  and selecting the input method again: an engine process that goes away takes the session's
  input method with it, and nothing puts it back by itself.

The input-source list is the user's. `apply` adds one entry and leaves the rest - their
layouts, their other engines, their recent-use order - exactly as it found them. Assert it:

```sh
gsettings get org.gnome.desktop.input-sources sources
# assert: the list contains ('ibus', 'voiceime')
```

## 5. The recognition path, without a keyboard

```sh
"$BIN" transcribe --file sample_16k.wav
# assert: the text, and a took_ms proportional to the recording
```

This is the same request the engine makes - one WAV, one decode - so it proves the endpoint,
the credential and the audio format at once, with no IBus and no keyboard involved.

A recording longer than the served model's encoder holds is refused in the model's own words
(`500 … SenseVoice encoder input exceeds positional capacity`); the numbers measured against
the default checkpoint are in [measurements.md](measurements.md).

An empty result is a different thing from a broken path, and says so: a recording with no
speech in it prints a line giving its length and the decode's milliseconds, and still exits
0. `--json` has always said the same thing with `"text": ""` and the timing; the line is
there because this command is what a deploy runs to prove the path, and an empty stdout with
exit 0 reads as a crash.

## 6. Use it

Super+Space (or the top-right menu) selects **English (Voice)**. The engine declares the
US keyboard layout, so with Voiceime selected the keyboard types English and speech is
transcribed: the separate English source can be deleted, leaving Voiceime and Pinyin. Hold `` Ctrl+` ``, say a
sentence, let go: the text appears at the caret. Super+Space goes back to the previous input
method.

Selecting the engine records nothing. `~/.cache/voiceime/engine.log` is the engine's account
of a hold: `ready: hold <Control>grave to dictate`, then `recording`, `decoded …` and
`committed N character(s)`.

## 7. Acceptance, in one command

```sh
"$BIN" doctor --json >/tmp/voiceime-doctor.json; doctor=$?
for id in asr_endpoint capture ibus component_exec component_env session_engine running_build; do
  grep -q "\"id\":\"$id\",\"status\":\"ok\"" /tmp/voiceime-doctor.json && echo "$id ok" || echo "$id not ok"
done
```

`doctor=0` and seven lines ending in `ok` is the deployable state: an endpoint that answers
and would be named correctly in a request, capture available, the engine registered and
selected for the session, and nothing stale running. A machine that starts its own model
asserts one more - `server_unit`, the record `scripts/asr-service.sh` wrote. What no command
can assert is a human seeing text in a focused window: dictate one sentence into a text
editor.

## Removing an installation

```sh
"$BIN" remove
```

Stops the engine, and deletes the component file, the `environment.d` file and the
input-source entry this tool added - and the model unit and the audio.cpp config it generated,
which `scripts/asr-service.sh` wrote but which are this project's, so an uninstall that left
them would leave a model loaded. The user's other input methods are left alone, the checkpoint
file is left alone, and so is `config.toml`: what was configured may be an endpoint someone
pays for, and an uninstall is not permission to delete what they wrote.

## Updating an installed copy

```sh
make install
"$BIN" apply
```

A running process keeps the binary it was started from, so `doctor` reports a stale one as a
`warn` (`running_build`) until it is restarted. `apply` is what restarts them, and it selects
the input method again afterwards, for the reason given in §4. A rebuilt **audio.cpp** is a
different pair of commands: running `scripts/asr-service.sh` again rewrites the unit with the
build that exists now, and `systemctl --user cat voiceime-asr-server.service` shows which one
that is.

## Recovery

| Symptom | Cause | Fix |
|---|---|---|
| `asr_endpoint` fails with `no address is configured` | `asr_server_url` is empty, and recognition is an address | write the provider's URL, or run `scripts/asr-service.sh` with a checkpoint (this machine) |
| `asr_endpoint` fails with `nothing names the model to ask it for` | an address was written without the id it is asked for | `voiceime config set model <id>`, or run `scripts/asr-service.sh`, which writes both |
| `asr_endpoint` fails with a connection error and `server_unit` says the unit runs a binary that does not exist | the audio.cpp build was moved, cleaned or never built | `scripts/asr-service.sh /path/to/audiocpp_server /path/to/model.gguf`, which rewrites both files |
| `server_unit` warns that a model starts here while the settings post elsewhere | the endpoint moved to a provider and the local model stayed loaded | `systemctl --user disable --now voiceime-asr-server.service` |
| no text, log shows `no audio was recorded` | the sound server is not giving the microphone | `"$BIN" doctor`, then `config set capture_source` |
| no text, log shows `transcribe …: …` | the endpoint refused, timed out, or the model id is wrong | `"$BIN" doctor` (the `asr_endpoint` finding), then `"$BIN" transcribe --file sample_16k.wav` |
| the desktop still shows the input method, but no application receives any key at all | no engine is selected for the session: an engine process ended, and the daemon clears the global engine when its bus name disappears (`ibus engine` answers `No engine is set.`) | `ibus engine voiceime`, which also starts the engine process (`doctor` reports `session_engine`; `apply` selects it again too) |
| the menu does not list Voiceime | not in the input-source list | `"$BIN" apply` |
| `doctor` reports `ibus` or `component_exec` as not registered | the daemon has not read the component | `"$BIN" apply`, then `"$BIN" doctor` again |
| the engine disappears after every login | `IBUS_COMPONENT_PATH` was set for one session only, not written to `~/.config/environment.d/voiceime.conf` | `"$BIN" apply` (`doctor` reports `component_env`) |
