# How the ASR pipeline works

This is the wiring diagram for anyone who has to change the recognition path. It
describes what goes over the socket, why there are two passes, and what happens when
one of them fails. The reasons behind each choice are in
[decisions.md](decisions.md) (D20-D22, D24); the numbers are in
[measurements.md](measurements.md).

The short version: **voiceime does not run a model.** A resident service does, and
voiceime is its client. A dictation is one long-lived HTTP connection; each finished
sentence is one HTTP POST.

```
microphone ──► capture (parec, 16 kHz mono s16le, 20 ms blocks)
                   │
                   ├──► endpointer (energy VAD) ── utterance closed ──┐
                   │                                                 │
                   ▼                                                 ▼
   POST /v1/audio/transcriptions/live             POST /v1/audio/transcriptions
   raw PCM up, SSE down, entry "stream"           multipart WAV, entry "refine"
                   │ deltas                                        │ final text
                   ▼                                               ▼
              preedit (provisional,          transcript.Place() → commit → IBus
              underlined, rewritable)        (final, cannot be rewritten)
```

## The service

`voiceime server install` writes a systemd user unit (`voiceime-asr-server.service`)
whose `ExecStart` is **`voiceime server run`**, not `audiocpp_server` directly. That
command renders the server config from `config.toml` and then execs the server, so the
settings stay the only source of truth: change a model, restart the unit, and what is
served changes. There is no generated file that can silently disagree.

The rendered config names **two entries**, and they cannot be merged:

```json
{
  "host": "127.0.0.1",
  "port": 18080,
  "backend": "cpu",
  "threads": 4,
  "lazy_load": false,
  "models": [
    { "id": "stream", "family": "sense_asr", "mode": "streaming", "task": "asr",
      "path": "/home/u/models/SenseVoice-Small-GGUF/sensevoice-small-q8-audiocpp-v1.gguf",
      "default_request_options": { "audio_chunk_mode": "fixed", "audio_chunk_duration_sec": "3" } },
    { "id": "refine", "family": "qwen3_asr", "mode": "offline", "task": "asr",
      "path": "/home/u/models/Qwen3-ASR-0.6B-GGUF/qwen3-asr-0.6b-q8_0.gguf",
      "default_request_options": { "audio_chunk_mode": "none" } }
  ]
}
```

* `lazy_load: false` is the point of the service: the weights belong to its startup,
  so nothing on the request path ever waits for a load. A session start is a TCP
  connect - 144 µs in D22's measurement, 157 µs in the run below.
* **Two entries, even when they name the same file.** The server holds a model for the
  whole length of a request, so a repair sent to the id a live dictation owns waits for
  that dictation to end - measured at 10.9 s behind a 12 s session.

Two probes are used by `doctor`, `server status` and `server restart`:

```
GET /health      → 200
GET /v1/models   → {"object":"list","data":[
                     {"id":"stream","object":"model","owned_by":"engine","family":"sense_asr",
                      "task":"asr","mode":"streaming","loaded":true,"path":"…/sensevoice-small-q8-audiocpp-v1.gguf"},
                     {"id":"refine", … "mode":"offline","loaded":true,"path":"…/qwen3-asr-0.6b-q8_0.gguf"}]}
```

`doctor` compares those `path`s with `streaming_model` and `refine_model`. The entry
ids never change, so a service left on the previous checkpoint is invisible otherwise -
it just produces slightly different text.

## Pass 1 - the live session (draft text)

`internal/asr/stream.go` writes HTTP/1.1 by hand, and that is not gold-plating:
`http.Client.Do` does not return until the request body is finished, even when the
server has already answered, so the standard client cannot express "open the session,
then send audio". curl can because it speaks HTTP directly; so does this.

### Request

```http
POST /v1/audio/transcriptions/live?model=stream&sample_rate=16000&channels=1&sample_format=s16le&stream=true&language=zh HTTP/1.1
Host: 127.0.0.1:18080
Content-Type: application/octet-stream
Accept: text/event-stream
Transfer-Encoding: chunked
```

