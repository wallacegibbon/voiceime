# Deploying voiceime on a machine

A complete, copy-pasteable procedure for getting from a checkout to dictating
into applications. It is written for an agent: every step is non-interactive,
every step says how to tell whether it worked, and the JSON field to assert on is
named where the output is machine-readable.

Nothing here needs a GUI. Nothing here needs a human to answer a prompt. If a step
fails, it prints a `fix` command; run it and continue.

- **What you end up with.** Press <kbd>F9</kbd>, speak, press <kbd>F9</kbd> again;
  the recognised text is committed into whatever application has focus, through an
  IBus engine. Everything is local: no network is used at dictation time.
- **How you know it worked.** `voiceime doctor` exits 0, `voiceime engine status`
  reports `engine_registered: true`, `voiceime hotkey status` reports
  `service_active: true`, and activating the engine spawns `parec` plus
  `audiocpp_cli`. Step 10 is a single command that checks all four.

Conventions below: `$REPO` is the checkout (`git rev-parse --show-toplevel`),
`$BIN` is the built binary (`$REPO/bin/voiceime`). Commands are run from `$REPO`.

---

## 0. Prerequisites

| Requirement | Why | Check |
|---|---|---|
| `audiocpp_cli` from an audio.cpp checkout | all inference | `ls ~/playground/audio.cpp/build/*/bin/audiocpp_cli` |
| `python3` | audio.cpp's model manager is a Python script | `python3 -V` |
| `ibus` CLI and a running `ibus-daemon` | text delivery | `ibus version && ibus engine` |
| `parec` (PipeWire or PulseAudio) | audio capture | `parec --version` |
| `systemctl --user` | keeps the shortcut listener running | `systemctl --user is-system-running` |
| Build tools: Go ≥ 1.24 | building the binary | `go version` |

Optional, and only cosmetic if missing: `notify-send` (status notifications),
`pw-play`/`paplay` (start/stop sounds). `doctor` reports them as a warning, never
as a failure.

Set the two paths you will need:

```sh
export REPO="$(git rev-parse --show-toplevel)"
export BIN="$REPO/bin/voiceime"
```

## 1. Build

```sh
make build          # -> built bin/voiceime (<git describe>)
make check          # what CI runs: gofmt, vet, tests, unreachable-code check
```

## 2. Put the binary somewhere permanent

**This step matters more than it looks.** The IBus component file and the systemd
unit both embed the *absolute path* of the binary, so it has to live somewhere that
outlives a rebuild and a moved checkout. `make install` puts it in
`~/.local/bin`, which needs no root:

```sh
make install                          # -> ~/.local/bin/voiceime
export BIN="$HOME/.local/bin/voiceime"
```

Running it straight from the checkout (`"$REPO/bin/voiceime"`) works too, and is
the normal development loop, but it has two consequences worth knowing: every
`make build` replaces the file under any running process, and moving or renaming
the checkout invalidates both installed artifacts. `voiceime doctor` reports both
(`engine_process`, `component_exec`, `hotkey_unit`), which is how they were found
here.

## 3. Configure

The configuration file is the only source of truth (`voiceime config path` prints
its location, `~/.config/voiceime/config.toml` by default).

```sh
"$BIN" config init                    # idempotent; refuses to overwrite without --force
"$BIN" config validate --json         # exit 0, or exit 3 with a fix per issue
```

`config init` prints what to look at before you trust the defaults, and discovers
an audio.cpp checkout when it can find one - the default `audio_cpp_dir` is the
layout of the machine voiceime was developed on, so a fresh install would otherwise
start with a path that does not exist:

```
$ voiceime config init
wrote /home/u/.config/voiceime/config.toml
discovered an audio.cpp checkout at /home/u/src/audio.cpp
review audio_cpp_dir          ~/playground/audio.cpp  (the default names the checkout voiceime was developed in, ...)
review threads                4  (this machine has 8 physical cores; more threads than cores contend ...)
review language               zh  (the reference user's language; ...)
review backend                cpu  (...)
review silence_rms_threshold  600  (the reference room's noise level; measure yours - see step 6)
review download_mirror        https://hf-mirror.com  (...)
```

The list is also in `--json` under `data.review`. Only the entries that are
genuinely machine-specific are on it; everything else in the file is either a hard
constraint (`capture_rate`) or policy that is the same everywhere.

Then set what the list flagged, plus anything you know better than it does:

```sh
"$BIN" config set language zh         # your language; empty asks the model to detect
"$BIN" config set threads 8           # physical cores; `nproc` counts threads, not cores
"$BIN" config set audio_cpp_dir ~/src/audio.cpp   # only if discovery did not find it
"$BIN" config set models_dir ~/models
"$BIN" config validate --json         # exit 0, or exit 3 with a fix per issue
```

