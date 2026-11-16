# Deploy on a new machine

GNOME on Linux. Nothing needs root: everything this project writes - the binary, the engine's
component file, a model service - lives in the user's home.

Set `BIN` to the binary for the rest of this page:

```sh
BIN=~/.local/bin/voiceime
```

## 1. Build and install

```sh
make check        # gofmt, vet, tests, unreachable code, unused code, docs, scripts
make install      # ~/.local/bin/voiceime, ~/.local/docs
"$BIN" doctor
```

`doctor` on a fresh checkout reports what is missing, each finding with the command that
fixes it. The rest of this page is those commands, in order.

## 2. Configuration

```sh
cp config.example.toml ~/.config/voiceime/config.toml
```

The settings are one file, and the only thing that writes it is whoever edits it. The sample
in this project is that file with every key present and its reason beside it; copy it and
change what is yours. Nothing prints the settings back: the file is the record, and the only
question about it that has an answer - can this work, and if not which line - is `doctor`'s
`config` finding.

What belongs to the person using it: `language`, `text_script`, `push_to_talk`, and the
safety limit on one recording. The sample's defaults are one machine's answers - Simplified
Chinese, `` Ctrl+` `` - so change what does not fit. What each key is for is the settings
table in [AGENT.md](AGENT.md), and in the sample itself.

A setting takes effect when the process that reads it next starts: the engine picks up a new
key when it is next spawned, which is what `voiceime apply` does. Nothing outside this program
reads this file - the model service in §3 runs from two files of its own - and nothing about the
machine is asked here: which audio.cpp to run, which backend it was built for, how many workers
it deserves are answered by looking at the machine once, in the step that starts a model. A file
that reads correctly but
cannot work is `doctor`'s `config` finding, which names the key, the reason, and the line
that would fix it; the same refusal stops `apply`, `transcribe` and the engine itself before
any audio or request is involved.

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
token, so keep it readable only by you (`chmod 600`); `voiceime doctor` says so as a warning
when it is not. The proof is the same command as ever:

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
#         produced says nothing on that line, and is not a model a service here can serve
```

Build audio.cpp (`cmake -B build && cmake --build build`). A model service is one script in
this project - `scripts/asr-service.sh`, in the same checkout, run from it - and one command
turns a checkpoint into a running service:

```sh
scripts/asr-service.sh \
  ~/playground/audio.cpp/build/linux-cpu-release/bin/audiocpp_server \
  ~/models/SenseVoice-Small-GGUF/sensevoice-small-q8-audiocpp-v1.gguf
# assert: wrote:     /home/<user>/.local/share/audiocpp-asr/serve.json
#         wrote:     /home/<user>/.config/systemd/user/audiocpp-asr.service
#         serving: sensevoice-small-q8-audiocpp-v1 (cpu, 8 threads) from <the checkpoint>
#         write these two lines in /home/<user>/.config/voiceime/config.toml:
#           asr_server_url = "http://127.0.0.1:18080/v1/audio/transcriptions"
#           model          = "sensevoice-small-q8-audiocpp-v1"
```

The last three lines are the assertion. The service writes the two files it owns and prints the
two lines the settings need; it does not touch the settings file, which is a person's. The
first half of that proof is a request anyone can repeat afterwards with no tool of ours:

```sh
curl -s http://127.0.0.1:18080/v1/models
# assert: data[0].id is the entry named above
```

That is the service's own list, not something the engine reads: the engine posts to the address
in the settings and names the id the settings name - which is why the two lines above are
worth writing down rather than remembering.

Four things about that service belong to this deploy:

* It writes two files, prints two settings lines, and starts the service. The audio.cpp to
  run, the backend its own banner reports, the worker count the CPU's topology allows (one per
  physical core: `nproc` counts siblings and would ask the server to queue half its threads)
  and the checkpoint go into those files; the address and the id are printed for the settings
  file, because a program that never asks an address what it serves has to be told - and the
  file it is told in is a person's, not this script's. Nothing looks any of them up again - a
  service started at login cannot fail for a reason only another run could fix.
* The two files it writes are the whole of the service, and they live where a login can read
  them - `~/.local/share/audiocpp-asr/serve.json` and `~/.config/systemd/user/audiocpp-asr.service`,
  not a runtime directory emptied at logout. The unit is a user one, so it needs no root and it
  dies with the user's session. Whether it therefore starts at boot or only at login is
  lingering, and the script says which this machine is instead of changing it:
  `loginctl enable-linger <user>` moves every service that user owns, which is their decision.
* It asks, rather than assumes: the family from the checkpoint, the backend from the build, the
  cores from the kernel. A build without `cpu` in its banner is refused by name, a
  checkpoint with no model spec in it is refused by name, and two checkpoints that would be
  served under one id are refused before anything is written - the answer in each case is to
  convert, rebuild or rename, not to point the service at it harder.
* It is idempotent, and says what it changed: running it again with nothing changed prints
  `unchanged:` twice, and rewriting the unit is what heals a moved checkout or a newer build.

