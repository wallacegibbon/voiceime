# Deploy on a new machine

GNOME on Linux, with an audio.cpp build. Nothing needs root: the binary, the component and
both units live in the user's home.

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
"$BIN" config init
"$BIN" config show --json | head
```

`config init` writes `~/.config/voiceime/config.toml`, points `asr_bin` at the built
`audiocpp_cli` it finds (build audio.cpp first — nothing else searches for it, ever again),
and lists the settings whose default is a property of the machine
(`asr_bin`, `threads`, `backend`, `language`, `text_script`, `push_to_talk`). Set them now:

```sh
"$BIN" config set threads 8
```

A setting takes effect when the next `apply` (below) reads the file, so nothing else is run
here. What a wrong setting would do is `doctor`'s `config` finding, which names the setting
and the command that corrects it.

## 3. A model

voiceime fetches nothing and scans nothing: a model is one GGUF file, and you name it.

```sh
"$BIN" model
# assert: exit 3, "no checkpoint is named", and the fix `config set model /path/to/model.gguf`
```

Fetch one - the answer above names a checkpoint worth starting with and what it costs on disk
(SenseVoice small, 242 MiB, is what the reference machine uses). Then name it:

```sh
"$BIN" config set model ~/models/SenseVoice-Small-GGUF/sensevoice-small-q8-audiocpp-v1.gguf
"$BIN" model --json
# assert: data.family is the audio.cpp family ("sense_asr"), data.size_bytes is the file's
# size, and data.tasks contains "asr"
```

`model` reads that out of the checkpoint's own metadata, which audio.cpp writes into every
GGUF it produces. Nothing is loaded, no other program is asked, and there is no cache to
refresh: the answer is the same a year later, or the answer is that the file is not one of
audio.cpp's. That second case says so by name - `is a GGUF (whisper) with no audio.cpp model
spec in it` - and what to do about it is to convert the checkpoint with audio.cpp, not to
point voiceime at it harder.

## 4. Make the machine match the file: `voiceime apply`

One command installs and starts everything: the recognition service, the IBus component, the
search path a login needs, and the desktop's input source. It is idempotent - a second run
reports everything unchanged - and it refuses halfway: a configuration that cannot work is
not applied at all.

```sh
"$BIN" apply
# assert: service:   installed and started (voiceime-asr-server.service)
#         serving:   sensevoice-small-q8-audiocpp-v1.gguf
#         component: /home/<user>/.local/share/voiceime/component/voiceime.xml (written);
#                    IBus restarted
#         desktop:   appended ('ibus', 'voiceime') to the GNOME input sources; the sources
#                    already configured are untouched
#         engine:    was not running
```

One command because the work is one decision: the installed machine should look like the
configuration. What it writes, and why each of those files is needed, is
[AGENT.md](AGENT.md); the measurements behind the search path and the component registry
cache are in [ibus-protocol.md](ibus-protocol.md). Three things about running it belong
here:

* It waits until the configured checkpoint is resident. A service that is started but still
  loading, or still serving the previous model, has not applied anything.
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
for id in asr_server component_exec component_env session_engine running_build; do
  grep -q "\"id\":\"$id\",\"status\":\"ok\"" /tmp/voiceime-doctor.json && echo "$id ok" || echo "$id not ok"
done
```

`doctor=0` and five lines ending in `ok` is the deployable state: the model resident, the
component installed and still installed after the next login, keys reaching the engine, and
nothing stale running. What no command can assert is a human seeing text in a focused window:
dictate one sentence into a text editor.

## Removing an installation

```sh
"$BIN" remove
```

Stops the service and the engine, and deletes the unit, the component file, the
`environment.d` file and the input-source entry this tool added. The user's other input
methods are left alone, and so is the model file: `model` names a file you own, and removing
an installation does not delete it.

## Updating an installed copy

```sh
make install
"$BIN" apply
```

A running process keeps the binary it was started from, so `doctor` reports a stale one as a
`warn` (`running_build`) until it is restarted. `apply` is what restarts them, and it selects
the input method again afterwards, for the reason given in §4.

## Recovery

| Symptom | Cause | Fix |
|---|---|---|
| `doctor` says there is no audio.cpp CLI, or that `asr_bin` names a file that cannot run | audio.cpp was never built, or the build tree moved or was cleaned | `"$BIN" config set asr_bin <path to audiocpp_cli>` (or build it and run `config init --force`), then `"$BIN" apply` |
| no text, log shows `no audio was recorded` | the sound server is not giving the microphone | `"$BIN" doctor`, then `config set capture_source` |
| no text, log shows `transcribe …: …` | the service refused or timed out | `"$BIN" doctor` (the `asr_server` finding), then `"$BIN" apply` |
| the desktop still shows the input method, but no application receives any key at all | no engine is selected for the session: an engine process ended, and the daemon clears the global engine when its bus name disappears (`ibus engine` answers `No engine is set.`) | `ibus engine voiceime`, which also starts the engine process (`doctor` reports `session_engine`; `apply` selects it again too) |
| the menu does not list Voiceime | not in the input-source list | `"$BIN" apply` |
| `doctor` reports `ibus` or `component_exec` as not registered | the daemon has not read the component | `"$BIN" apply`, then `"$BIN" doctor` again |
| the engine disappears after every login | `IBUS_COMPONENT_PATH` was set for one session only, not written to `~/.config/environment.d/voiceime.conf` | `"$BIN" apply` (`doctor` reports `component_env`) |
