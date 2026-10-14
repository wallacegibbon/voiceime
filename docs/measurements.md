# Measurements

Every number here was measured on the reference machine, and each one has the
command that produced it. Nothing is estimated unless the row says so explicitly;
the two inferences are marked `(inferred)` and say what they are inferred from.

Machine: AMD Ryzen 7 5800U (8 cores / 16 threads), 14 GiB RAM, Ubuntu 24.04,
Wayland session, CPU-only inference (no GPU used). audio.cpp built from source as
`RelWithDebInfo` with the CPU backend. Threads = 4 unless stated.

Reproduce with `voiceime stream --file <wav> --realtime --chunk <seconds>` and
`voiceime transcribe <wav> --model <path>`; both print the numbers below.

## Models on this machine

| Model | Package id | Bytes on disk | License note |
|---|---|---:|---|
| SenseVoice-Small Q8 GGUF | `sensevoice_small_q8` | 254,211,200 (242 MiB) | HuggingFace only; the repo `FunAudioLLM/SenseVoiceSmall-GGUF-audiocpp` is **not** on ModelScope |
| Qwen3-ASR-0.6B Q8 GGUF | `qwen3_asr_0_6b_q8_0` | 1,151,272,416 (1.07 GiB) | published in the aggregate repo, present on both HuggingFace and ModelScope |

Two models were deliberately not installed, and the reasons matter:

* `audio8_asr_0_1b_q8_0` — the spec declares `download.kind = unsupported` (the
  checkpoint is CC-BY-NC-4.0 and audio.cpp ships no public GGUF), and the family
  has **no streaming mode** (`modes: ["offline"]`). It cannot be the default for a
  live input method even though it is the right size for CPU.
* `confucius4_r2t2_q8_0` — 2,477,512,064 bytes (2.31 GiB) for a 1.7B model.
  Extrapolating the measured 0.28 RTF of the 0.6B model of the same family gives
  roughly 0.8 RTF, i.e. slower than real time on this CPU `(inferred)`.

## Offline decode speed

| Model | Audio | wall_ms (framework) | Process + load | RTF | x realtime | Peak RSS |
|---|---|---:|---:|---:|---:|---:|
| SenseVoice-Small | 14.07 s (English) | 751 ms | 0.24 s for a 1 s clip | 0.053 | 18.7x | 428 MiB |
| SenseVoice-Small | 5.55 s (Chinese) | 259 ms | — | 0.047 | 21.4x | — |
| Qwen3-ASR-0.6B | 14.07 s (English) | 3,934 ms | 5.02 s | 0.280 | 3.6x | 2,110 MiB |
| Qwen3-ASR-0.6B | 5.55 s (Chinese) | 1,574 ms | — | 0.284 | 3.5x | — |

The "process + load" column is `time` around a whole `audiocpp_cli` invocation, so
it includes process start and weight loading. Three consecutive runs of a 1 s clip
took 0.24 s / 0.22 s / 0.22 s, which is where the ~150 ms model-load figure for
SenseVoice comes from — cheap enough that one process per dictation session is
fine and no warm daemon is needed.

## Streaming latency and quality

Feeding a Chinese sample (5.55 s, `asr_example_zh.wav` from the FunASR test set)
at real time through `--mode streaming`. Reference offline transcript:
`欢迎大家来体验达摩院推出的语音识别模型。`

| Window | Text as it appeared | First text at | Note |
|---:|---|---:|---|
| 3.0 s | `欢迎大家来体验达摩院。` then `推出的语音识别模型。` | 3.14 s of audio | sentence split in the middle; content right |
| 2.0 s | `欢迎大家来。` `体验达摩院推出的。` `识别模型。` | 2.04 s | more splits, content still right |
| 1.0 s | `こ。欢迎大家来。` `体验达摩院。` `推出的。` `音识别模型。` `行。` | 1.04 s | **corrupted**: hallucinated characters, a dropped character, spurious punctuation |

The pattern is the important result: this family decodes each window
independently, with no context carried across the boundary, so shrinking the
window buys latency and pays for it in accuracy. Below about 1.5 s the output is
no longer usable for Chinese, which is why `voiceime config validate` warns there
and why the default is 3 s.

English behaves the same way (1 s windows produced `Some.Call me nature..` with
doubled punctuation where 3 s windows produced `Some call me nature.`).

