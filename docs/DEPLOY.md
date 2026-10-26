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
  `service_active: true`, and `voiceime server status` reports `listening: true` with both
  entries served. Step 10 is a single command that checks all four.

Conventions below: `$REPO` is the checkout (`git rev-parse --show-toplevel`),
`$BIN` is the built binary (`$REPO/bin/voiceime`). Commands are run from `$REPO`.

**Run them from inside the graphical session**, not from a bare ssh or console
shell. Several commands - `ibus` itself, and through it `engine status` and the
`ibus` check in `doctor` - reach the daemon through the session, and in a shell that
did not inherit `DISPLAY` (or `WAYLAND_DISPLAY`, or `IBUS_ADDRESS`) they fail in a
way that reads as a broken install: `doctor` exits 3 with `ibus: the voiceime engine
is not registered` and a fix that would change nothing, while `engine status`
reports `engine_registered: false` on a machine where the engine is registered.
`hotkey install` is the same trap from the other side - see step 8. If you must work
over ssh, copy the session's environment first (`DISPLAY=:0` or whatever `ps` shows
`ibus-daemon` holding, or its `IBUS_ADDRESS`).

**Scope.** The procedure: one numbered path from checkout to dictating, plus
updating, recovery and uninstall. The command reference is [AGENT.md](AGENT.md); the
reasons behind the steps are in [decisions.md](decisions.md); the measured numbers
are in [measurements.md](measurements.md).

---

## 0. Prerequisites

| Requirement | Why | Check |
|---|---|---|
| `audiocpp_cli` from an audio.cpp checkout | model inspection and the model manager | `ls ~/playground/audio.cpp/build/*/bin/audiocpp_cli` |
| `audiocpp_server`, in the same build tree | **all inference**: one resident service serves both the live draft and the offline repair | `ls ~/playground/audio.cpp/build/*/bin/audiocpp_server` |
| `python3` | audio.cpp's model manager is a Python script | `python3 -V` |
| `ibus` CLI and a running `ibus-daemon` | text delivery | `ibus version && ibus engine` |
| `parec` (PipeWire or PulseAudio) | audio capture | `parec --version` |
| `systemctl --user` | keeps the recognition server and the shortcut listener running | `systemctl --user is-system-running` |
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

`make check`'s `deadcode` step installs `golang.org/x/tools/cmd/deadcode` into a
throwaway directory, so it needs network access **once**. On a machine without it,
the pinned module is usually already in the module cache and can be used directly:

```sh
GOPROXY="file://$(go env GOMODCACHE)/cache/download" make deadcode
```

or run the rest of the gate on its own: `make fmt-check vet test`.

## 2. Put the binary somewhere permanent

**This step matters more than it looks.** The IBus component file and the systemd
unit both embed the *absolute path* of the binary, so it has to live somewhere that
outlives a rebuild and a moved checkout. `make install` puts it in
`~/.local/bin`, which needs no root:

```sh
make install                          # -> ~/.local/bin/voiceime
export BIN="$HOME/.local/bin/voiceime"
```

`make install` also copies `docs/` to `~/.local/docs/`, and the embedded OpenCC
`t2s` data's Apache-2.0 licence and notice to `~/.local/licenses/opencc/`. That is
not decoration: the systemd unit names the agent guide *relative to the binary*
(`dir(binary)/../docs/AGENT.md`), so the docs are part of the install - without them
the unit's `Documentation=` is a dead link. The licence travels with the binary
because the conversion tables are compiled into it.

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
"$BIN" config init                    # fresh machine: writes the file and prints the defaults to review;
                                       # existing file: one line and exit 0, nothing written; --force rewrites it
