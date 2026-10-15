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
make check          # exactly what CI runs: gofmt, vet, tests
```

## 2. Put the binary somewhere permanent

**This step matters more than it looks.** The IBus component file and the systemd
unit both embed the *absolute path* of the binary. If you move or rename either the
binary or the checkout afterwards, dictation and the shortcut silently break - see
[Recovery](#recovery). Pick one location and stay there.

```sh
# either use it in place
"$BIN" --help

# or install it system-wide
sudo make install                     # -> /usr/local/bin/voiceime
export BIN=/usr/local/bin/voiceime
```

## 3. Configure

The configuration file is the only source of truth (`voiceime config path` prints
its location, `~/.config/voiceime/config.toml` by default).

```sh
"$BIN" config init                    # idempotent; refuses to overwrite without --force
"$BIN" config set audio_cpp_dir ~/playground/audio.cpp
"$BIN" config set models_dir ~/models
"$BIN" config set language zh         # your language; empty asks the model to detect
"$BIN" config set threads 4           # physical cores is a good starting point
"$BIN" config validate --json         # exit 0, or exit 3 with a fix per issue
```

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

```sh
"$BIN" models recommended --json                       # what to install, and why
"$BIN" models sizes sensevoice_small_q8 --json         # reachability + size, downloads nothing
"$BIN" models install sensevoice_small_q8              # ~254 MiB into models_dir
"$BIN" models installed --json                         # local state, no network
```

`sensevoice_small_q8` is the measured default: multilingual streaming, about 20x
realtime on a laptop CPU (`docs/measurements.md`). On a machine where a repair pass
slower than speech is acceptable, add `qwen3_asr_0_6b_q8_0` and point
`refine_model` at it; otherwise leave `refine_model` empty so repairs reuse the
streaming model.

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
floor crosses the threshold - a six-second capture in a silent room produced
`被。` and `嗯。` here, and `transcribe` on a file of pure silence produced `我。`
too. That is the recogniser, not the pipeline, and the knobs are:

```sh
"$BIN" config set endpoint_min_speech_ms 500   # ignore short noise bursts (default 300)
"$BIN" config set silence_rms_threshold 500    # for a room with a high noise floor (default 350)
```

Raise one at a time and re-run `stream --seconds 6 --events` to see the effect:
what you want is no `segment` event at all while the room is quiet.

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
"$BIN" hotkey install           # writes ~/.config/systemd/user/voiceime-hotkey.service
"$BIN" hotkey status --json     # assert: data.service_active == true
```

`hotkey install` also runs `systemctl --user daemon-reload` and
`enable --now`, and is idempotent. The default trigger is `<F9>`.

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
appears to succeed until it is restarted. Fix by re-running the installers from the
new location, in this order:

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

A running engine keeps running the binary it was started from. After rebuilding,
end it and let the daemon respawn it:

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
sudo make uninstall                  # removes /usr/local/bin/voiceime, if installed there
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