If `audio_cpp_dir` is wrong, `doctor` still works: `audiocpp_cli` is looked for on
`PATH` and in a checkout found in a conventional location, so the check reports what
it found rather than failing on the configured path.

Two settings are deliberate and normally left alone:

* `streaming_model = ""` and `refine_model = ""` mean *pick automatically* - the
  installed models are inspected and ranked. Setting a path is only for pinning a
  choice.
* `download_mirror` is an endpoint URL, not a preset name. The default is
  `https://hf-mirror.com`. Change it if your network reaches a different
  Hugging Face protocol host (`https://huggingface.co`, a company cache, a LAN
  address).

If you need a non-default endpoint:

```sh
"$BIN" config set download_mirror https://huggingface.co
```

## 4. Install a model

Two models are in use on the reference machine. Which you need depends on whether
the machine has a working GPU. The numbers are measured on the reference machine
(`docs/measurements.md`); the one inference is called out below.

| Machine | Package | Role | Measured |
|---|---|---|---|
| **CPU only** | `sensevoice_small_q8` | streaming **and** repair | 242 MiB on disk, 428 MiB RSS; decode RTF 0.047 (~21x realtime, Chinese); one process per dictation session with a ~0.3 s load; repair 0.55 s for a 4.2 s utterance |
| **CPU only** | *nothing else* | — | Leave `refine_model` empty so repairs reuse the streaming model. Adding `qwen3_asr_0_6b_q8_0` here makes every repair cost ~5.7 s for a 4.2 s utterance - slower than the person speaking |
| **With a GPU** | `sensevoice_small_q8` | streaming | same model, same reason: see below |
| **With a GPU** | `qwen3_asr_0_6b_q8_0` | repair (`refine_model`) | 1.07 GiB on disk, 2110 MiB RSS; decode RTF 0.28 (3.5x realtime); its offline decode of the reference Chinese sample matched the reference transcript exactly where the streaming model produced one wrong character |

CPU only:

```sh
"$BIN" models install sensevoice_small_q8
"$BIN" config set streaming_model ""     # empty: pick the best installed streaming model
"$BIN" config set refine_model ""        # empty: reuse the streaming model for repair
```

With a GPU:

```sh
"$BIN" config set backend vulkan         # or cuda / hip / metal; your audio.cpp build must have it
"$BIN" models install sensevoice_small_q8
"$BIN" models install qwen3_asr_0_6b_q8_0
"$BIN" config set refine_model ~/models/Qwen3-ASR-0.6B-GGUF/qwen3-asr-0.6b-q8_0.gguf
```

Why the roles are split that way:

* **Streaming stays SenseVoice either way.** It is the smaller model, it loads in
  ~0.3 s on every activation where Qwen3 needs about 5 s, and live text is bounded
  by `chunk_seconds` rather than by throughput - a faster decoder does not make the
  next window arrive any sooner. Qwen3 does declare a streaming mode; it is simply
  not the better streaming choice here.
* **Repair is where accuracy is worth paying for**, because it re-reads a finished
  utterance with the whole utterance as context. That is exactly the difference
  measured above: the sentence split and the wrong character, repaired.
* **On CPU that accuracy is unaffordable**: 5.66 s for a 4.24 s segment, most of it
  loading a 1.07 GiB checkpoint once per segment. The repair would still be running
  when the user starts the next sentence, so `doctor` reports it as advice rather
  than switching to it.

**The GPU half of this table is an inference, not a measurement.** No GPU backend
has been benchmarked for this project (`docs/measurements.md`, "Not measured yet");
what is measured is that the repair cost on CPU is dominated by weight loading,
which is the part a GPU shortens along with the decode. Verify it on your own
machine by watching the `repair` event:

```sh
"$BIN" stream --file speech.wav --events | grep repair     # prints took_seconds
```

Anything below the length of the utterance keeps up with speech.

A 404 from `models install` is a property of the endpoint and the package: set
another `download_mirror` and retry the same package.

## 5. Verify without a microphone

```sh
"$BIN" doctor --json           # exit 0; every check ok (a `refine_model_advice`
                               # warning is informational and expected)
"$BIN" capabilities --json     # commands, settings and detected environment
```

Deliver-boundary check with no audio at all:

```sh
"$BIN" transcribe some.wav     # prints the text; the same code path repair uses
```

## 6. Verify the microphone path

```sh
"$BIN" devices --json                              # capture sources, default marked
"$BIN" stream --seconds 5 --events                 # live partials + endpointing
```

`--events` prints one JSON object per event, so latency and repair costs are
inspectable rather than anecdotal. To test the model and endpointing without a
microphone, replay a file at real time: `"$BIN" stream --file speech.wav --realtime`.