"$BIN" config validate --json         # exit 0, or exit 3 with a fix per issue
```

`config init` prints what to look at before you trust the defaults, and discovers
an audio.cpp checkout when it can find one - the default `audio_cpp_dir` is the
layout of the machine voiceime was developed on, so a fresh install would otherwise
start with a path that does not exist. The list is also in `--json` under
`data.review` (sample output in [AGENT.md, Configure](AGENT.md#configure)); it
carries only the entries a new machine should look at rather than accept - the
reference machine's paths and resources, and the user preferences that default to
the reference user's choices. Everything else in the file is either a hard
constraint (the 16 kHz capture rate) or policy that is the same everywhere.

Then set what the list flagged, plus anything you know better than it does:

```sh
"$BIN" config set language zh         # your language; empty asks the model to detect
"$BIN" config set text_script preserve      # only if you want Traditional Chinese
"$BIN" config set threads 8           # physical cores; `nproc` counts threads, not cores
"$BIN" config set audio_cpp_dir ~/src/audio.cpp   # only if discovery did not find it
"$BIN" config set models_dir ~/models
"$BIN" config validate --json         # exit 0, or exit 3 with a fix per issue
```

If `audio_cpp_dir` is wrong, `doctor` still works: `audiocpp_cli` is looked for on
`PATH` and in a checkout found in a conventional location, so the check reports what
it found rather than failing on the configured path.

**The file outlives versions.** A new version's settings need no migration: the loader
starts from the defaults with the file's values over them, so a key a new version adds
takes its default on the next load with no one touching the file, and a key a version
removes is tolerated, ignored and dropped at the next write - a removed setting must
not brick the file the version that wrote it no longer runs. A key that matches nothing
is rejected, with `voiceime capabilities` named in the error, so a typo cannot silently
do nothing. `config init --force` is the one command that rewrites the file itself: it
keeps every value the file chose, adds the new keys at their defaults and drops the
removed ones - a migration, not a clobber.

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

## 4. Install a model and start the server

Two models are in use on the reference machine. Which you need depends on whether
the machine has a working GPU. The numbers are measured on the reference machine
(`docs/measurements.md`); the one inference is called out below.

| Machine | Package | Role | Measured |
|---|---|---|---|
| **CPU only** | `sensevoice_small_q8` | streaming **and** repair | 242 MiB on disk; decode RTF 0.047 (~21x realtime, Chinese); both entries resident in the service; repair 0.55 s for a 4.2 s utterance, and a dictation starts in microseconds |
| **CPU only** | *nothing else* | — | Leave `refine_model` empty so both entries serve the streaming model: two copies of 242 MiB, and repairs that cost the decode rather than a load. Adding `qwen3_asr_0_6b_q8_0` gives the more accurate reader, at 1.07 GiB resident extra (below) |
| **With a GPU** | `sensevoice_small_q8` | streaming | same model, same reason: see below |
| **With a GPU** | `qwen3_asr_0_6b_q8_0` | repair (`refine_model`) | 1.07 GiB on disk, 2110 MiB RSS; decode RTF 0.28 (3.5x realtime); its offline decode of the reference Chinese sample matched the reference transcript exactly where the streaming model produced one wrong character |
| **CPU, accurate repair** | `qwen3_asr_0_6b_q8_0` | repair (`refine_model`) | 1.07 GiB extra resident; measured 0.54-0.84 s per sentence through the service, against 5.13/8.45/11.67 s as a process per utterance |

CPU only:

```sh
"$BIN" models install sensevoice_small_q8
"$BIN" models list --refresh             # identify what was downloaded (the only writer)
"$BIN" config set streaming_model ""     # empty: pick the best installed streaming model
"$BIN" config set refine_model ""        # empty: reuse the streaming model for repair
```

With a GPU:

```sh
"$BIN" config set backend vulkan         # or cuda / hip / metal; your audio.cpp build must have it
"$BIN" models install sensevoice_small_q8
"$BIN" models install qwen3_asr_0_6b_q8_0
"$BIN" models list --refresh             # identify both checkpoints before serving them
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
* **On CPU that accuracy used to be unaffordable** as a process per utterance: the
  model load was paid for every sentence, and the repairs queued behind it accumulated.
  The resident service removes that cost rather than hiding it: the checkpoint is loaded
  when the *service* starts and never again, so the same repair is a 0.5-0.8 s request
  and a dictation begins in microseconds (the process-versus-resident measurements are
  in [measurements.md](measurements.md); the rationale is [decisions.md, D22](decisions.md)).
  The price is memory: both entries stay resident.