## What the repair pass is worth

The repair pass re-decodes a finished segment offline, with the whole segment as
context. Same 3 s-window session as above:

| Repair model | Repair time for a 4.24 s segment | Result |
|---|---:|---|
| streaming model (SenseVoice) | **0.55 s** | `欢迎大家来体验打摩院推出的语音识别模型。` — split repaired, one character wrong |
| Qwen3-ASR-0.6B | **5.66 s** | `欢迎大家来体验达摩院推出的语音识别模型。` — matches the offline reference exactly |

So: repair reliably removes the window-boundary damage (the spurious `。` and the
sentence split) for a tenth of the utterance's length with the streaming model.
Buying the last character costs ten times more than real time on CPU, which is
slower than the speaker — which is why repair defaults to the streaming model and
`doctor` reports the alternative as advice rather than switching to it.

Neither layer is strictly better on this sample: the *streaming* window happened
to transcribe `达摩院` correctly while the offline path of the same model produced
`打摩院`. Model accuracy and boundary handling are different problems.

## Model download

| Route | Package | Result |
|---|---|---|
| `HF_ENDPOINT=https://hf-mirror.com` | `sensevoice_small_q8` | 254 MiB in 37 s |
| `--source modelscope` | `qwen3_asr_0_6b_q8_0` | 1.15 GiB in 48 s |
| `--source modelscope` | `sensevoice_small_q8` | **fails**: HTTP 404, that repo does not exist on ModelScope |

Both mirrors work, per package. A package's availability is a property of the
package, not of the mirror: voiceime therefore reports the failing package id and
the error text instead of silently falling back, and `models sizes <pkg>` answers
"is it reachable" without downloading.

## Integration constraints found by measurement

These are the findings that changed the implementation, so they are recorded
rather than left in commit history:

1. **Input must be 16 kHz mono.** Feeding 24 kHz makes offline decoding abort:
   `Silero VAD 16k model only supports sample_rate=16000 (VAD audio chunking
   failed)`. Capture therefore converts up front (PipeWire does it server-side),
   and `capture_rate` other than 16000 is a validation error.

2. **Bundled assets resolve relative to the working directory.** Running the CLI
   from `/tmp` failed with `Silero VAD model path does not exist:
   assets/framework/models/silero_vad`, while the same command from the audio.cpp
   checkout worked. voiceime runs both the streaming engine and the repair pass
   with the working directory set to the checkout.

3. **The repair pass does not need the model's VAD at all.** Segments handed to
   repair are single utterances that voiceime has already endpointed, so
   `--request-option audio_chunk_mode=none` is both faster and immune to (2). On
   the reference sample it also transcribed one character more accurately than the
   VAD path did (`达摩院` versus `打摩院`).

4. **`text_output` is available on stdout even in streaming mode**, along with
   `metrics.*`, which is why the adapter can report the framework's own RTF next
   to its own timings.

5. **`ibus` has no command to commit text.** Its subcommands are engine,
   list-engine, restart, read-config and so on — nothing that injects text into
   the focused application. Text delivery therefore requires being an IBus engine
   (see decisions D6).

6. **The models directory is shared with unrelated models.** On the reference
   machine `~/models` also holds 27B text LLMs, embedding models and vision
   projectors, so 13 of 15 GGUFs there are not audio models at all. Discovery
   filters to models that declare an ASR task and reports the rest as a count.

## Not measured yet

Stated plainly so nobody mistakes an assumption for a result:

* **The IBus engine.** Not implemented, so nothing about injection latency,
  preedit behaviour or per-application compatibility has been measured.
* **Microphone capture end to end.** `voiceime devices` reads the source list and
  `parec` is present with a default source configured, but a captured dictation
  session has not been run.
* **Confucius4-R2T2**, i.e. the append-only long-context streaming family. Its
  viability on this CPU is an inference from a sibling model, not a result.
* **Endpointing against real speech.** The detector is covered by synthetic tests
  (speech between silence, short bursts, sub-hangover pauses, a noisy room); its
  thresholds have not been tuned against a human speaking into this microphone.
* **Concurrency between streaming and repair.** They compete for the same four
  threads; the figures above were taken with both active, but the split has not
  been studied. A GPU backend would remove the contention entirely.
