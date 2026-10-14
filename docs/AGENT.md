# Agent guide

This document is written so that a program can install, configure and verify
voiceime without a human. Every command is non-interactive, idempotent and has a
`--json` mode. `voiceime capabilities` prints the same surface at runtime,
derived from the code, so it is the authoritative version if the two disagree.

## Building and installing

The Makefile is the intended interface; every target is non-interactive.

```
make build                     # bin/voiceime, version stamped from git describe
make check                     # gofmt check, go vet, unit tests
make install PREFIX=~/.local   # DESTDIR is honoured for packaging
make measure AUDIO=speech.wav CHUNK=3
```

A plain `go build ./cmd/voiceime` also works and produces the same binary, with
the version reporting as `dev`. An installed copy is not required: running
`bin/voiceime doctor` straight from the checkout is the normal development loop,
and nothing in the program depends on being on `PATH`.

## Ground rules

* **No prompts, no GUI.** Nothing waits for input. If a command needs a decision,
  it is a flag or a config key.
* **`--json` may be placed anywhere** on the command line, before or after the
  subcommand: `voiceime doctor --json`, `voiceime --json doctor` and
  `voiceime transcribe f.wav --json` all work. Flags and positional arguments may
  also be interspersed.
* **One JSON object per invocation**, on stdout on success, on stderr on failure:
  `{"command": ..., "ok": true, "data": {...}}` or
  `{"command": ..., "ok": false, "error": {"kind": ..., "message": ..., "fix": ...},
  "exit_code": N}`.
* **Exit codes** are the primary machine signal:

  | Code | Meaning | What to do |
  |---:|---|---|
  | 0 | success | — |
  | 1 | the command ran and failed | read `error.message` |
  | 2 | the command line was wrong | fix the invocation, do not retry |
  | 3 | a prerequisite is missing | run `error.fix`, then retry |

* **`fix` is a runnable command.** Findings from `doctor`, `config validate` and
  precondition failures carry one.
* **Nothing is destructive without a flag.** `models install` writes into the
  models directory; `config init` refuses to overwrite without `--force`.

## Configure

The configuration file is the single source of truth. Read the whole surface with:

```
voiceime capabilities --json          # commands, flags, settings, detected environment
voiceime config show --json           # current values plus each key's description
```

Set keys with the generic setter — it converts types, writes atomically and
reports the new config's validity:

```
voiceime config set models_dir ~/models
voiceime config set streaming_model /home/u/models/SenseVoice-Small-GGUF/sensevoice-small-q8-audiocpp-v1.gguf
voiceime config set chunk_seconds 3
voiceime config set language zh
voiceime config set refine_on_segment_end true
```

Settings that matter, and the reasoning a caller needs to pick values:

| Key | Default | Notes |
|---|---|---|
| `models_dir` | `~/models` | Scanned for models; downloads land here. May contain unrelated models. |
| `audio_cpp_dir` | `~/playground/audio.cpp` | Locates `audiocpp_cli` and the model manager. |
| `asr_bin` | `""` | Explicit CLI path; empty means discover. |
| `backend` | `cpu` | `audio.cpp` backend: cpu, cuda, vulkan, hip, metal. |
| `threads` | `4` | Worker threads for both the streaming engine and repairs. |
| `streaming_model` | `""` | Empty means pick automatically. |
| `refine_model` | `""` | Empty means reuse the streaming model (fastest). |
| `language` | `zh` | Passed to the model; empty asks it to auto-detect. |
| `chunk_seconds` | `3` | Below 1.5 measurably corrupts CJK output; validate warns. |
| `refine_on_segment_end` | `true` | The pass that repairs window-boundary damage. |
| `capture_rate` | `16000` | Must stay 16000 (models' VAD). Validation error otherwise. |
| `capture_source` | `""` | PipeWire source name; empty uses the default input. |
| `download_mirror` | `hf-mirror` | `hf-mirror`, `huggingface` or `modelscope`. |
| `session_options` / `request_options` | `{}` | Passed straight to `audiocpp_cli`. |

Validate after changing anything:

```
voiceime config validate --json       # exit 3 when a blocking issue exists
```

## Install a model

```
voiceime models recommended --json    # what to install, and why
voiceime models sizes <pkg> --json    # reachability and size, downloads nothing
voiceime models install <pkg>         # downloads into models_dir
voiceime models installed --json      # what is present locally, no network
```

Mirrors are per package, not global: `models sizes` reports the failure per
package, so a caller that sees a 404 on one mirror can retry the same package
after changing `download_mirror`. Recommended packages, measured on the reference
machine, are printed by `models recommended`; the short version is
`sensevoice_small_q8` for streaming (242 MiB, ~20x realtime on CPU) and
`qwen3_asr_0_6b_q8_0` only if the machine can afford a repair slower than speech.

## Verify

```
voiceime doctor --json                # every prerequisite, each with its fix
voiceime doctor --refresh --json      # re-inspect models instead of using the cache
voiceime devices --json               # capture sources and the default one
```

`doctor` covers: the config file, the `audiocpp_cli` binary and its version, the
model manager script, the models directory, model selection for streaming and
repair, capture tooling and default source, IBus engine registration, and optional
notification/sound tooling. Exit code 3 means at least one check failed; the
`fix` of each failure is the next command to run. Loop until it exits 0.

## Exercise the pipeline

Both of these print their own timings, which is what makes quality questions
answerable without a human listening:

```
voiceime transcribe speech.wav --model <path> --json
voiceime stream --file speech.wav --realtime --chunk 3 --json
voiceime stream --seconds 5 --mic            # capture from the microphone
```

`stream` accepts `--events` for one JSON object per event instead of human text.
Event kinds: `ready` (model loaded, with load time), `partial` (new text, with
the audio position at which it arrived), `segment` (an utterance closed), `vad`
(model-reported voice activity, when a family reports it), `repair` (offline
re-decode of a segment, with its cost), `summary`.

With `--realtime`, the `fed_seconds` of a partial is also the time from the user
speaking to that text appearing, which is the latency number to compare against
`chunk_seconds`.

## Pending: text delivery

The IBus engine is **not implemented yet**. Commands referenced by `doctor`'s fix
text (`voiceime engine install`, `voiceime dictate`) do not exist in this build;
`doctor` reports IBus as failing and that is accurate. Until it lands, voiceime
can be configured and measured end to end, but it cannot type into an application.

## Troubleshooting table

| Symptom | Cause | Action |
|---|---|---|
| `doctor` exits 3 with `audio_cpp` failing | `audiocpp_cli` not found | `voiceime config set audio_cpp_dir <checkout>` or `voiceime config set asr_bin <binary>` |
| `no speech models found` | empty or wrong models directory | `voiceime models install sensevoice_small_q8` |
| `models install` fails with 404 | that package is not on the configured mirror | `voiceime config set download_mirror huggingface` (or `--source modelscope`) and retry |
| Repair fails with `Silero VAD model path does not exist` | the CLI's bundled assets resolved relative to the working directory | voiceime already sets the working directory and disables chunking for repairs; if it recurs, set `audio_cpp_dir` correctly |
| Text is corrupted and windows are short | `chunk_seconds` below 1.5 | `voiceime config set chunk_seconds 3` |
| Model aborts with `only supports sample_rate=16000` | input is not 16 kHz | `voiceime config set capture_rate 16000` |
| `models list` shows unrelated files | the models directory is shared | expected; discovery filters to ASR models and reports the count it ignored |
