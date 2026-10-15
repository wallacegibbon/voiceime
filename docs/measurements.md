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

| Model | Package id | Bytes on disk | Availability note |
|---|---|---:|---|
| SenseVoice-Small Q8 GGUF | `sensevoice_small_q8` | 254,211,200 (242 MiB) | its repo `FunAudioLLM/SenseVoiceSmall-GGUF-audiocpp` is served over the Hugging Face protocol and does **not** exist on ModelScope |
| Qwen3-ASR-0.6B Q8 GGUF | `qwen3_asr_0_6b_q8_0` | 1,151,272,416 (1.07 GiB) | published in the aggregate repo `audio-cpp/audio.cpp-gguf`, which is mirrored on both |

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

Downloads go to the single endpoint in `download_mirror`. Measured on the
reference machine:

| Endpoint | Package | Result |
|---|---|---|
| `https://hf-mirror.com` | `sensevoice_small_q8` | 254 MiB in 37 s |
| `https://hf-mirror.com` | `audio-cpp/audio.cpp-gguf` (`qwen3_asr_0_6b_q8_0`) | reachable: a ranged request returns `206` |

Two properties of the reference network are recorded because the default endpoint
is chosen for them:

* **`huggingface.co` is unreachable there without the local proxy**: a direct
  `curl` fails with `Connection timed out after 20002 ms`, while `hf-mirror.com`
  serves the same object. This is why the default is a mirror rather than the
  upstream host, and why `download_mirror` is a URL: the usable host is a property
  of the network, not of voiceime.
* **`hf-mirror.com` is a real mirror**: it answers
  `302 → https://cas-bridge.xethub.hf.co/...` - its own CDN bridge - and returns
  bytes. An earlier reading of `308 → huggingface.co` was an artefact of the local
  HTTP proxy (`https_proxy=http://127.0.0.1:7890`) and does not reproduce with
  `--noproxy '*'`.

A package's availability is a property of the endpoint *and* the package, so
voiceime reports the failing package id and the error text instead of silently
falling back, and `models sizes <pkg>` answers "is it reachable" without
downloading.

## Endpointing in a real room

The detector decides when text becomes committable, so its behaviour was measured
against the reference machine's own microphone rather than only against synthetic
audio. All figures are 20 ms frame RMS, the same frame the detector uses.

| Audio | p10 | p50 | p90 | p99 | max |
|---|---:|---:|---:|---:|---:|
| 20s of the idle room (nobody speaking) | 231 | 309 | 436 | 7992 | 23873 |
| 8s of continuous speech (16 kHz reference clip) | 180 | 3883 | 7495 | 9625 | 10716 |

Two things follow, and both were bugs:

* **The room is not quiet, it is *spiky*.** Its median is 309 while 1% of frames
  reach 7992 and the peak is 23873 - keystrokes and chair noise. The configured
  threshold was 350, which sits inside the room's own distribution, and *any*
  single frame above the threshold reset the silence hangover, so the 800 ms of
  continuous silence required to end an utterance almost never accumulated. A
  silent room produced "utterances" of 4.28 s, 9.90 s and 1.30 s.
* **The adaptive noise floor was dead code.** `session.go` passed
  `Adaptive: true`, but `Threshold()` only used the measured floor when
  `Calibrate()` had been called, and nothing in the pipeline calls it. The
  detector therefore ran on the static 350 the whole time.

Replaying the *same* 20s recording is what makes these numbers comparable - it
removes the room's run-to-run variation:

| Build | threshold | phantom segments | committed text |
|---|---|---:|---|
| before the fixes | 350 | 3 (0-4.28, 7.82-17.72, 18.68-19.98s) | `被。`, `。。`, `嗯。` |
| after the fixes, at the 350 that was the default then | 350 | 3 (0.38, 1.10, 2.30s) | `被。。。。`, `嗯。`, `。嗯。` |
| after the fixes, tuned for this room | 800 -> 600 | 0 | nothing (`skipped: no speech detected`) |

