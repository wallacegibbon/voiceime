# voiceime

Speech input for GNOME on Linux, built on
[audio.cpp](https://github.com/0xShug0/audio.cpp).

Select **Voiceime** as an input source, hold `` Ctrl+` ``, speak, let go. The recording is
transcribed by a resident model and the text is inserted at the caret.

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

* One model, served by one systemd user unit (`voiceime server install`). The entry is
  published under the model file's own name, so nothing is configured twice.
* Selecting the engine records nothing. A dictation starts on the key press and ends on
  its release; the whole recording is sent, unchanged.
* Switching input methods is the desktop's own: Voiceime is in GNOME's input-source list,
  so Super+Space and the top-right menu select it and show it.

## Install

```sh
make install                     # ~/.local/bin, no root needed
voiceime config init             # write defaults, discover audio.cpp
voiceime models install qwen3_asr_0_6b_q8_0
voiceime models list --refresh   # the only command that identifies models
voiceime server install          # serve the model, resident
voiceime engine install --apply  # register the IBus engine and the input source, and
                                 # record the search path so a login keeps them
voiceime doctor                  # every finding carries a fix
```

[docs/DEPLOY.md](docs/DEPLOY.md) is the same procedure with what to assert at each step.

## Use

| | |
|---|---|
| switch to Voiceime | Super+Space, or the top-right input-source menu |
| dictate | hold `` Ctrl+` ``, speak, release |
| switch back | Super+Space |

Voiceime carries the US keyboard layout, so while it is selected the keyboard types
English and speech is transcribed: it can replace the separate English input source, and
Super+Space then toggles between Voiceime and Pinyin.

`voiceime transcribe --file speech.wav` runs the same decode over a recording and prints
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
make check     # gofmt, vet, tests, unreachable code, doc links
make install   # ~/.local/bin and ~/.local/docs
```

Capture is a subprocess and inference is HTTP, so there is no cgo, no audio headers and no
toolchain per platform. Go 1.24 or newer, plus an audio.cpp build for the server.
