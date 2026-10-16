# Resident-server repair: prototype harness

This directory is the measurement rig for the one change on this branch: letting
the repair pass decode a finished utterance through a resident `audiocpp_server`
instead of a fresh `audiocpp_cli` process.

Nothing here is needed to *use* the feature. It exists so the numbers behind the
decision can be re-run on this machine instead of being taken on trust.

## Why the route exists

The repair pass re-decodes every accepted utterance, so a refine model is only
usable if it is affordable per utterance. On the CLI route the cost is not the
decode, it is the model load, paid again for every utterance:

| | 0.5s clip | 4.24s clip |
| --- | --- | --- |
| `audiocpp_cli`, fresh process (Qwen3-ASR-0.6B) | 4.87s | 5.79s |
| resident server, warm | 0.34s | 1.05s |

The per-process cost is ~4.4s of CPU on a 1.15 GiB model regardless of audio
length, and it is not I/O: the whole file reads out of page cache in 0.09s
(`cat model.gguf > /dev/null`) and the CLI reports only 463ms of session time for
a 0.5s clip, so the remainder is unreported work before the session starts.

Replayed through the real IBus engine (`engine_ab.py`), one 4.24s clip with
Qwen3-ASR-0.6B as the refine model both ways, commit latency measured from the
endpointer closing a segment to its text landing:

| | utterance 1 | utterance 2 |
| --- | --- | --- |
| CLI process per utterance | 5.29s | (queued behind the first) |
| resident server, warm | 0.73s | — |

Three utterances (`multi.wav`, 1.2s sentences on 1s pauses) shows the difference
that matters: the CLI route *accumulates*, because each ~5.2s repair queues
behind the last, so the third sentence arrives 11.7s after it was spoken.

| | utterance 1 | utterance 2 | utterance 3 |
| --- | --- | --- | --- |
| CLI process per utterance | 5.13s | 8.45s | 11.67s |
| resident server, warm | 0.56s | 0.60s | 0.59s |

The server column is flat, which is the whole point: the repair keeps up with
speech, so raising the refine model becomes a quality decision rather than a
latency one.

## Two properties that are requirements, not details

**A model cannot be shared with a live session.** The server holds a model for the
whole length of a request, so a repair sent to the id a live dictation owns waits
for that dictation to end. `model_lock.py` shows it: a ~215ms repair takes 9.7s
when sent to the streaming model's id and 1.14s when sent to a second id, against
a 12.7s live session. Two entries cost two resident copies of the weights (+253MB
for SenseVoice Q8, measured). The server voiceime spawns therefore serves only the
offline entry: the streaming layer stays on the CLI's stdin route, so nothing is
duplicated.

**Per-utterance request options must live in the server config.** The multipart
transcription upload accepts only `file`, `model`, `language`, `prompt/text`,
`busy_timeout_ms` and `stream`; only the JSON route reads an `options` object.
The repair route passes `audio_chunk_mode=none` plus the user's `request_options`
on the command line, so the equivalent belongs in the entry's
`default_request_options` - which is what `server_config.py` writes. Verified
that the text matches the CLI route on the sample clips either way.

## Running it

The spawn path is what `voiceime` does by default now, so the commands below are for
measuring it rather than for driving it:

```bash
voiceime config set refine_model ~/models/Qwen3-ASR-0.6B-GGUF/qwen3-asr-0.6b-q8_0.gguf
voiceime config set asr_server_url http://127.0.0.1:18080   # spawned on demand
```

To run a server by hand instead - which is what the probes here talk to - use two
roles with the refine one its own entry:

```bash
python3 server_config.py \
  --streaming ~/models/SenseVoice-Small-GGUF/sensevoice-small-q8-audiocpp-v1.gguf \
  --refine    ~/models/Qwen3-ASR-0.6B-GGUF/qwen3-asr-0.6b-q8_0.gguf --refine-family qwen3_asr \
  --out /tmp/voiceime-server.json
audiocpp_server --config /tmp/voiceime-server.json
```

Then:

```bash
# load time versus decode time, per request
python3 latency.py --model refine --clip speech.wav --runs 4

# the model-lock rule
python3 model_lock.py --live-model stream-sense --other-model refine \
  --live-clip long.wav --repair-clip utterance.wav

# end-to-end through the real engine, per utterance. --server rewrites two config
# keys for the run and restores the file afterwards.
python3 engine_ab.py --binary ../../bin/voiceime --clip speech.wav \
  --server --server-url http://127.0.0.1:18080 --model refine --label "server"
python3 engine_ab.py --binary ../../bin/voiceime --clip speech.wav \
  --model ~/models/Qwen3-ASR-0.6B-GGUF/qwen3-asr-0.6b-q8_0.gguf --label "CLI"
```

`engine_ab.py` needs a live IBus session: it starts the engine by hand and
activates it with `ibus engine voiceime`, which is what a hotkey press does.

## Not done yet

- **The spawned server is not supervised outside a session.** Its lifetime is the
  dictation session's: spawned at the start, stopped after the last repair lands.
  A parent killed outright (SIGKILL, no chance to run Stop) leaves the child
  running; it is reused by the next session, and `idle_unload_ms` releases its
  weights two minutes later rather than holding them. A systemd user unit would
  make that airtight and is the next step if this graduates from a prototype.
- **A hung server stalls one repair at a time.** A server that accepts a
  connection and never answers costs `asr_server_timeout_ms` on each repair before
  the fallback runs. A server that has died refuses the connection immediately, so
  the common failure is cheap; a circuit breaker after the first failure would
  bound the rare one.
