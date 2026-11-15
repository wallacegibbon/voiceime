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

## How long a recording the served model takes

SenseVoice small refuses a recording longer than its encoder can hold, in its own words
(`voiceime transcribe`, the default checkpoint):

| Recording | Result |
|---|---|
| 28 s (real speech) | decodes |
| 90 s (a 7.5 s clip repeated) | decodes |
| 418 s | `500 … SenseVoice encoder input exceeds positional capacity` |

The boundary was not narrowed. Nothing in the model's metadata or in the service's
`/v1/models` answer states a maximum, so voiceime cannot name one and passes the refusal
through as the server's own sentence. `max_utterance_seconds` (60 s) is inside what was
accepted here, which is the only reason that default is safe.

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

## A held key repeats; a pressed-again key does not

The hold is read from key events, and the repeats of a held key arrive as more presses.
Measured on this desktop:

```
$ xset q | grep 'repeat delay'
  auto repeat delay:  500    repeat rate:  33
```

so the first repeat follows a press by 500ms and every repeat after it by 30ms.

That number sets a constant. The engine has to tell the repeats of one press from a press
that follows a release which never arrived, and the first repeat looks exactly like the
latter. A gap of 300ms was tried, and measured against a person dictating: every hold
longer than half a second was cut in two at that first repeat, three times in one session,
each one visible in the log as `the press before this one never saw its release`. So
`autoRepeatGap` is 750ms: above the repeat delay, below the pause someone makes before
pressing again on purpose after seeing nothing happen.

The gap exists because of what the engine used to do: it remembered "the trigger is down"
until it saw the release, and a release that never reached it left that belief standing for
as long as the engine stayed selected. Every press after it was taken for a repeat, so the
key did nothing at all - the failure reported by the user, which they cleared by switching
to another input method and back (deselecting the engine drops the state). Nothing was
logged, because a repeat is not interesting.

With the gap, a press that cannot be a repeat ends the recording the missing release left
open - that audio was spoken as part of the same dictation, so it is decoded rather than
dropped - starts the next one, and writes the log line that says so.

Not measured: which client swallowed the release. The engine has no way to know, and its log
line reports only what it saw.

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
keystroke while the top-right menu kept showing the previous input method. `voiceime apply`
adds `('ibus', 'voiceime')`, and the menu lists it.

## Restarting the daemon is not the same as the daemon being back

`ibus restart` returns while the old daemon is still shutting down. Asking the new one to
select an input method immediately afterwards:

```
$ ibus restart; ibus engine voiceime
(process:69153): IBUS-CRITICAL **: 19:31:50.529: ibus_bus_set_global_engine: assertion
'IBUS_IS_BUS (bus)' failed
Set global engine failed.
```

`ibus engine` and `ibus list-engine` both fail until the daemon has come back, and the same
command succeeds a moment later. `voiceime apply` waits for `ibus list-engine` to answer
before it reads or sets the session's engine - bounded at 20 s, after which it reports a
finding instead of hanging. Without that wait `apply` answered "the input method could not be
selected again", caused by the restart it had just performed itself.

## Which audio.cpp runs is not something the settings said

The reference machine holds two runnable audio.cpp binaries, dated the same day:

| Path | Size | md5 |
|---|---|---|
| `~/.local/bin/audiocpp_cli` | 34 MB | `b4e03e71…` |
| `~/playground/audio.cpp/build/linux-cpu-release/bin/audiocpp_cli` | 865 MB | `ce548d18…` |

With `asr_bin` empty, `FindASRBinary` takes the first because it is on `PATH`, and
`audiocpp_server` is taken from beside it - so a deploy that never named one was served by a
different host than the one an earlier configuration had recorded, and every command called
that a success. That is why `voiceime apply` records the answer it used, search or `PATH`
whichever answered, instead of leaving the setting empty for the next run to re-derive.

## Not measured

* GPU backends: every figure is CPU.
* Capture latency and drop behaviour under load.
* Anything about a model other than the one named above.
