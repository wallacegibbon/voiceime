# Deploy on a new machine

GNOME on Linux, with an audio.cpp build. Nothing needs root: the binary, the component and
both units live in the user's home.

Set `BIN` to the binary for the rest of this page:

```sh
BIN=~/.local/bin/voiceime
```

## 1. Build and install

```sh
make check        # gofmt, vet, tests, unreachable code, doc links
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
"$BIN" config set model ""            # empty: pick the best installed model
"$BIN" config validate --json         # exit 3 when a setting would stop the service
```

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

## 4. The service

```sh
"$BIN" server install
# assert: "serving <path> (family) as <id>" and then "serving at http://127.0.0.1:18080"
"$BIN" server status --json
# assert: data.unit_active == true, data.listening == true, data.served[0].file is the model
```

The unit runs `voiceime server run`, which renders the audio.cpp config from the settings at
every start: changing `model` and re-running `config apply` is the whole update.

Check the decode end to end before touching the desktop:

```sh
"$BIN" transcribe --file sample_16k.wav
# assert: the text, and a took_ms proportional to the recording
```

## 5. The IBus engine

```sh
"$BIN" engine install --apply
# assert: "added ('ibus', 'voiceime') to the GNOME input sources"
# assert: "wrote /home/<user>/.config/environment.d/voiceime.conf"
"$BIN" engine status --json
# assert: data.engine_registered == true, data.engine_visible == true,
#         data.environment_set == true, data.gnome_input_source.listed == true
gsettings get org.gnome.desktop.input-sources sources
# assert: the list contains ('ibus', 'voiceime')
```

`--apply` writes the component, adds its directory to `IBUS_COMPONENT_PATH`, deletes the
registry cache, restarts IBus and appends the input source. The source list is the user's:
the install adds one entry and leaves the rest - their layouts, their other engines, their
recent-use order - exactly as it found them. The input source is the half that
is easy to miss: the desktop's switcher and indicator are drawn from that list, so without
it the engine receives keystrokes but cannot be selected from the menu.

It also writes the search path to `~/.config/environment.d/voiceime.conf`, because the
activation environment it sets above belongs to this session only. `environment.d` is read
when a session starts, so the file is what makes the install survive the next login, while
the activation environment is what makes the engine work now. Without the file the install
looks finished and works until the next login: `ibus-daemon` then starts without
`IBUS_COMPONENT_PATH`, and `engine status` reports the engine as not registered although
nothing was uninstalled (`doctor` says the same as `component_env`).

Run `engine status` a few seconds after `--apply`: the daemon restart is asynchronous, and
`ibus list-engine` fails while it comes up.

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
"$BIN" doctor --json >/dev/null;                                   doctor=$?
"$BIN" server status --json | grep -q '"listening": *true';        server=$?
"$BIN" engine status --json | grep -q '"engine_registered": *true'; engine=$?
"$BIN" engine status --json | grep -q '"listed": *true';           desktop=$?
echo "doctor=$doctor server=$server engine=$engine desktop=$desktop"
```

All four zero is the deployable state. What no command can assert is a human seeing text in
a focused window: dictate one sentence into a text editor.

## Updating an installed copy

```sh
make install
"$BIN" config apply      # restarts the service, drops the engine, converges the component
```

A running process keeps the binary it was started from, so `doctor` reports a stale one as a
`warn` until it is restarted. `config apply` is what restarts them, and it selects the input
method again afterwards: an engine process that goes away takes the session's input method
with it, so without that the person at the keyboard is left unable to type until they pick an
input method themselves.

## Recovery

| Symptom | Cause | Fix |
|---|---|---|
| `doctor` says there is no audio.cpp CLI, or that `asr_bin` names a file that cannot run | audio.cpp was never built, or the build tree moved or was cleaned | `"$BIN" config set asr_bin <path to audiocpp_cli>` (or build it and run `config init --force`), then `config apply` |
| no text, log shows `no audio was recorded` | the sound server is not giving the microphone | `"$BIN" doctor`, then `config set capture_source` |
| no text, log shows `transcribe …: …` | the service refused or timed out | `server status --json`, then `config apply` |
| the desktop still shows the input method, but no application receives any key at all | no engine is selected for the session: an engine process ended, and the daemon clears the global engine when its bus name disappears (`ibus engine` answers `No engine is set.`) | `ibus engine voiceime`, which also starts the engine process (`doctor` reports `session_engine`; `config apply` selects it again too) |
| the menu does not list Voiceime | not in the input-source list | `"$BIN" engine install --apply` |
| `engine status` says not registered | the daemon has not read the component | `"$BIN" engine install --apply`, then re-check after a few seconds |
| the engine disappears after every login | `IBUS_COMPONENT_PATH` was set for one session only, not written to `~/.config/environment.d/voiceime.conf` | `"$BIN" engine install --apply` (`doctor` reports `component_env`) |