Measured behaviour worth knowing before you judge the output: a quiet room is not
digital silence, and the recogniser can emit a short hallucination when the noise
floor crosses the threshold. That happened here, and it turned out to be a detector
bug rather than a model quirk - the adaptive noise floor was never actually active,
so the threshold sat inside the room's own noise distribution and utterances never
ended. Both are fixed, and the numbers are in
[measurements.md](measurements.md#endpointing-in-a-real-room).

Set the threshold from data, not by guessing, because the room's *spikiness* is
what matters rather than its average:

```sh
# 1. Measure the idle room (20s, nobody speaking).
parec --format=s16le --rate=16000 --channels=1 --raw --latency-msec=40 | head -c 640000 > /tmp/room.raw
python3 - <<'PY'
import struct
data = open('/tmp/room.raw','rb').read()
s = struct.unpack('<%dh' % (len(data)//2), data)
frame = 320
rms = sorted((sum(x*x for x in s[i:i+frame])/frame) ** 0.5 for i in range(0, len(s)-frame, frame))
p = lambda q: rms[int(len(rms)*q/100)]
print("p10=%.0f p50=%.0f p90=%.0f p99=%.0f max=%.0f" % (p(10), p(50), p(90), p(99), rms[-1]))
PY

# 2. Put silence_rms_threshold above the idle p90, and endpoint_min_speech_ms
#    above the longest noise burst (a keystroke is 20-100ms, a word is not).
#    600 is the shipped default: it cleared this room's p90 of 436 in the sweep
#    recorded in measurements.md, and a higher value silently drops quiet speech.
"$BIN" config set silence_rms_threshold 600
"$BIN" config set endpoint_min_speech_ms 500

# 3. Confirm both directions with the same command.
"$BIN" stream --seconds 10 --events     # silent room -> no `segment`, `skipped: no speech detected`
"$BIN" stream --seconds 10 --events     # one sentence -> one `segment` and one `commit`
```

If a sentence produces no `segment`, the threshold is above your voice: lower it
and repeat step 3. That failure is deliberately silent rather than wrong - the
engine commits nothing it did not hear, so a mis-set threshold shows up as no text
instead of hallucinated words. Getting this backwards is the common mistake: a
threshold that is too *high* looks like a broken input method, while one that is
too low only adds occasional junk, so prefer the lower value when in doubt.

## 7. Register the IBus engine

```sh
"$BIN" engine install --apply
```

`--apply` is the part that matters. It writes the component file, adds its
directory to `IBUS_COMPONENT_PATH`, deletes the registry cache and restarts IBus.
Without it you get a file IBus never reads. Two facts make this step surprising, so
they are spelled out:

* `IBUS_COMPONENT_PATH` **replaces** the default search path. The value written
  lists both directories (`.../voiceime/component:/usr/share/ibus/component`); if
  only ours were listed, every other input method would disappear from the panel.
* The daemon caches the component registry. A newly added path stays invisible
  until `~/.cache/ibus/bus/registry` is deleted, which is why `--apply` removes it
  before restarting.

Verify:

```sh
"$BIN" engine status --json
# assert: data.engine_registered == true, data.engine_visible == true
ibus engine voiceime && ibus engine            # switch, then read back
```

A healthy activation spawns children of the engine process - this is the
end-to-end proof that capture and the model work:

```sh
pgrep -af "ibus-engine --ibus"
pgrep -af parec                                 # capture
pgrep -af audiocpp_cli                          # streaming recogniser
ibus engine xkb:us::eng                         # switching away stops both
```

The engine's own session log has nowhere to go when the daemon spawns it (its
stdout is discarded). To see it, run the engine by hand as described in
`docs/AGENT.md`, or point `--trace <file>` at the manual run.

## 8. Start the shortcut listener

```sh
"$BIN" hotkey install           # unit, plus a GNOME keybinding on X11
"$BIN" hotkey status --json     # assert: data.service_active == true
                                # on X11 assert too: data.gnome_binding.installed == true
```

`hotkey install` also runs `systemctl --user daemon-reload` and
`enable --now`, and is idempotent. The default trigger is `<F9>`.

**Which mechanism it installs depends on the session, and the choice is
automatic.** The listener registers the shortcut through IBus
(`org.freedesktop.IBus.GlobalShortcutKeys`), which the compositor honours on
**Wayland**. On **X11** nothing grabs the key - `ibus-daemon` has no display
connection at all, and a passive grab on the key is free for anyone to take - so
the key would just reach the focused application. `hotkey install` therefore also
writes a **GNOME custom keybinding** (executed by `gsd-media-keys`, which does
grab keys on X11) bound to `voiceime hotkey toggle`, and on Wayland it removes
that keybinding if a previous X11 install left one, so two consumers never fight
over one key. `hotkey status --json` reports `data.session`, `data.mechanisms`
and `data.gnome_binding`, so the effective trigger is visible rather than
assumed; `hotkey uninstall` removes both.