Whether a machine wants a model at all is not something either half decides: this script starts
one because it was run, and the client posts to whatever address its settings hold. The two meet
at one string - the address - and nothing else.

Switching among the ids it served is not a redeploy: it is editing the one `model` line in the
settings file and running `voiceime apply`, which is why those two lines are printed rather than
written. What the first request to a model that is not loaded costs is measured in
[measurements.md](measurements.md#what-starting-a-model-costs).

None of those three names - the unit, the audio.cpp config, the checkpoint - is a voiceime
setting, and none is a path this program knows. It has no name for this service at all: the
assertion below is made of the unit and the address, not of a `doctor` finding.

The unit is where those answers ended up, so reading it back is the assertion that they were
written down at all:

```sh
systemctl --user cat audiocpp-asr.service
# assert: ExecStart=<the audio.cpp named above> --config
#         /home/<user>/.local/share/audiocpp-asr/serve.json --log
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
selected for this session, and nothing stale running. The two findings not in that list are
the two whose warning is never the reason something failed: `config` warns about a setting
worth changing and about a credential in a file other people can read, `feedback` about a
notification sound that is not installed. A settings file that is actually wrong makes the
first line fail by itself, and a silent cue stops nothing working.
What a model behind that endpoint is doing is not in the list, and no `doctor` finding can
put it there: a machine that runs its own model asserts it separately, with `systemctl
--user status` and the `curl` above. What no command can assert is a human seeing text in a
focused window: dictate one sentence into a text editor.

## Removing an installation

```sh
"$BIN" remove
```

Stops the engine, and deletes the component file, the `environment.d` file and the
input-source entry this tool added. Nothing else is part of that install, so nothing else is
looked at: what answers the address may be a model this machine runs or a provider someone pays
for, and `remove` has no name for either and no opinion about either. The user's other input
methods are left alone, and so is `config.toml`: what was configured is what a person wrote, and
an uninstall is not permission to delete it.

## Updating an installed copy

```sh
make install
"$BIN" apply
```

A running process keeps the binary it was started from, so `doctor` reports a stale one as a
`warn` (`running_build`) until it is restarted. `apply` is what restarts them, and it selects
the input method again afterwards, for the reason given in §4. A rebuilt **audio.cpp** is a
different pair of commands, and they belong to the model service rather than to this program:
running `scripts/asr-service.sh` again rewrites that unit with the build that exists now, and
`systemctl --user cat audiocpp-asr.service` shows which one it points at now.

## Recovery

| Symptom | Cause | Fix |
|---|---|---|
| `asr_endpoint` fails with `no address is configured` | recognition is an address, and none is written | the finding names the line: `asr_server_url = "…"`, whole, path included - a provider's URL, or the address `scripts/asr-service.sh` prints when it starts a model on this machine |
| `asr_endpoint` fails with `nothing names the model to ask it for` | an address was written without the id it is asked for | the finding names the line: `model = "<id>"`, which is what `scripts/asr-service.sh` prints beside the address, and what a provider's documentation gives |
| `asr_endpoint` fails with `cannot be reached` | nothing is listening at that address, and the program was never told what should be | if the address is meant to be a model here: `systemctl --user status audiocpp-asr.service`, and `scripts/asr-service.sh /path/to/audiocpp_server /path/to/model.gguf` to write both files again if the build moved; if it is meant to be a provider: read the address back out of the settings file, because that is the only place it is written |
| a model is loaded into memory here that nothing sends audio to | the settings post to a provider and a local model was left running | `systemctl --user disable --now audiocpp-asr.service` - no voiceime command reports this, because no voiceime command knows a model was ever meant to be here |
| no text, log shows `no audio was recorded` | the sound server is not giving the microphone | `"$BIN" doctor`, then the `capture_source` line it names |
| no text, log shows `transcribe …: …` | the endpoint refused, timed out, or the model id is wrong | `"$BIN" doctor` (the `asr_endpoint` finding), then `"$BIN" transcribe --file sample_16k.wav` |
| `"$BIN" transcribe` answers `the settings cannot work: <key>: …` and exits 3, and no key reaches the engine | the file parses, but the values do not work as a pair | the message names the key and the line that completes it, in the file that holds the rest of the pair; the engine logs the same reason in `~/.cache/voiceime/engine.log` and stays down until the file works |
| the desktop still shows the input method, but no application receives any key at all | no engine is selected for the session: an engine process ended, and the daemon clears the global engine when its bus name disappears (`ibus engine` answers `No engine is set.`) | `ibus engine voiceime`, which also starts the engine process (`doctor` reports `session_engine`; `apply` selects it again too) |
| the menu does not list Voiceime | not in the input-source list | `"$BIN" apply` |
| `doctor` reports `ibus` or `component_exec` as not registered | the daemon has not read the component | `"$BIN" apply`, then `"$BIN" doctor` again |
| the engine disappears after every login | `IBUS_COMPONENT_PATH` was set for one session only, not written to `~/.config/environment.d/voiceime.conf` | `"$BIN" apply` (`doctor` reports `component_env`) |
