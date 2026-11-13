# The recognition path

**Scope.** What voiceime sends, what it expects back, and how it fails. The measured
numbers are in [measurements.md](measurements.md); the protocol of the engine on the other
side is [ibus-protocol.md](ibus-protocol.md).

## One request

A dictation is one recording and one HTTP request:

```
POST /v1/audio/transcriptions
Content-Type: multipart/form-data

model     = the entry the server publishes (its model file's name)
language  = config.language
file      = recording.wav   (16 kHz mono s16le, in memory)
```

The whole recording goes up, unchanged: no trimming, no voice-activity gate, no window.
The server decodes it offline and answers once:

```json
{"text":"…","timing":{"wall_ms":1234}}
```

voiceime commits `text` to the focused application. An empty `text`, or one that carries no
letter or digit, commits nothing.

Every recording is sent whole. Holding the trigger is the user saying "this is the audio",
and the engine does not second-guess it: no level test decides whether a recording is worth
sending. The default model does answer words to non-speech - two seconds of silence come
back as `"我。"` ([measurements.md](measurements.md#what-the-models-do-with-non-speech)) -
and a recording that contains only punctuation is dropped rather than typed, because a lone
`"。"` is punctuation from audio the model could not read, not a transcript.

## The service

`voiceime server install` writes a systemd user unit whose `ExecStart` is
**`voiceime server run`**. That command renders the audio.cpp config from `config.toml`
and then runs the server, so the settings stay the only source of truth: there is no
generated file that can disagree with them.

The config names **one entry**, in offline mode, with the request options a multipart
upload has no field for:

```json
{
  "host": "127.0.0.1", "port": 18080, "backend": "cpu", "threads": 4,
  "lazy_load": false,
  "models": [{ "id": "qwen3-asr-0.6b-q8_0", "family": "qwen3_asr",
               "path": "/home/u/models/Qwen3-ASR-0.6B-GGUF/qwen3-asr-0.6b-q8_0.gguf",
               "task": "asr", "mode": "offline",
               "default_request_options": { "audio_chunk_mode": "none" } }]
}
```

`lazy_load: false` is the point of the service: the weights belong to its startup, so a
request never waits for a load.

The id is the model file's name (`server.EntryID`). The client does not need it
configured: it asks `GET /v1/models` and uses the single entry it finds, which is also
how it reports which model is actually being served.

## Failure

| Failure | What happens |
|---|---|
| no service, wrong address | the request fails; the engine log names it, and `doctor` reports it before a dictation |
| the server refuses (bad model id, unloadable checkpoint) | the request fails and the reason is logged; nothing is typed |
| the decode exceeds `asr_server_timeout_ms` | the request is abandoned; nothing is typed |
| the answer is empty | nothing is typed |

A failed dictation delivers nothing rather than a guessed sentence: the user repeats it.

## Text form

Two steps run on every answer before it is typed, in this order:

1. **`text_script`** (`simplified` | `preserve`) normalizes the Chinese script with embedded
   OpenCC `t2s` data; it is a no-op for other languages.
2. **The trailing period is dropped.** A recogniser ends its answer with a sentence-final
   period whether or not one was dictated, which is wrong for dictation - a noun dropped at
   a cursor in the middle of a sentence arrives with a full stop after it. Question marks,
   exclamation marks and commas stay: those say something the speaker did say.

The alternative is the model's own switch. SenseVoice answers with **no** punctuation at
all when its `enable_itn` option is off, and that takes the digits out of dates and numbers
with it (measured: `"嗯。"` against `"嗯"`), so voiceime keeps the switch on
(`request_options enable_itn=false` would turn it off) and removes the one mark that is
always added.

An answer that carries no letter or digit is not typed at all: a lone `"。"` is punctuation
from audio the model could not read, not a transcript.