Two constraints are built into this step, both measured (`docs/decisions.md`
D13/D14):

* **Use a function key, never a modifier combination.** The IBus global-shortcut
  mechanism consumes the key *release*, so a shortcut like `Control+apostrophe`
  leaves that modifier latched down in the focused application.
* **Never hand-edit the unit to drop `--keys`.** Registering an empty key list
  aborts `ibus-daemon` (`assertion 'size > 0' failed`), which takes every input
  method down with it.

Optional: `loginctl enable-linger "$USER"` if the listener must run without an
interactive login session.

## 9. Use it

Tap <kbd>F9</kbd> to start dictating, tap it again to stop. While it is on, the
input method is `voiceime`; when it is off, the previously selected method comes
back. `voiceime hotkey run --log-events` in a terminal shows every shortcut event
if the key seems to do nothing.

## 10. Acceptance, in one command

```sh
"$BIN" doctor --json >/dev/null; doctor=$?
"$BIN" engine status --json | grep -q '"engine_registered": *true'
"$BIN" hotkey status --json  | grep -q '"service_active": *true'
pgrep -f "ibus-engine --ibus" >/dev/null
echo "doctor=$doctor engine=ok listener=ok engine_process=ok"
```

`doctor=0` plus the three `ok`s is the deployable state. The one thing no command
can assert is a human seeing text appear in a focused window; if that is the goal,
dictate one sentence into a text editor and watch for it.

---

## Recovery

### The checkout or binary moved or was renamed

The symptom is the most confusing one in this project: the engine is listed by
`ibus list-engine`, yet dictation does nothing, and the listener unit restarts
forever:

```
voiceime-hotkey.service: Main process exited, code=exited, status=203/EXEC
```

`203/EXEC` means the executable named in the unit does not exist. Both the
component file and the unit recorded the *old* absolute path, and a process that
was already running keeps working from its old in-memory path - so activation
appears to succeed until it is restarted. `voiceime doctor` reports this as a
failing `component_exec` / `hotkey_unit`, and a moved checkout as a failing
`engine_process`. Fix by re-running the installers from the new location, in this
order:

```sh
"$BIN" engine install --apply      # rewrites <exec> with the current path
"$BIN" hotkey install              # rewrites ExecStart, daemon-reload, enable --now
"$BIN" doctor --json               # must exit 0
```

### The engine does not appear after `engine install`

Delete the registry cache and restart the daemon; `--apply` does this, a hand-run
`ibus restart` does not:

```sh
rm -f ~/.cache/ibus/bus/registry && ibus restart
ibus list-engine | grep voiceime
```

### A stale engine process holds the old code

A running engine keeps running the binary it was started from, so a rebuild does
not take effect until it is restarted. `voiceime doctor` reports it as a `warn` on
`engine_process` ("still running the previous build"); the fix is the same command
either way:

```sh
ibus engine xkb:us::eng
pkill -f "voiceime ibus-engine"
ibus engine voiceime
```

### The key is swallowed but nothing happens

The listener is the only component with no automatic repair. Check
`systemctl --user status voiceime-hotkey`, then `voiceime hotkey run --log-events`
by hand. If a previous listener died without clearing its registration, the key is
still held by the daemon: `ibus restart` clears it (`voiceime hotkey uninstall`
does exactly this).

### Everything is configured but the first partial never arrives

`voiceime stream --seconds 5 --events` isolates this: if it produces text, the
problem is delivery (IBus, step 7) or the trigger (step 8); if it produces nothing,
the problem is capture (`voiceime devices`) or the model (`voiceime doctor`).

## Uninstall

```sh
"$BIN" hotkey uninstall              # stops the listener and restarts IBus, releasing the key
"$BIN" engine uninstall              # removes the component file
make uninstall                       # removes ~/.local/bin/voiceime
"$BIN" models uninstall sensevoice_small_q8
```

`engine uninstall` leaves `IBUS_COMPONENT_PATH` pointing at an empty directory;
`ibus restart` (or a re-login) finishes the cleanup.

## What this deployment does not cover

Stated so nobody mistakes an assumption for a result:

* **Text appearing in a focused window** is verified at the D-Bus boundary
  (`docs/ibus-protocol.md` records the committed-text payload), not by a human
  watching an editor.
* **GPU backends.** Everything here is `backend = cpu`; `cuda`/`vulkan` are passed
  through to audio.cpp and untested.
* **Other desktop environments and X11.** Only GNOME on Wayland has been used.
* **Multi-user machines.** IBus component paths and the wire format for committed
  text are recorded in `docs/ibus-protocol.md`.