The same recording through the tuned settings still recognises speech: the
reference clip is segmented once (0.04-8.00 s) and repaired normally. The tunings
applied to the reference machine are `silence_rms_threshold = 800` (above the
room's p90 of 436, and well below speech's p50 of 3883) and
`endpoint_min_speech_ms = 500` (longer than the 340 ms transient that survived the
threshold change). Sweeping the threshold over the same recording settled the
default: 450, 500, 600, 700 and 800 all produced zero phantom segments on it (the
min-speech rule rejects the transients), and all produced the same single repaired
segment for the speech clip. Since the three values are indistinguishable on noise
but not on quiet speech, the default is the lowest that keeps real margin above the
room: **600**, not the 800 first tried, and not the 350 that the third row above
shows sitting inside the room's own noise. A speaker quiet enough to be missed at
600 should lower it further and re-run the check above.

To measure a different room, the same way:

```bash
parec --format=s16le --rate=16000 --channels=1 --raw --latency-msec=40 | head -c 640000 > /tmp/room.raw
python3 - <<'PY'
import struct
data = open('/tmp/room.raw','rb').read()
s = struct.unpack('<%dh' % (len(data)//2), data)
frame = 320                      # 20 ms at 16 kHz, the detector's frame
rms = sorted((sum(x*x for x in s[i:i+frame])/frame) ** 0.5 for i in range(0, len(s)-frame, frame))
p = lambda q: rms[int(len(rms)*q/100)]
print("p10=%.0f p50=%.0f p90=%.0f p99=%.0f max=%.0f" % (p(10), p(50), p(90), p(99), rms[-1]))
PY
```

Set `silence_rms_threshold` above the idle p90, then confirm the other direction
with the same command while speaking: the empty room must produce no `segment`
event, and a sentence must produce one.

**Only a boundary commits.** On the sandbox bus with `dbus-monitor`, activating
the engine over an 8 s clip of unbroken speech emitted 6 `UpdatePreeditText` and
**one** `CommitText` - at the end, because no boundary was ever found. The same
clip with two seconds of silence in the middle emitted **two** `CommitText`, one
per utterance. So text stays underlined (preedit) for exactly as long as the
detector considers the utterance open, and "it never commits" and "it never ends"
are the same sentence.

The other half of that experiment is what a *late* window does. Replaying 4.5 s of
speech followed by 2 s of silence: the segment commits at 4.50 s, and the next
window - which covers [3.14 s, 6.16 s] and therefore starts inside audio the
application already has - arrived after the commit. Before the fix the transcript
read `... we meet at a time when it.` + `We meet at a time when it is.` + `Is easy
...`, i.e. the delivered sentence came back as preedit and was committed twice; the
fix drops that window and the transcript reads `... we meet at a time when.` +
`Is easy to confuse noise with strength and speed with.`, with the drop reported as
`skipped: a window overlapping already-delivered text was dropped`. The missing
few words are the ones the dropped window held *after* the delivered boundary; the
next segment's repair re-decodes that audio, which is why the second sentence still
appears.

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

* **GPU backends.** Nothing here has been measured on a GPU: `backend` accepts
  cuda/vulkan/hip/metal and passes it through to audio.cpp, but every figure in
  this document is from the CPU backend. The model recommendation for a GPU machine
  in [DEPLOY.md](DEPLOY.md#4-install-a-model) is an inference from the CPU
  measurement (the repair cost is dominated by weight loading), not a result.
* **Confucius4-R2T2**, i.e. the append-only long-context streaming family. Its
  viability on this CPU is an inference from a sibling model, not a result.
* **Endpointing against a human voice in this room.** The detector is covered by
  synthetic tests (speech between silence, short bursts, sub-hangover pauses, a
  noisy room) and its thresholds are now tuned against a 20 s recording of this
  room plus a 16 kHz speech clip - but not against a person speaking into this
  microphone. The reference clip's speech sits at p50 RMS 3883 against a room p90
  of 436, so the current threshold of 800 has margin on both sides; a quiet
  speaker at a distance is the case that would need it lowered - as one did at
  800 on the reference machine, which is why the default is 600.
* **Concurrency between streaming and repair.** They compete for the same four
  threads; the figures above were taken with both active, but the split has not
  been studied. A GPU backend would remove the contention entirely.
