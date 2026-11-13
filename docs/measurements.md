# Measured numbers

Every figure below was measured on the reference machine (8 physical cores, CPU backend,
`threads = 8`): AMD Ryzen 7 5800U, Qwen3-ASR-0.6B q8 for the decode.

## Decode speed

The offline entry reads the whole recording and answers once. Wall time from
`voiceime transcribe` and from the server's own `wall_ms`:

| Audio | Decode | RTF |
|---|---|---|
| 3.36 s | 0.66 s | 0.20 |
| 4.0 s | 0.65 s | 0.16 |
| 10.5 s | 1.35 s | 0.13 |
| 34.5 s | 4.42 s | 0.13 |

RTF falls as the recording grows (fixed overhead amortises), so the wait after a release is
roughly `0.13 × the length of what you said`. `asr_server_timeout_ms` (30 s) is the ceiling
on one decode.

## What the model does with non-speech

Two seconds of digital silence and two seconds of this room (peak 23966, RMS 4940) were sent
to the offline entry:

```
{"text":"","timing":{"wall_ms":301.12}}    ← 2 s of digital silence
{"text":"","timing":{"wall_ms":275.80}}    ← 2 s of room
```

An empty answer commits nothing, which is what makes "send the whole recording, no gate"
safe: a mis-press costs one request, not inserted words. This is one model on one machine at
one moment, not a property of every checkpoint.

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