CPU with the more accurate repair (Qwen3) read by the resident service:

```sh
"$BIN" config set refine_model ~/models/Qwen3-ASR-0.6B-GGUF/qwen3-asr-0.6b-q8_0.gguf
```

`server install`, below, records that setting, `streaming_model` and `asr_server_url` as it
finds them, so nothing else has to be pointed at the service by hand.

`models list --refresh` is not optional bookkeeping: it is the only command that identifies a
checkpoint, and `server install` refuses to serve a model the cache has no answer for - exit
3, with `voiceime models list --refresh` as the fix it prints. Skipping it cannot leave a
half-working install; it leaves the command that needs it refusing, with the step named.

**The server is a deployment step of its own**, because dictation cannot work without
it: there is no process to fall back to, and no on-demand start. It is installed the same
way the shortcut listener is, and it loads both models at startup so a session only opens a
connection:

```sh
"$BIN" server install            # picks the models, records them, writes the unit, starts it
"$BIN" server status --json      # assert: data.unit_active == true, data.listening == true
                                 # data.served lists the files, which is how "which model is
                                 # streaming" is answered when streaming_model is empty
```

What it writes and where to look when something is wrong:

| Piece | Where | What it means |
|---|---|---|
| `audiocpp_server` | beside `audiocpp_cli` in the audio.cpp build tree | Nothing to install separately: the build produces it. A build without it makes `server install` fail rather than leaving dictation broken |
| generated config | `~/.config/voiceime/asr-server.json` | Two entries - `stream` (live partials) and `refine` (offline repair), ids fixed - with `lazy_load: false`. Rewritten by `voiceime server run` from the settings at every service start, so edit the settings, not this |
| unit | `~/.config/systemd/user/voiceime-asr-server.service` | `systemctl --user status voiceime-asr-server`, `journalctl --user -u voiceime-asr-server` |
| model families | the `models list --refresh` inspection cache | The server requires each model's audio.cpp family, from `audiocpp_cli --inspect`; an unknown family is a `server install` failure |

Two entries rather than one because the server holds a model for the whole length of a
request: a repair sent to the entry a live dictation owns waits for that dictation to
end (measured; [decisions.md, D22](decisions.md)). The two may point at the same GGUF -
that costs two resident copies, and is what a machine with one small model should do.

`server install` also **records** which checkpoints it chose: `streaming_model` and
`refine_model` are written into the configuration, so after deployment the file names what
is actually loaded instead of saying "pick one". Clear a setting (`config set
streaming_model ""`) and re-run install to have it pick again - for instance after
installing a better model.

#### Changing a model, or editing the file by hand

The configuration file is the single source of truth, so editing it directly is
legitimate. Three processes read it, each when it starts - the recognition service (which
models, the streaming window, threads), the engine (endpoints, language, the server
address), and the shortcut listener (the key) - and one command applies it to all three:

```sh
# either edit the file, or:
"$BIN" config set refine_model ~/models/Other-GGUF/other.gguf

"$BIN" config apply        # restarts the service and listener, rewrites the IBus
                           # component if the engine name changed, drops the engine
"$BIN" doctor              # asr_server must be ok
```

`config apply` prints what it did, including the models the service ended up serving. The
engine is *dropped* rather than restarted: ibus-daemon starts a fresh one on the next
activation, which is the same thing without disturbing the selected input method.

It also **validates the file before restarting anything**, and refuses to apply a setting
that cannot work - a `refine_model` path that does not exist is a `stat`, not a service
start, so the answer arrives before the running install is disturbed instead of as a
crash-looping unit that looks like a setting with no effect.