The body is **raw PCM**, not a form and not a WAV. Each capture block becomes one HTTP
chunk, so about 50 chunks per second:

```
280␍␊                      ← 0x280 = 640 bytes = 20 ms at 16 kHz s16le
<640 bytes of PCM>␍␊
280␍␊
<640 bytes of PCM>␍␊
```

End of audio is the **terminating chunk**, `0␍␊␍␊`. That is what makes a
push-to-talk release work with no special command: the server flushes the model and
answers `done`.

### Response

The server answers `200` and headers immediately (that is why `OpenStream` returns
before any audio is fed), then streams SSE:

```
data: {"type":"transcript.text.delta","delta":"Some call me nature."}

data: {"type":"transcript.text.delta","delta":"Others call me Mother Nature."}

data: {"type":"transcript.text.done","text":"Some call me nature.Others call me Mother Nature.","timing":{"ttft_ms":3173.6}}

data: [DONE]
```

`ttft_ms` is time to first token. A revision-capable model family may also emit
`{"type":"transcript.text.partial","text":"…"}`, a whole provisional transcript to
*replace*; see [Known gap](#known-gap-whole-transcript-snapshots) below.

### What voiceime does with it

* **delta** → a provisional chunk. The span is `[samples fed at the previous delta,
  samples fed now)`: an approximation, because the text came from the last
  `chunk_seconds` window but arrived at the current position. `RepairSpan` (pass 2)
  exists to absorb that approximation.
* The provisional text is published as **preedit** - underlined, not yet delivered.
* **done** → recorded as `Result.Text` and reported by `stream --events` as
  `stream_text`. It is **not** used to commit: the offline pass decides what is final,
  and a snapshot here would overwrite the repair.

## Pass 2 - the repair decode (final text)

When the endpointer closes an utterance (default: 800 ms of silence after speech, and
at least 300 ms of speech in total), `internal/daemon/session.go` queues an offline
decode of that audio. One worker, in order, off the audio path.

### Request

```http
POST /v1/audio/transcriptions HTTP/1.1
Content-Type: multipart/form-data; boundary=c38c5ba7…

--c38c5ba7…
Content-Disposition: form-data; name="model"

refine
--c38c5ba7…
Content-Disposition: form-data; name="language"

zh
--c38c5ba7…
Content-Disposition: form-data; name="file"; filename="segment.wav"
Content-Type: application/octet-stream

RIFF…WAVEfmt … <PCM>          ← a 16 kHz mono 16-bit WAV, encoded in memory
--c38c5ba7…--
```

Why multipart: the audio is already in the rolling buffer and never has to exist on
disk. The server's other route takes JSON with a **server-local path**, which is no use
to a client that holds the audio in RAM - and it is also the only route that reads a
per-request `options` object, which is why the decode contract lives in the entry's
`default_request_options` instead (`asr.RepairRequestOptions`).

### Response

```json
{"text":"Twenty-two thousand five hundred times longer than you.","timing":{"wall_ms":…}}
```

`wall_ms` is the server-side decode time (the client measures the whole request
separately; the trace below reports that as `took_seconds`). The timeout is
`asr_server_timeout_ms` (default 30 s): a repair slower than the
sentence is worse than no repair.

### Which audio is decoded

Three cases, and the distinction is where the reported bugs came from:

1. **Streaming text covers the utterance** → decode the **union** of the utterance's
   range and every provisional chunk the repair will replace (`RepairSpan`). A chunk is
   decoded as one unit and extends past the range; decoding only the range would delete
   its text for the overhang without re-reading it.
2. **Nothing covers it** → decode the utterance itself, widened by
   `endpoint_silence_ms / 2` on each side (default ±400 ms). A short utterance always
   lands here: it closes 800 ms after the speech ends, while a draft arrives only every
   `chunk_seconds`. The detector stops on the last loud frame, so without the pad a
   quiet tail is lost - measured: `Some call me。` instead of `Some call me nature.`.
3. **Too short (< 0.4 s) or fallen out of the buffer** → do not decode; commit the
   draft and report why (`skipped`).

### How the text becomes final

`Transcript` (in `internal/asr/transcript.go`) holds chunks with the sample range they
came from and a `source` of `stream` or `repair`:

* `Place(start, end, text)` replaces every provisional chunk overlapping the range, or
  inserts the text in sample order when none does. Chunks before `committed` are never
  touched: text delivered to an application cannot be rewritten.
* `CommitProvisional()` returns exactly the text that was provisional until now, and
  marks it committed. The session clears the composition first, then commits, because
  an application that kept showing the text would display the utterance twice.

The commit happens **after** the repair lands. Delivering before it would deliver the
window-boundary damage the repair exists to fix.

## The failure contract

There are three ways a session can fail, and they surface in three places:

| Failure | Where it is caught | What the caller sees |
|---|---|---|
| Service not running, wrong model id, refused request | `OpenStream` handshake | an error naming the unit to check; no session starts |
| SSE `error` event mid-dictation (model threw, stall past the idle timeout, oversized or malformed chunk) | `Stream.readEvents` | `Close` returns the server's own reason |
| Connection closed without the terminating chunk | `chunkedReader` → `unexpected EOF` | the same, as a truncated session |

The protocol is built so the last two cannot masquerade as "the user stopped talking":
the server refuses to send `transcript.text.done` for a body that was not terminated
properly. The client has to hold up its end of that, which it does in two ways:

* `Close()` returns the failure (and `Result.ProcessErr` carries the same value for a
  caller that reads what the session produced).
* A session that failed **does not commit its stranded draft**. Utterances that did
  close are unaffected - each was repaired and committed before this point - so what is
  withheld is only text no offline decode ever reached. It is reported with a count:

  ```
  streaming session failed; withheld 12 characters it never decoded offline
  (streaming server: model died: cuda OOM)
  ```

`voiceime stream` exits 1 with that message; the IBus engine logs
`session ended: …` into its trace. The server's side of the same failure is in
`journalctl --user -u voiceime-asr-server` (the unit runs the server with `--log`).

The error payload nests its reason - `{"type":"error","error":{"message":…}}` - and the
client reads that shape. If a future server puts the message somewhere else, the client
falls back to quoting the payload rather than inventing a generic sentence.

## A real session

`voiceime stream --file sample_16k.wav --events --json` against the reference machine
(SenseVoice streaming + Qwen3-ASR repair): 14.07 s of audio, 5 partials, wall 15.23 s,
connect 157 µs. Trimmed to the lines that matter; every number is from that run:

```json
{"kind":"ready","load":156602,"model":"stream"}
{"kind":"segment","start_seconds":0.42,"end_seconds":2.0,"speech_seconds":1.58}
{"kind":"partial","text":"Some call me nature.","fed_seconds":3.2}
{"kind":"repair","text":"Some call me nature.","took_seconds":0.774,"speech_seconds":2.38}
{"kind":"commit","text":"Some call me nature."}
{"kind":"partial","text":"I've been here for over 4.5 billion.","fed_seconds":9.16}
{"kind":"segment","start_seconds":5.92,"end_seconds":9.64,"speech_seconds":3.72}
{"kind":"repair","text":"I've been here for over 4.5 billion years.","took_seconds":1.273,"speech_seconds":3.72}
{"kind":"commit","text":"I've been here for over 4.5 billion years."}
{"kind":"skipped","reason":"a window overlapping already-delivered text was dropped"}
{"kind":"partial","text":"Years,22.","fed_seconds":12.16}
{"kind":"segment","start_seconds":10.7,"end_seconds":13.64,"speech_seconds":2.94}
{"kind":"partial","text":"F00 times longer than you.","fed_seconds":14.07}
{"kind":"repair","text":"Twenty-two thousand five hundred times longer than you.","took_seconds":1.168,"speech_seconds":3.74}
{"kind":"commit","text":"Twenty-two thousand five hundred times longer than you."}
```

Three things in that trace are the design in miniature:

* The draft said `…4.5 billion.` and `F00 times longer than you.`; the offline decode
  said `…4.5 billion years.` and `Twenty-two thousand five hundred times longer than
  you.`. Only the second was committed - this is why the streaming pass never decides
  what is final.
* `Years,22.` is a window that began *inside* the sentence just delivered (its recorded
  start is 9.16 s, the delivered audio ends at 9.64 s). The model still produced it, so
  `partial` prints it - but `AppendStream` refuses it, and the `skipped` line immediately
  before it is that refusal. Keeping it would show those words as preedit again and then
  commit them a second time.
* The sentence those broken windows belonged to is repaired from its own range
  (10.7-13.64 s), not from the draft: `Years,22.` was never part of the transcript, and
  the repair re-read the whole span with context. That is why the delivered text is
  correct even though the draft was not.

`stream --events` prints one JSON object per line; `kind` is one of:

| kind | meaning |
|---|---|
| `repair_route` | which service and entry ids this session will use |
| `ready` | the session is open (`load` is connect time, nanosecond integer) |
| `partial` | new draft text, with the audio position at which it arrived |
| `preedit` | the whole provisional text as a composition snapshot |
| `segment` | the endpointer closed an utterance |
| `repair` | an offline decode finished, its text and its cost |
| `commit` | text delivered to the application |
| `skipped` | something was not done, and why |
| `summary` | the session's totals, then `{"command":"stream","ok":true,…}` |

## Tuning

Settings that change behaviour, and what they trade:

| Key | Default | Effect |
|---|---|---|
| `chunk_seconds` | 3 | Streaming window. Below 1.5 measurably corrupts CJK (`config validate` warns). Does **not** gate when text is committed - a closed utterance is decoded offline. |
| `endpoint_silence_ms` | 800 | Silence after speech before an utterance closes. Also sets the repair pad to half of it. |
| `endpoint_min_speech_ms` | 300 | Shorter is treated as noise and never committed. |
| `silence_rms_threshold` | 600 | **Floor** of the adaptive detector, not a fixed threshold. Too high makes quiet speech invisible; measure before raising it. |
| `max_utterance_seconds` | 60 | Forces a boundary, and sizes the rolling buffer that repairs read from. |
| `refine_on_segment_end` | true | The only switch that trades accuracy for latency. |
| `asr_server_timeout_ms` | 30000 | Deadline for one repair. |
| `language` | `zh` | Passed to both passes; empty lets the model detect. |

The model paths, `chunk_seconds`, `threads`, `backend` and the option maps are read by
the service at **startup**, so changing one needs `voiceime config apply` (which
restarts the unit and waits until it serves the new entries).

## Known gap: whole-transcript snapshots

`transcript.text.partial` is the event a revision-capable family (Phonon-2 and
similar) uses to replace the whole provisional transcript. `asr.EventSnapshot` and the
session code that honours it exist, but the live reader does not map the event, so no
such session produces one.

That is deliberate rather than forgotten: a snapshot describes the *whole* transcript,
including audio already committed, and no part of it can be attributed to a sample
range - so accepting one after a commit would mean inserting or deleting text the
application already has. Wiring it up needs a trim to the provisional range first.
None of the families voiceime selects emits snapshots today.

## Code map

| File | What lives there |
|---|---|
| `internal/asr/stream.go` | the live session: handshake, chunked writes, SSE parsing, error propagation |
| `internal/asr/server.go` | the offline decode over multipart, and `RepairRequestOptions` |
| `internal/asr/transcript.go` | the two-layer text model: `AppendStream`, `Place`, `RepairSpan`, `CommitProvisional` |
| `internal/daemon/session.go` | capture → endpointing → repair queue → commit, and the failure rules |
| `internal/audio/segmenter.go` | the energy detector that decides where an utterance begins and ends |
| `internal/audio/buffer.go` | the rolling buffer a repair reads its audio from |
| `internal/server/server.go` | renders `asr-server.json`, probes `/health` and `/v1/models` |
| `internal/cli/cmd_server.go` | `server install/run/restart/status`, and the unit that runs them |
