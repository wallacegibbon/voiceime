# voiceime

Speech input for GNOME on Linux, built on
[audio.cpp](https://github.com/0xShug0/audio.cpp).

Select **Voiceime** as an input source, hold `` Ctrl+` ``, speak, let go. The recording is
recognised and the text is inserted at the caret.

**No GUI.** The configuration file is the single source of truth and every action is a
subcommand with machine-readable output, so an agent can install, configure and repair it
without a human.

## How it works

```
microphone ──► capture (parec, 16 kHz mono) ── hold ── release
                                                          │
                             POST /v1/audio/transcriptions ▼
                             one WAV, one decode → commit → IBus
```

* Recognition is an endpoint. What answers that request is a separate decision: a model on
  this machine (`scripts/asr-service.sh <audiocpp_server> <model.gguf>` — one GGUF, one
  systemd user unit) or a provider across a network (three settings). The engine knows only
  the address, and which model id to name.
* Selecting the engine records nothing. A dictation starts on the key press and ends on
  its release; the whole recording is sent, unchanged.
* Switching input methods is the desktop's own: Voiceime is in GNOME's input-source list,
  so Super+Space and the top-right menu select it and show it.

## Install

```sh
make install                     # ~/.local/bin, no root needed

# recognition from an endpoint elsewhere — the whole address, and nothing else installed here:
voiceime config set asr_server_url https://api.example.com/v1/audio/transcriptions
voiceime config set model          gpt-4o-mini-transcribe
voiceime config set api_key        sk-…

# recognition from a model on this machine — two arguments; the script records the address and
# the model id for you, and starts it:
scripts/asr-service.sh \
  ~/playground/audio.cpp/build/linux-cpu-release/bin/audiocpp_server \
  ~/models/SenseVoice-Small-GGUF/sensevoice-small-q8-audiocpp-v1.gguf

voiceime apply                   # register the engine with IBus, add the input
                                 # source, record the search path a login needs
voiceime doctor                  # every finding carries a fix
```

Nothing fetches a model, nothing searches the disk for one, and `doctor` checks the endpoint
by asking it the question the engine will. [docs/DEPLOY.md](docs/DEPLOY.md) is the same
procedure with what to assert at each step.

## Use

| | |
|---|---|
| switch to Voiceime | Super+Space, or the top-right input-source menu |
| dictate | hold `` Ctrl+` ``, speak, release |
| switch back | Super+Space |

Voiceime carries the US keyboard layout, so while it is selected the keyboard types
English and speech is transcribed: it can replace the separate English input source, and
Super+Space then toggles between Voiceime and Pinyin.

`voiceime transcribe --file speech.wav` runs the same request over a recording and prints
the text with its timing.

## Where to go

| If you want to... | Read |
|---|---|
| deploy on a new machine, step by step | [docs/DEPLOY.md](docs/DEPLOY.md) |
| drive it from a script: commands, exit codes, settings | [docs/AGENT.md](docs/AGENT.md) |
| what goes over the socket, and how it fails | [docs/asr.md](docs/asr.md) |
| the IBus engine protocol, as measured | [docs/ibus-protocol.md](docs/ibus-protocol.md) |
| measured numbers | [docs/measurements.md](docs/measurements.md) |

The runtime is authoritative: `voiceime capabilities --json` prints the command and setting
surface, `voiceime doctor --json` the state of this machine.

## Build

```sh
make build     # bin/voiceime
make check     # gofmt, vet, tests, unreachable code, unused code, doc links
make install   # ~/.local/bin and ~/.local/docs
```

Capture is a subprocess and recognition is one HTTP request, so there is no cgo, no audio
headers and no toolchain per platform. Go 1.24 or newer — and audio.cpp only if the model
runs on this machine.
