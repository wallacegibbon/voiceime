# voiceime

Local streaming speech input for GNOME on Linux, built on
[audio.cpp](https://github.com/0xShug0/audio.cpp). Dictate into any application
and watch the text appear as you speak.

**No GUI, by design.** The configuration file is the single source of truth and
every action is a subcommand with machine-readable output, so an automated agent
can install, configure and repair it without a human. See
[docs/decisions.md](docs/decisions.md) for why, and [docs/AGENT.md](docs/AGENT.md)
for the operational surface.

## Status

| Piece | State |
|---|---|
| Config surface, validation, capabilities | working |
| Model discovery with an inspect cache | working |
| Model download through a configurable Hugging Face protocol endpoint | working |
| Microphone capture, endpointing | implemented, tests pass, not yet run against a real dictation |
| Streaming ASR adapter, offline repair pass | working, measured ([numbers](docs/measurements.md)) |
| `doctor` diagnostics with fix commands | working |
| IBus engine (text delivery into applications) | working; protocol verified against a live daemon ([transcript](docs/ibus-protocol.md)) |
| Dictation inside the engine process | working: activating the engine starts capture, live preedit and committed text |
| Hotkey installation, notification/sound cues | **not implemented** — switching input methods is currently the on/off switch |

So the whole path now exists: install a model, install the engine, activate it, and
speak. Proven by measurement rather than by assertion - the engine process was
observed loading SenseVoice (327 ms), emitting partials at 3.14 s and 5.55 s,
repairing the finished utterance in 594 ms, and committing the repaired Chinese
sentence as an `CommitText` signal on the bus.

The one hop that is not directly observed is text appearing in a *focused*
application's text field: the GTK probe written for it never received window
focus, so there was no focused input context to commit into. Everything on this
side of that boundary is measured.

## Build

```
make build        # bin/voiceime, version stamped from git describe
make check        # gofmt check, go vet, unit tests
make install      # PREFIX=/usr/local by default, DESTDIR honoured
make cross        # linux/amd64, linux/arm64, windows/amd64, darwin/arm64
make dist         # tarball with the binary and the docs
make help         # every target
```

`make` drives everything; the targets are non-interactive for the same reason the
program is. `make measure AUDIO=speech.wav` replays a recording at real time and
prints the latency and repair numbers.

Requires an audio.cpp installation (the CLI binary is what does the inference) and
Go 1.24 or newer. Nothing else: capture is a subprocess, so there is no audio
library, no development headers and no cgo — which is why the same source
cross-compiles to four platforms from Linux with no toolchain per platform.

The stripped binary is 3.1 MB, 1.4 MB compressed, plus the documentation.

## Use

```
voiceime doctor                  # check the machine, each finding carries a fix
voiceime models recommended      # what to install and why
voiceime models install sensevoice_small_q8
voiceime devices                 # pick a microphone if the default is wrong
voiceime stream --seconds 5      # dictate for five seconds, watch it transcribe
```

`voiceime stream --file sample.wav --realtime --chunk 3` replays a file at real
time and prints per-partial latency plus what the repair pass changed — that is
where the numbers in [docs/measurements.md](docs/measurements.md) came from.

## How it works

```
microphone ──► capture (parec, 16 kHz mono)
                   │
                   ├──► segmenter ── voice activity ──┐
                   │                                 │
                   ▼                                 │
        audiocpp_cli --mode streaming  ── partials ──►├──► transcript
                   (one process per session)         │      │  committed  ──► (IBus)
                   │                                 │      └─ provisional ─┐
                   ▼                                 │                     │
        rolling audio buffer ─────────────────────────┘                     │
                   │                                                       │
                   └──► audiocpp_cli offline ── re-decode ──────────────────┘
```

Two layers of text, and the split is the whole design:

* **Provisional** text comes from the streaming session. It is fast and it is
  wrong at every window boundary, because each window is decoded without context.
* **Committed** text has been delivered to an application and is final. The
  endpoint detector decides the moment of commitment.

When an utterance ends, the audio for it is re-decoded with the offline path and
replaces the provisional text for that same span. That is what corrects words the
user has already seen — a measured cost of 0.55 s for a 4.2 s sentence when the
repair uses the streaming model.

## Layout

```
cmd/voiceime        the single binary
internal/config     settings, XDG paths, validation, external-program discovery
internal/audio      PCM helpers, capture, endpointing, rolling buffer
internal/asr        the streaming adapter, the transcript model, the repair pass
internal/models     discovery through `--inspect`, the model manager wrapper
docs/AGENT.md       the operational surface, for agents
docs/ibus-protocol.md  the IBus engine protocol, established by experiment
docs/decisions.md   why each choice was made, and what would change it
docs/measurements.md  every number, with the command that produced it
```

## Licence

TBD.
