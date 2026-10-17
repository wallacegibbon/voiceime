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
| Microphone capture, endpointing | working, measured against a real room ([numbers](docs/measurements.md#endpointing-in-a-real-room)) |
| Streaming ASR adapter, offline repair pass | working, measured ([numbers](docs/measurements.md)) |
| `doctor` diagnostics with fix commands | working |
| IBus engine (text delivery into applications) | working; protocol verified against a live daemon ([transcript](docs/ibus-protocol.md)) |
| Dictation inside the engine process | working: activating the engine starts capture, live preedit and committed text |
| Shortcut listener (`hotkey install`) | working: a function key (`<F9>` by default) toggles dictation — the IBus global shortcut on Wayland, also a GNOME keybinding on X11 |
| Notification/sound cues | **not implemented** — `doctor` detects `notify-send`/`pw-play`, but nothing emits a cue yet |

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
make check        # gofmt, go vet, unit tests, unreachable-code check
make install      # ~/.local/bin, no root needed; DESTDIR honoured
make cross        # linux/amd64, linux/arm64, windows/amd64, darwin/arm64
make dist         # tarball with the binary and the docs
make help         # every target
```

`make` drives everything; the targets are non-interactive for the same reason the
program is. `make measure AUDIO=speech.wav` replays a recording at real time and
prints the latency and repair numbers.

**Getting from a checkout to dictating: [docs/DEPLOY.md](docs/DEPLOY.md)** — the
whole procedure, step by step, with what to assert after each step.

Requires an audio.cpp installation (the CLI binary is what does the inference) and
Go 1.24 or newer. Nothing else: capture is a subprocess, so there is no audio
library, no development headers and no cgo — which is why the same source
cross-compiles to four platforms from Linux with no toolchain per platform.

The stripped binary is 8.3 MB, 3.4 MB compressed, plus the documentation.

## Use

```
voiceime config init             # write defaults, discover audio.cpp, list what to review
voiceime doctor                  # check the machine, each finding carries a fix
voiceime models install sensevoice_small_q8
voiceime server install          # pick the models, start the resident service
voiceime stream --seconds 5      # dictate for five seconds, watch it transcribe
```

`voiceime stream --file sample.wav --realtime --events` replays a file at real time
and prints per-partial latency plus what the repair pass changed — that is where the
numbers in [docs/measurements.md](docs/measurements.md) came from.

## Models

Two models matter, and which you need depends on the machine. Numbers are measured
on the reference machine ([details](docs/measurements.md)).

| Machine | `streaming_model` | `refine_model` |
|---|---|---|
| **CPU only** | `sensevoice_small_q8` (242 MiB, ~21x realtime) | empty — repairs reuse the streaming model (0.55 s per sentence) |
| **CPU, accurate repair** | `sensevoice_small_q8` | `qwen3_asr_0_6b_q8_0` (0.5-0.8 s per sentence) |
| **With a GPU** | `sensevoice_small_q8` | `qwen3_asr_0_6b_q8_0` (1.07 GiB, the more accurate offline reader) |

```
voiceime models install sensevoice_small_q8          # every machine
voiceime models install qwen3_asr_0_6b_q8_0          # with a GPU, or with the server below
voiceime config set refine_model ~/models/Qwen3-ASR-0.6B-GGUF/qwen3-asr-0.6b-q8_0.gguf
voiceime config set asr_server_url http://127.0.0.1:18080
```

Streaming stays SenseVoice with or without a GPU: it loads in ~0.3 s per dictation
session and live text is bounded by `chunk_seconds`, not by decode speed. Qwen3 is
worth adding only as the repair model, where the whole sentence is re-read with
context.

Both passes run through one resident server that voiceime installs
(`voiceime server install`, a systemd user unit like the shortcut listener), with both
models loaded at startup. That is what makes Qwen3 affordable on CPU: a repair is a
request against weights that are already there, not a process that reloads 1.07 GiB.
The cost is memory — the pair sits at 1.4-1.9 GB — and that is the trade in one line:
the service holds the models so dictation does not load them. See
[docs/decisions.md](docs/decisions.md) D22.

## How it works

```
microphone ──► capture (parec, 16 kHz mono)
                   │
                   ├──► segmenter ── voice activity ──┐
                   │                                 │
                   ▼                                 │
   POST /v1/audio/transcriptions/live ──► deltas ───►├──► transcript
     (one SSE connection per session)                │      │  committed  ──► (IBus)
                   │                                 │      └─ provisional ─┐
                   ▼                                 │                     │
        rolling audio buffer ─────────────────────────┘                     │
                   │                                                       │
                   └──► POST /v1/audio/transcriptions ── re-decode ─────────┘
                          (the same service, its offline entry)
```

Both arrows go to **one resident service** - a systemd user unit whose models are loaded
at startup - so a dictation opens a connection instead of starting a process, and no
sentence pays a model load. The price is memory: the service holds both entries
(1.4-1.9 GB with SenseVoice plus Qwen3-ASR; docs/decisions.md D22).

Two layers of text, and the split is the whole design:

* **Provisional** text comes from the streaming session. It is fast and it is
  wrong at every window boundary, because each window is decoded without context.
* **Committed** text has been delivered to an application and is final. The
  endpoint detector decides the moment of commitment.

When an utterance ends, the audio for it is re-decoded through the service's offline
entry and replaces the provisional text for that same span — or is placed directly when
the streaming layer had not reached it yet, which is the normal case for a short
utterance. That is what corrects words the user has already seen, at a measured 0.5-0.8 s
per sentence whatever the checkpoint, because the weights are already loaded.

## Layout

```
cmd/voiceime        the single binary
internal/config     settings, XDG paths, validation, external-program discovery
internal/audio      PCM helpers, capture, endpointing, rolling buffer
internal/asr        the streaming adapter, the transcript model, the repair pass
internal/models     discovery through `--inspect`, the model manager wrapper
docs/AGENT.md       the operational surface, for agents
docs/DEPLOY.md      deploy on a new machine, step by step, with assertions
docs/ibus-protocol.md  the IBus engine protocol, established by experiment
docs/decisions.md   why each choice was made, and what would change it
docs/measurements.md  every number, with the command that produced it
```

## Licence

TBD.
