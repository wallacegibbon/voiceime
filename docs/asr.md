# The recognition path

**Scope.** What voiceime sends, what it expects back, and how it fails. The measured
numbers are in [measurements.md](measurements.md); the protocol of the engine on the other
side is [ibus-protocol.md](ibus-protocol.md).

## One request

A dictation is one recording and one HTTP request:

```
POST <asr_server_url>
Content-Type: multipart/form-data

model     = config.model        # the id this address is asked for
language  = config.language
file      = recording.wav       (16 kHz mono s16le, in memory)
```

The address is used exactly as it is written: no route is appended to it, nothing is derived
from it, and it is the same string `curl` would be pointed at.

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

## The address, and what may be behind it

The engine has one requirement: the address in `asr_server_url` answers that POST. A provider
and a model on this machine are both answers, and the engine's code does not distinguish
them - no route is appended to the address, no model is guessed for it, and the request names
the id in the `model` setting because nothing in the program asks an address what it serves.
A model list is a catalogue, not a promise about what this account accepts, and an endpoint may
publish none at all.

So this document describes the request, and the rest of what makes a local model answer it
belongs to the project that installs one (`asr-service.sh`). Two facts about that service are
worth knowing from here, because they are what this request assumes:

* The weights are loaded before the first request, not during it. That is why a dictation's
  latency is a decode and not a load.
* The served entry is configured not to cut the upload up. A recording is sent whole, and a
  service that decoded the pieces separately would answer a different question.

`voiceime doctor`'s `server_unit` finding reads back whatever such a service on this machine
says about itself - the unit that runs an audio.cpp, and the binary and config that unit names -
because a unit whose paths have gone missing restarts forever and says nothing. The names are
that project's; the engine looks for a unit by the binary it runs (`audiocpp_server`) and takes
the rest from its own `ExecStart`.

## Failure

| Failure | What happens |
|---|---|
| the address does not answer | the request fails; the engine log names it, and `doctor` reports it before a dictation |
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
with it (measured: `"嗯。"` against `"嗯"`), so voiceime keeps the switch on - there is no
setting for it, because the measured trade is not worth offering - and removes the one mark
that is always added.

An answer that carries no letter or digit is not typed at all: a lone `"。"` is punctuation
from audio the model could not read, not a transcript.
