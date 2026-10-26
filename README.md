# voiceime

Local streaming speech input for GNOME on Linux, built on
[audio.cpp](https://github.com/0xShug0/audio.cpp). Dictate into any application
and watch the text appear as you speak.

**No GUI, by design.** The configuration file is the single source of truth and
every action is a subcommand with machine-readable output, so an automated agent
can install, configure and repair it without a human. See
[docs/decisions.md](docs/decisions.md) for why, and [docs/AGENT.md](docs/AGENT.md)
for the operational surface.

## Where to go

| If you want to... | Read |
|---|---|
| deploy on a new machine, step by step, with what to assert after each step | [docs/DEPLOY.md](docs/DEPLOY.md) |
| drive it from a script or an agent: commands, exit codes, config keys | [docs/AGENT.md](docs/AGENT.md) |
| change the recognition pipeline | [docs/asr.md](docs/asr.md) |
| change the IBus engine or the shortcut listener | [docs/ibus-protocol.md](docs/ibus-protocol.md) |
| verify any number, or how it was measured | [docs/measurements.md](docs/measurements.md) |
| understand why a choice was made, and what would change it | [docs/decisions.md](docs/decisions.md) |

The runtime is the authoritative surface: `voiceime capabilities --json` prints
the command and setting surface, and `voiceime doctor --json` the state of this
machine. When this documentation and the runtime disagree, the runtime wins.

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
sentence as a `CommitText` signal on the bus.

The one hop that is not directly observed is text appearing in a *focused*
application's text field: the GTK probe written for it never received window
focus, so there was no focused input context to commit into. Everything on this
side of that boundary is measured ([docs/ibus-protocol.md](docs/ibus-protocol.md)
records where the measurement stops).

## Build

```
make build        # bin/voiceime, version stamped from git describe
make check        # gofmt, go vet, unit tests, unreachable-code check, docs links
make install      # ~/.local/bin, no root needed; DESTDIR honoured
make help         # every target (cross, dist, measure, ...)
```

`make` drives everything; the targets are non-interactive for the same reason the
program is. `make measure AUDIO=speech.wav` replays a recording at real time and
prints the latency and repair numbers. Requires an audio.cpp installation (the CLI
binary is what does the inference) and Go 1.24 or newer. Nothing else: capture is a
subprocess, so there is no audio library, no development headers and no cgo — which
is why the same source cross-compiles to four platforms from Linux with no
toolchain per platform.

**Getting from a checkout to dictating: [docs/DEPLOY.md](docs/DEPLOY.md)** — the
whole procedure, step by step, with what to assert after each step.

## Use

```
voiceime config init             # write defaults, discover audio.cpp, list what to review
voiceime doctor                  # check the machine, each finding carries a fix
voiceime models install sensevoice_small_q8
voiceime models list --refresh    # identify it (the only command that writes that cache)
voiceime server install          # pick the models, start the resident service
voiceime stream --seconds 5      # dictate for five seconds, watch it transcribe
```

`voiceime stream --file sample.wav --realtime --events` replays a file at real time
and prints per-partial latency plus what the repair pass changed — that is where the
numbers in [docs/measurements.md](docs/measurements.md) came from.

## Models

One small model on every machine (`sensevoice_small_q8`, the streaming model), plus
an optional, more accurate repair model (`qwen3_asr_0_6b_q8_0`) with a GPU or for
accurate repair on CPU. Both run in one resident service the project installs, so a
dictation opens a connection instead of loading a model; the cost is that the
service holds the weights all the time. Which to install, the commands, and the
measured trade: [docs/DEPLOY.md, step 4](docs/DEPLOY.md#4-install-a-model-and-start-the-server).
The numbers: [docs/measurements.md](docs/measurements.md). The rationale in one
entry: [docs/decisions.md](docs/decisions.md) (D22).

## How it works

A microphone stream is captured at 16 kHz mono, endpointed by a small energy
detector, and sent to one resident service whose models are loaded at startup. Live
draft text comes back over a streaming connection; when the detector closes an
utterance, its audio is re-decoded offline and the repaired text replaces the draft
before it is committed. That split is the whole design: **provisional** text
(underlined, rewritable) while the utterance is open, **committed** text (final)
after the repair lands. The wire-level contract — the two requests, the SSE stream,
which audio a repair re-reads, and what happens when a pass fails — is in
[docs/asr.md](docs/asr.md); the reasons are in
[docs/decisions.md](docs/decisions.md) (D20-D22).

## Layout

```
cmd/voiceime        the single binary
internal/config     settings, XDG paths, validation, external-program discovery
internal/audio      PCM helpers, capture, endpointing, rolling buffer
internal/asr        the streaming adapter, the transcript model, the repair pass
internal/textnorm   Chinese text cleanup: Traditional to Simplified
internal/models     discovery through `--inspect`, the model manager wrapper
docs/asr.md         how the recognition pipeline works: the two requests, the SSE stream, the failure contract
docs/AGENT.md       the operational surface, for agents
docs/DEPLOY.md      deploy on a new machine, step by step, with assertions
docs/ibus-protocol.md  the IBus engine protocol, established by experiment
docs/decisions.md   why each choice was made, and what would change it
docs/measurements.md  every number, with the command that produced it
```

## Licence

TBD.