`config set` prints `voiceime config apply` as the next step, for every setting - and exits
non-zero (`3`) when the value it wrote leaves the file unusable, while still writing it, so
staging a model path before the download finishes stays possible. An edit that looks like
it did nothing is the failure this project keeps running into.

The unit runs `voiceime server run` rather than audio.cpp directly, which renders
`~/.config/voiceime/asr-server.json` from the settings before exec'ing the server -
the settings stay the only source of truth, and there is no generated file that can
disagree with it ([asr.md, the service](asr.md#the-service)). `config apply` waits
until the entries being served are the ones the settings name, because a restart that
has not finished loading is a service that still answers the old requests, and
`doctor` compares the *paths* being served with the paths configured, so a service
left on the previous checkpoint cannot hide: the entry ids never change, and the
symptom of a stale service is slightly different text rather than an error. All
settings the service reads at startup apply the same way
([AGENT.md, Configure](AGENT.md#configure)); `--stream-model` and `--repair-model` on
`server install` are one-off overrides that write settings, and a plain restart is
what applies them.

#### Why the server needs the model memory

The service keeps its entries resident (both loaded at once: about 1.4-1.9 GB,
[measurements.md, models on this machine](measurements.md#models-on-this-machine)),
so the machine holds both models all the time. That is the whole trade: a repair is a
0.5-0.8 s request instead of a 5 s process, and a dictation starts instead of
loading a model; the full rationale and the rejected alternatives are in
[decisions.md, D22](decisions.md). If that memory is not available,
`voiceime server uninstall` puts the machine back to "no dictation" rather than quietly
degrading - there is no slower route left to fall back to, by design.

**The GPU half of this table is an inference, not a measurement**
([measurements.md, not measured yet](measurements.md#not-measured-yet)): what is
measured is that the repair cost on CPU is dominated by weight loading,
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
"$BIN" doctor --json           # exit 0; every check ok. A `warn` is informational,
                               # never a failure: `backend` on a machine with a GPU
                               # whose audio.cpp was built for cpu is the usual one.
"$BIN" capabilities --json     # commands, settings and detected environment
```

Deliver-boundary check with no audio at all:

```sh
"$BIN" stream --file some.wav --realtime --events   # the real pipeline: drafts, repairs, timing
```

## 6. Verify the microphone path

```sh
"$BIN" stream --seconds 5 --events                 # live partials + endpointing
```

`--events` prints one JSON object per event, so latency and repair costs are
inspectable rather than anecdotal. To test the model and endpointing without a
microphone, replay a file at real time: `"$BIN" stream --file speech.wav --realtime`.

Measured behaviour worth knowing before you judge the output: a quiet room is not
digital silence, and the recogniser can emit a short hallucination when the noise
floor crosses the threshold - a real detector bug this project once had, since
fixed; the measurement and its history are in
[measurements.md, endpointing in a real room](measurements.md#endpointing-in-a-real-room).

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
Without it you get a file IBus never reads. The two measured daemon facts that make
this surprising - `IBUS_COMPONENT_PATH` **replaces** the default search path (so the
value always lists `/usr/share/ibus/component` too), and the daemon's component
registry is **cached** (so the stale `~/.cache/ibus/bus/registry` must be deleted
before the restart) - are in
[ibus-protocol.md, how the daemon learns about an engine](ibus-protocol.md#how-the-daemon-learns-about-an-engine).

Verify:

```sh
"$BIN" engine status --json
# assert: data.engine_registered == true, data.engine_visible == true
ibus engine voiceime && ibus engine            # switch, then read back
```

A healthy activation starts capture and connects to the recognition service - the
end-to-end proof that both work:

```sh
pgrep -af "ibus-engine --ibu[s]"
pgrep -af par[e]c                               # capture, a child of the engine
pgrep -af "voiceime server ru[n]"               # the service: one process, all dictations
ibus engine xkb:us::eng                         # switching away stops capture, not the service
```

The service is a user unit, not a child of the engine, so it is running whether or not
anyone is dictating. That is the point of it: no dictation pays a model load. The
bracketed patterns (`--ibu[s]`, `par[e]c`, `ru[n]`) are not decoration: `pgrep -f`
matches any command line that contains the pattern, so a raw pattern run from a shell
whose command line carries it - a script, an agent's `bash -c` - reports the prober as
the process. Step 10's probe carries the same guard, and [decisions.md, D31](decisions.md)
records the trap.

The engine's own session log goes to `~/.cache/voiceime/engine.log`, because the
daemon spawns it with stdout and stderr on pipes it discards. `voiceime engine status`
names the file; `tail -f` it while dictating to see each utterance, repair and commit.
For the D-Bus calls as well, run the engine by hand as described in `docs/AGENT.md`.

## 8. Start the shortcut listener

```sh
"$BIN" hotkey install           # unit, plus a GNOME keybinding on X11
"$BIN" hotkey status --json     # assert: data.service_active == true
                                # on X11 assert too: data.gnome_binding.installed == true
```

`hotkey install` also runs `systemctl --user daemon-reload` and
`enable --now`, and is idempotent. The default trigger is `<F9>`.

The key's source of truth is the configuration (`config set hotkey <combo>` then
`config apply`): the unit names no key of its own, so re-running `hotkey install`
never changes it, and the install converges the *mechanism* for the current session
type (on Wayland it removes a GNOME keybinding an X11 install left behind; on X11 it
adds one) - which is why a machine that switched session types must re-run it.
`--keys` is the one exception: `hotkey install --keys <combo>` bakes the combo into
the unit's `ExecStart`, where `hotkey run` prefers it over the configuration - a
one-shot pin, not a setting, and the next plain `hotkey install` rewrites the unit
and the trigger falls back to the configuration ([decisions.md, D26](decisions.md)
records why the unit used to bake the *configured* key and what that did to a
`config set`).

**The commands do not change; the mechanism does, and the choice is automatic.**
The listener registers the shortcut through IBus
(`org.freedesktop.IBus.GlobalShortcutKeys`), which the compositor honours on
**Wayland**. On **X11** nothing grabs that property - the key would reach the
focused application - so `hotkey install` also writes a **GNOME custom keybinding**,
because `gsd-media-keys` does grab keys there. Both sessions install the same unit
with the same commands; only the trigger differs:

| | Wayland | X11 |
|---|---|---|
| who grabs the key | the compositor | `gsd-media-keys` |
| who runs the toggle | the listener (`hotkey run`), in process | a GNOME keybinding that runs `voiceime hotkey toggle` |
| a **held** key | collapsed by the listener's `held` flag | collapsed by `hotkey toggle`'s repeat guard |
| any **install** of the key | *removes* any leftover GNOME keybinding | *adds* the GNOME keybinding |
| `hotkey status` says | `trigger: IBus global shortcut` | `trigger: GNOME keybinding` |

Both rows matter. On Wayland the keybinding is removed so two consumers never fight
over one key. And because the X11 trigger is a *command* rather than a listener,
`gsd-media-keys` runs it once per key event - **including every auto-repeat tick of a
held key** - so `hotkey toggle` carries its own guard: a second invocation within
700 ms is treated as the key repeating and ignored, which is what keeps a held F9 to
a single toggle (`docs/decisions.md` D19, `docs/measurements.md`). `hotkey status
--json` reports `data.session`, `data.mechanisms` and `data.gnome_binding`, so the
effective trigger is visible rather than assumed; `hotkey uninstall` removes both.

**Install it from inside the session.** `hotkey install` reads the session type from
`XDG_SESSION_TYPE`, falling back to `WAYLAND_DISPLAY`/`DISPLAY`. From a bare ssh or
console shell where none of those name a display server it installs only the unit -
no keybinding - and the key would do nothing on X11. It says so now: `data.session`
is reported and a `data.warning` asks you to re-run it from a terminal inside the
graphical session. `hotkey status` warns the same way.

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
"$BIN" doctor --json >/dev/null;                                    doctor=$?
"$BIN" engine status --json | grep -q '"engine_registered": *true';  engine=$?
"$BIN" hotkey status --json  | grep -q '"service_active": *true';    listener=$?
"$BIN" server status --json  | grep -q '"listening": *true';         server=$?
pgrep -f "ibus-engine --ibu[s]" >/dev/null;                        engine_process=$?
echo "doctor=$doctor engine=$engine listener=$listener server=$server engine_process=$engine_process"
[ "$doctor" = 0 ] && [ "$engine" = 0 ] && [ "$listener" = 0 ] && [ "$server" = 0 ]
```

`doctor=0`, `engine=0` and `listener=0` are the deployable state, and the closing
`[ ... ]` reports them as the snippet's own exit code - the earlier version of this
block echoed `ok` for each whatever the command returned, so it could not fail.
`engine_process` is deliberately not part of that test: the engine is spawned lazily
on the first activation, so it reads `1` on a fresh machine until you tap the key
once (`ibus engine voiceime` does it too). Once dictation has been activated, `0`
there is the end-to-end proof that the engine, capture and the recogniser all came
up. The pattern is `--ibu[s]` rather than `--ibus` on purpose: `pgrep -f` matches
any command line that contains the pattern - including this snippet's own shell,
the same trap [decisions.md, D31](decisions.md) recorded for the `pkill` it
replaced. `doctor`'s `asr_server` line is part of `doctor=0`: it fails when no server is configured
or nothing answers at the address, and there is no per-utterance fallback for that to hide
behind - a missing service is a dictation that refuses to start. `doctor` also carries the
model check, so a file nobody has run `models list --refresh` for is reported there with
that command as its fix. The one thing no command can assert is a human seeing text appear in a focused
window; if that is the goal, dictate one sentence into a text editor and watch for
it.

---

## Updating an installed copy

This list serves two situations: rebuilding the same version, and upgrading from an
older release. They differ less than they look: every step is idempotent, `"$BIN"` is
always the *new* binary, so a component an older install never had - the recognition
server unit is the current example - is installed by the same step a fresh machine
runs. What the list cannot know is what this machine is missing; that is the closing
`doctor`, whose failing checks each carry the `fix` command - the step this version
needs on this machine, run until it exits 0. `config.toml` needs no migration of its
own ([step 3](#3-configure)): a new version's settings take their defaults on load.
The numbered path above stays the reference for fresh machines and for what each step
does.

`make build && make install` replaces the binary and nothing else happens: a
process keeps the binary it was started from, so the code you just built is not
running until the process is. Both halves of that are counter-intuitive, so both
were measured on the reference machine rather than reasoned about.

```sh
make build && make install                 # replaces ~/.local/bin/voiceime, ~/.local/docs and the OpenCC licence
"$BIN" server install                      # rewrites the config and restarts the service, waiting for the new build
"$BIN" engine install --apply              # component, activation env, ibus restart
"$BIN" engine status --json                # assert: engine_registered == true
"$BIN" hotkey install                      # safe to re-run; converges the trigger for the current session type
"$BIN" doctor --json                       # exit 0, and the process checks ok; on an upgrade its `fix` lines name
                                            # the steps this version adds and this machine does not have yet
ibus engine voiceime                       # only if dictation was on before
# the step 10 acceptance block is the gate: doctor, engine, listener and server each assert 0
```

(`$BIN` is the installed binary, as in step 2. Run `doctor` from that copy, not
from the checkout: the check compares the running processes against the binary
running `doctor`, so the checkout build reports a difference that is real but
expected.)

What each step actually does:

* **`install` neither fails on nor disturbs a running copy.** The destination is
  replaced in place - there is no `Text file busy` - and the process started from
  it keeps the old inode, so `/proc/<pid>/exe` then reads
  `/home/u/.local/bin/voiceime (deleted)`. That is exactly what `doctor` reports as
  a `warn` on `engine_process`, `hotkey_process` and `server_process` ("still running the previous
  build"). So that warning is the *expected* state between the two steps, not a
  fault to hunt, and nothing has to be stopped before installing.
* **`engine install --apply` ends both running processes, and only one of them is
  yours to restart.** It restarts `ibus-daemon`, and the engine and the listener
  are both connected to the daemon's *own* socket (resolved from `IBUS_ADDRESS` or
  the registry file, not the session bus), so both lose it. The engine process
  exits and IBus respawns it lazily on the next activation - which is why the last
  command above exists: `ibus restart` also resets the selected engine to the
  default, so dictation has to be turned back on. The listener exits with
  `status=1` and its own unit starts it again (`Restart=on-failure`,
  `RestartSec=2`), re-registering the shortcut on the way up. Measured: after
  `ibus restart` the unit's `NRestarts` went 0 → 1 and the new process logged its
  startup line about two seconds later - so on a systemd session the listener
  restart is not yours to do. It *is* yours when the listener was started by hand
  (there is no unit to restart it), or when it is alive-but-stale: the
  `make install` without a daemon restart case at the top of this list.

The shortcut is the one thing no command can confirm. The registration is a
write-only property on the daemon, so `hotkey status` can confirm that the listener
is *active* and what was asked for, but not what the daemon actually holds; whether
the compositor grabs the key is a property of the session and is settled by
pressing it.

To restart the engine and the listener without touching the daemon - which is what
`doctor` offers as the fix when the stale-process *warning* is what you noticed:

```sh
"$BIN" config apply                        # engine by pid, then the listener
ibus engine voiceime                       # only if it was dictating
```

`config apply` sends SIGTERM and waits for the engine to commit the sentence in progress
(D25), then restarts the listener; the engine comes back on the next activation. It replaced
`pkill -f "voiceime ibus-engine"` here, which matches any process whose command line merely
mentions that string - a shell running a command that contains it, for one (D31).

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
`engine_process` and `hotkey_process` ("still running the previous build"). The
procedure is [Updating an installed copy](#updating-an-installed-copy); the short
version is `voiceime config apply`, then `ibus engine voiceime` if it was dictating.

### The key is swallowed but nothing happens

The listener is the only component with no automatic repair. Check
`systemctl --user status voiceime-hotkey`, then `voiceime hotkey run --log-events`
by hand. If a previous listener died without clearing its registration, the key is
still held by the daemon: `ibus restart` clears it (`voiceime hotkey uninstall`
does exactly this).

### Everything is configured but the first partial never arrives

`voiceime stream --seconds 5 --events` isolates this: if it produces text, the
problem is delivery (IBus, step 7) or the trigger (step 8); if it produces nothing,
the problem is capture or the model (`voiceime doctor` names the source it would use).

## Uninstall

```sh
"$BIN" hotkey uninstall              # stops the listener and restarts IBus, releasing the key
"$BIN" engine uninstall              # removes the component file
make uninstall                       # removes the binary, docs and OpenCC licence
"$BIN" models uninstall sensevoice_small_q8
"$BIN" server uninstall              # stops and removes the service and its config
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
* **Other desktop environments.** Only GNOME has been used. A non-GNOME session
  gets no custom keybinding, and on X11 that leaves no trigger at all.
* **X11.** Every figure in this project was measured on GNOME/Wayland. The X11 path
  exists and works - the trigger is a GNOME keybinding there, and the repeat guard in
  `docs/decisions.md` D19 exists because a held key fired the command dozens of times
  - but it is the less-travelled half, and it was the half with the defect.
* **Multi-user machines.** IBus component paths and the wire format for committed
  text are recorded in `docs/ibus-protocol.md`.
* **The server's own lifetime.** It is a systemd user unit, so it is started at login
  and stopped at logout like the shortcut listener; nothing here restarts it for you if
  the audio.cpp build is replaced underneath it. `systemctl --user restart
  voiceime-asr-server` is the step, and `server status` says whether the entries it
  serves are the ones the client asks for.
