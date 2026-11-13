# Measured numbers

Every figure below was measured on the reference machine (AMD Ryzen 7 5800U, 8 physical
cores, CPU backend, `threads = 8`), with the model each row names.

## Decode speed

The offline entry reads the whole recording and answers once. Wall time from
`voiceime transcribe` and from the server's own `wall_ms`:

| Model | Audio | Decode | RTF |
|---|---|---|---|
| SenseVoice small q8 (default) | 3.0 s | 0.11 s | 0.04 |
| SenseVoice small q8 | 34.5 s | 1.58 s | 0.05 |
| Qwen3-ASR 0.6B q8 | 3.36 s | 0.66 s | 0.20 |
| Qwen3-ASR 0.6B q8 | 10.5 s | 1.35 s | 0.13 |
| Qwen3-ASR 0.6B q8 | 34.5 s | 4.42 s | 0.13 |

RTF falls as the recording grows (fixed overhead amortises), so the wait after a release is
`RTF × the length of what you said`: about 1.6 s for a 35 s sentence with SenseVoice, 4.4 s
with Qwen3-0.6B. `asr_server_timeout_ms` (30 s) is the ceiling on one decode.

## What the models do with non-speech

Four recordings that contain no speech, sent to both models directly:

| Input | SenseVoice small q8 | Qwen3-ASR 0.6B q8 |
|---|---|---|
| 2 s digital silence | `"我。"` | `""` |
| 2 s of this room (peak 23966, RMS 4940) | `"。"` | `""` |
| 0.4 s of key clicks | `"。"` | `""` |
| 5 s of key clicks in a room | `"我。"` | `""` |

Qwen3 answers nothing; SenseVoice answers words. The engine does not try to tell these
apart before sending - holding the trigger is the user's statement that the audio is speech,
and a level test that guessed wrong would swallow a quiet recording instead of typing a
wrong word. What it does do is drop an answer that carries no letter or digit: a lone `"。"`
is punctuation from audio the model could not read, not a transcript.

Absent from the table: **accuracy on real speech.** The two models were not compared on a
transcribed recording here, so "the larger one is more accurate" is the vendor's claim, not
this machine's measurement.

## Streaming is not available from this entry

The multipart route accepts `stream=true`, but only for a model served in streaming mode:

```
$ curl -F model=refine -F stream=true -F file=@clip.wav …/v1/audio/transcriptions
{"error":{"message":"transcription stream=true requires a model configured with mode=streaming"}}
```

and with a streaming-mode entry the deltas arrive per streaming window, not per token
(`ttft_ms` 121 for the first). voiceime does not use that path: the text of a dictation is
decided by one offline read of the whole recording.

## Hold-to-talk, end to end

The deployed engine, driven over D-Bus on a sandbox `ibus-daemon`, with audio played into a
null sink's monitor so it reached the engine through `parec` as a microphone's would:

| Assertion | Result |
|---|---|
| selecting the engine starts no capture | `pgrep -c par[e]c` = 0 |
| the trigger press opens it | 1 |
| the release closes it | 0 |
| the recording, sent whole | 3.36 s |
| decoded once | 700 ms |
| text delivered | `committed 2 character(s): 嗯。` |

`ProcessKeyEvent(96, 0, 4)` and `ProcessKeyEvent(96, 0, 1<<30 | 4)` — a press and a release
of `` Ctrl+` `` (96 = `XK_grave`) — both returned `true`.

**A real client sends both edges.** A live session's engine receives the user's own
keystrokes, and they include releases with the mask set:

```
ProcessKeyEvent(105,23,1073741824)     ← 'i' released
ProcessKeyEvent(32,57,1073741824)      ← space released
ProcessKeyEvent(65288,14,1073741824)   ← BackSpace released
ProcessKeyEvent(65507,29,0)            ← Ctrl pressed
```

`1073741824` is `1 << 30` (`IBUS_RELEASE_MASK`). Not measured: a human holding the trigger —
the presses above are a client's typing, not a test of the chord.

## The desktop's input-source list

After an install that used `ibus engine` only:

```
$ ibus list-engine | grep voiceime
  voiceime - Voice
$ ibus engine voiceime && ibus engine
  voiceime                                   ← the switch worked
$ gsettings get org.gnome.desktop.input-sources sources
  [('xkb', 'us'), ('ibus', 'pinyin')]        ← and the desktop never heard of it
```

That is the "I cannot see whether the switch happened" report: the engine received every
keystroke while the top-right menu kept showing the previous input method. `engine install`
now adds `('ibus', 'voiceime')`, and the menu lists it.

## Not measured

* GPU backends: every figure is CPU.
* Capture latency and drop behaviour under load.
* Anything about a model other than the one named above.
