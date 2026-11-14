# The IBus engine protocol, as established by experiment

IBus does not document its D-Bus protocol for third-party engines, and the
Python/C bindings hide all of it. Everything below was measured against a live
daemon on the reference machine, not inferred from source. It is recorded because
each fact cost time to find and would cost the same again.

**Scope.** Measured facts about the IBus daemon's protocol for third-party engines. How
voiceime installs and drives the engine is in [AGENT.md](AGENT.md); the recognition path is
[asr.md](asr.md).

## Reproducing the experiments safely

Never experiment against the session daemon. A malformed `RegisterComponent` call
**makes ibus-daemon abort** (reproduced twice: the connection drops and D-Bus
restarts the daemon; the user's input method flickers with it). Use a private
daemon:

```bash
# 1. A private session bus.
SADDR=$(dbus-daemon --session --print-address --fork --address=unix:path=/tmp/vs-dbus)

# 2. A private ibus-daemon on its own bus, config dir and display.
env -u WAYLAND_DISPLAY DISPLAY=:99 \
    XDG_CONFIG_HOME=/tmp/vs-cfg \
    DBUS_SESSION_BUS_ADDRESS="$SADDR" \
    IBUS_COMPONENT_PATH="/tmp/vs-components:/usr/share/ibus/component" \
    setsid ibus-daemon -d --panel disable --config disable \
        --address unix:path=/tmp/vs-ibus -t none --verbose

# 3. Talk to it. Every ibus CLI call honours IBUS_ADDRESS.
env IBUS_ADDRESS=unix:path=/tmp/vs-ibus ibus list-engine | grep voiceime
env IBUS_ADDRESS=unix:path=/tmp/vs-ibus ibus engine voiceime

# 4. Watch the wire.
dbus-monitor --address unix:path=/tmp/vs-ibus
```

`-d` (daemonize) is required: without it the daemon exits when the launching shell
does ("The parent process died"). Setting `DISPLAY=:99` and unsetting
`WAYLAND_DISPLAY` is also required, otherwise the daemon refuses to start with
"current session already has an ibus-daemon".

## Finding the daemon's bus

IBus runs on its own private bus, not the session bus.

* `IBUS_ADDRESS` wins if set, which is what libibus itself does.
* Otherwise the daemon writes a registry file at
  `$XDG_CONFIG_HOME/ibus/bus/<machine-id>-<display>` containing
  `IBUS_ADDRESS=...` and `IBUS_DAEMON_PID=...`.
* **The file can be stale and `ibus address` can print a stale value.** Observed
  directly: `ibus address` returned `dbus-x2I4qrSB` while the live daemon's file
  named `dbus-qBM9imL4`, and connecting to the former was refused. Address
  resolution therefore pings every candidate and keeps the one that answers.

## How the daemon learns about an engine

From component XML files, not from the engine process:

* It scans `/usr/share/ibus/component` (and, on a pristine system, the
  `ibus/component` directory of each `XDG_DATA_DIRS` entry).
* It does **not** scan `~/.local/share/ibus/component`. A component placed there
  is invisible even after `ibus write-cache` and `ibus restart` — verified: the
  regenerated registry listed only `/usr/share/ibus/component/*.xml`.
* `IBUS_COMPONENT_PATH` is consulted, and it **replaces** the default search path
  rather than adding to it. Measured on a sandbox daemon:

  | `IBUS_COMPONENT_PATH` | engines listed |
  |---|---:|
  | unset | 1270 |
  | `/tmp/vs-components` | 2 (ours only — every system engine disappeared) |
  | `/tmp/vs-components:/usr/share/ibus/component` | 1271 (both) |

  So an install that uses this variable must list the system directory too, and it
  must be set in the *activation* environment before the daemon starts:

  ```bash
  dbus-update-activation-environment --systemd IBUS_COMPONENT_PATH="$HOME/.local/share/voiceime/component:/usr/share/ibus/component"
  ibus restart
  ```

* **The activation environment lasts one session.** `dbus-update-activation-environment`
  edits the environment of the *running* session. The next login starts `ibus-daemon`
  from systemd without the variable, the component directory is not scanned, and the
  engine is gone from `ibus list-engine` although the file is still there. To outlive a
  login the value has to be read when the session starts, which is
  `~/.config/environment.d/voiceime.conf`: the systemd user manager reads that directory,
  and on GNOME `ibus-daemon` runs as the user service
  `org.freedesktop.IBus.session.GNOME.service`. `voiceime engine install --apply` writes
  it, `engine uninstall` removes it, and `doctor`'s `component_env` finding names it when
  it is missing.
* **The component registry is cached**, and the cache records the directories the
  daemon watched when it was written. A daemon started with a cache never looks at
  a newly configured directory: with `IBUS_COMPONENT_PATH` already in the daemon's
  environment (confirmed by reading `/proc/<pid>/environ`), the engine stayed
  invisible across `ibus restart` and appeared the moment
  `~/.cache/ibus/bus/registry` was removed. `voiceime engine install --apply` now
  removes that file before restarting, and this is the step that makes a user
  install actually take effect.
* Adding a component file to an already-watched directory is **not** picked up
  live: a new XML written into the watched directory did not appear in
  `list-engine` after three seconds. A cache-invalidating restart is required.
* An engine process does **not** call `RegisterComponent` on startup. The pinyin
  engine was killed and re-spawned while monitoring the bus, and no registration
  call was captured: the daemon already knows the engine from the XML.

## The component file

Parsed fields, taken from a working component (`/usr/share/ibus/component/pinyin.xml`)
and verified by loading a generated copy into a sandbox daemon:

```xml
<component>
  <name>org.freedesktop.IBus.Voiceime</name>   <!-- also the engine's bus name -->
  <description>...</description>
  <exec>/path/to/voiceime ibus-engine --ibus</exec>  <!-- spawned on activation -->
  <version>...</version>
  <author>...</author>
  <license>...</license>
  <homepage>...</homepage>
  <textdomain>...</textdomain>
  <engines>
    <engine>
      <name>voiceime</name>      <!-- what `ibus engine voiceime` selects -->
      <language>zh</language>
      <license>...</license>
      <author>...</author>
      <layout>default</layout>
      <longname>Voice</longname>
      <description>...</description>
      <rank>1</rank>
      <symbol>&#x1F3A4;</symbol>
    </engine>
  </engines>
</component>
```

## The engine process contract

0. **The bus name the engine owns must be the component `<name>`.** A mismatch is
   silent on the engine's side and a timeout on the daemon's: the daemon spawns
   the `<exec>` and then waits for a name nobody claims
   (`SetGlobalEngine: Timeout was reached`). Found the hard way with a probe
   component whose name differed from the hardcoded default, which is why
   `voiceime ibus-engine` takes `--name`.
1. Connect to the private bus and take the well-known name equal to the component
   `<name>` (`org.freedesktop.IBus.Voiceime`).
2. Export `/org/freedesktop/IBus/Factory` implementing
   `org.freedesktop.IBus.Factory`:
   `CreateEngine(in s name) → (out o object_path)`.
3. Export the object path it returned as `org.freedesktop.IBus.Engine`. The
   daemon calls methods on it and listens for its signals.
4. Answer `org.freedesktop.IBus.Service.Destroy` to be shut down, and
   `org.freedesktop.DBus.Properties` (`ContentType` is written, `FocusId` and
   `ActiveSurroundingText` are read).

The engine interface, extracted from the installed `libibus-1.0.so.5` (which
embeds the introspection XML the daemon itself uses):

*Methods:* `ProcessKeyEvent(u,u,u)→b`, `SetCursorLocation(i,i,i,i)`,
`ProcessHandWritingEvent(ad)`, `CancelHandWriting(u)`, `SetCapabilities(u)`,
`PropertyActivate(s,u)`, `PropertyShow(s)`, `PropertyHide(s)`,
`CandidateClicked(u,u,u)`, `FocusIn()`, `FocusInId(s,s)`, `FocusOut()`,
`FocusOutId(s)`, `Reset()`, `Enable()`, `Disable()`, `PageUp()`, `PageDown()`,
`CursorUp()`, `CursorDown()`, `SetSurroundingText(v,u,u)`,
`PanelExtensionReceived(v)`, `PanelExtensionRegisterKeys(v)`.

*Signals:* `CommitText(v)`, `UpdatePreeditText(v,u,b,u)`,
`UpdateAuxiliaryText(v,b)`, `UpdateLookupTable(v,b)`, `RegisterProperties(v)`,
`UpdateProperty(v)`, `ForwardKeyEvent(u,u,u)`, `PanelExtension(v)`. libibus also
knows `delete-surrounding-text` and `require-surrounding-text` as GObject signal
names, but they are not in the introspection XML, so their wire form is
unverified and voiceime does not depend on them.

Only `Enable`, `Disable` and `ProcessKeyEvent` carry real meaning for a dictation
engine; the rest are accepted and ignored.

## Key events arrive on both edges

A dictation that lasts until the user lets go needs the *release*, and the engine gets
it: `ProcessKeyEvent(u keyval, u keycode, u state)` is the same call for both edges, and
the state carries `IBUS_RELEASE_MASK` on the way up. The masks are libibus's own, not
X11's (`ibustypes.h`, 1.5.29):

| Bit | Meaning |
|---|---|
| `1 << 0` | Shift |
| `1 << 1` | Lock (Caps Lock) |
| `1 << 2` | Control |
| `1 << 3` | Mod1 (Alt) |
| `1 << 6` | Mod4 (Super) |
| `1 << 30` | **release** (`IBUS_RELEASE_MASK`) |

**Measured: a real client sends both edges.** A live session's engine receives the user's
own keystrokes through this call, and they include releases with the bit set - captured in
an engine trace while the engine was the selected input method:

```
ProcessKeyEvent(105,23,1073741824)     ← 'i' released          (105 = XK_i)
ProcessKeyEvent(32,57,1073741824)      ← space released        (32 = XK_space, keycode 57)
ProcessKeyEvent(65288,14,1073741824)   ← BackSpace released    (65288 = XK_BackSpace)
ProcessKeyEvent(65507,29,0)            ← Ctrl pressed          (65507 = XK_Control_L)
```

`1073741824` is `1 << 30`. So the release edge is not an inference from the header: the
daemon routes it, and a hold can be read from it. Two things were *not* observed, and are
not claimed: a human holding the trigger (the presses in the trace above are a real
client's typing, not ours), and a client that withholds a release. The end-to-end run -
one recording opened by a press of the trigger and decoded after its release - is in
[measurements.md](measurements.md#hold-to-talk-end-to-end).

Two consequences shape voiceime's use of it :

* **Only the trigger key's own events are consumed** (`ProcessKeyEvent` returns true for
  them and false for everything else). An engine that consumed a modifier's press but not
  its release would leave that modifier latched in the application - the failure measured
  with the daemon's global shortcut (D13) - so modifiers are watched and always passed
  through.
* **A lost release ends nothing**, so the recording is capped by
  `max_utterance_seconds` regardless: the cap is what keeps a missed key event from
  holding a microphone open.

## The wire format of committed text

Captured with `dbus-monitor` while an engine committed `你好`:

```
signal path=/org/freedesktop/IBus/Engine/1; interface=org.freedesktop.IBus.Engine; member=CommitText
   variant       struct {
         string "IBusText"        <- type name first, as ibus serialises everything
         array  []                <- a{sv} attachments
         string "你好"             <- the text
         variant struct {
               string "IBusAttrList"
               array  []
               array  []
            }
      }
```

So the signatures are:

* **IBusText** = `(sa{sv}sv)` — type name, attachments, text, variant holding an
  attribute list.
* **IBusAttrList** = `(sa{sv}as)` — type name, attachments, attributes.

`UpdatePreeditText` carries the same text variant plus
`(u cursor_pos, b visible, u mode)`; mode 0 means "replace the composition".

Attributes are what draws the preedit underline; an empty attribute list is
accepted (and is what a plain commit uses), but on X11 it means **no underline**:
GTK's IM module renders the preedit itself and takes the underline from these
attributes alone. Measured here: the engine had sent an empty list since it was
written and nobody noticed, because on Wayland the compositor's text-input path
marks a composition by itself. The same engine, same signals, differs by session
type — so this is the one part of the payload that has to be built rather than
left to the client.

An attribute is `(sa{sv}uuuu)` — type name, attachments, type, value, start index,
end index. Taken from libibus 1.5.29 itself, by serialising one every-character
underline over a six-character string with its own `serialize_object`:

```
('IBusAttribute', @a{sv} {}, uint32 1, uint32 1, uint32 0, uint32 6)
```

* type `1` is `IBUS_ATTR_TYPE_UNDERLINE`, value `1` is `IBUS_ATTR_UNDERLINE_SINGLE`.
* The indices count **characters**, not bytes (`6` above is six Chinese
  characters, eighteen bytes), and `end_index` is exclusive. A byte count would
  underline past the end of the text.
* The attributes sit inside the `IBusAttrList`'s `as`, each element a variant
  wrapping the struct.

Verified round trip on a private daemon: the engine emitted

```
('IBusText', @a{sv} {}, 'Good morning. Could you tell me …', <('IBusAttrList', @a{sv} {}, [<('IBusAttribute', @a{sv} {}, uint32 1, uint32 1, uint32 0, uint32 58)>])>)
```

and a minimal client that created an input context for it received the same text
with the same attribute — the daemon had deserialised and re-serialised it, which
is the part that can fail silently if the struct is wrong.

## Verified path

Reproduced end to end on a sandbox daemon:

1. Component XML written to a directory named by `IBUS_COMPONENT_PATH`.
2. `ibus list-engine` shows `voiceime - Voice`.
3. `ibus engine voiceime` makes the daemon **spawn the `<exec>` command**, here
   the Go binary (`voiceime ibus-engine --ibus`).
4. The process takes the component name, the daemon calls `CreateEngine`, and the
   engine receives `Enable`.
5. The engine emits `CommitText` with the structure above — captured on the bus.

The same path was then exercised against the **live session** daemon (not just the
sandbox), with a trace of incoming calls recorded by the engine:

```
CreateEngine("voiceimetest")
Properties.GetAll(org.freedesktop.IBus.Engine)
Properties.Get(org.freedesktop.IBus.Engine.FocusId)
Properties.Get(org.freedesktop.IBus.Engine.ActiveSurroundingText)
Enable()
Properties.Set(org.freedesktop.IBus.Engine.ContentType)
```

After activation, `ibus engine` reported the engine as current and the trace
filled with the *user's own keystrokes* (`ProcessKeyEvent`, `SetCursorLocation`)
— proof that GNOME routes a real application's input through an engine process
that speaks this protocol, and that answering the methods above is sufficient to
be that engine.

Not yet verified: text visibly landing in an application's field. The GTK entry
probe used for it never received window focus (its `notify::is-active` stayed
false), so there was no focused input context to commit into - the same thing was
measured again on a Wayland session: a GTK4 window created from a shell is mapped but
never activated, and Wayland gives no way to focus it from there.

What *is* measured is the signal path up to that boundary, with `dbus-monitor` on the
daemon's own bus while an engine dictated from a WAV file:

```
:1.145  path=/org/freedesktop/IBus/Engine/1  member=UpdatePreeditText   "嗯。"
:1.145  path=/org/freedesktop/IBus/Engine/1  member=CommitText
   ↓ the daemon forwards, to the input context it has focused
org.freedesktop.IBus  path=/org/freedesktop/IBus/InputContext_3
   member=UpdatePreeditTextWithMode  "嗯。"
org.freedesktop.IBus  path=/org/freedesktop/IBus/InputContext_3
   member=CommitText
```

So the engine emits, the daemon routes, and a focused input context receives - the same
signals a working engine (pinyin) produces on the same machine. What has not been
observed is a *window* consuming them, which is IBus's client library and the
application's business rather than this protocol's.
