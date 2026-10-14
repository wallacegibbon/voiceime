# The IBus engine protocol, as established by experiment

IBus does not document its D-Bus protocol for third-party engines, and the
Python/C bindings hide all of it. Everything below was measured against a live
daemon on the reference machine, not inferred from source. It is recorded because
each fact cost time to find and would cost the same again.

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
      <longname>Voiceime (speech)</longname>
      <description>...</description>
      <rank>1</rank>
      <symbol>&#x1F3A4;</symbol>
    </engine>
  </engines>
</component>
```

## The engine process contract

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
accepted (and is what a plain commit uses).

## Verified path

Reproduced end to end on a sandbox daemon:

1. Component XML written to a directory named by `IBUS_COMPONENT_PATH`.
2. `ibus list-engine` shows `voiceime - Voiceime (speech)`.
3. `ibus engine voiceime` makes the daemon **spawn the `<exec>` command**, here
   the Go binary (`voiceime ibus-engine --ibus`).
4. The process takes the component name, the daemon calls `CreateEngine`, and the
   engine receives `Enable`.
5. The engine emits `CommitText` with the structure above — captured on the bus.

Not yet verified: the final hop into a focused application's text field. That
needs a real client on the session bus (a GTK entry receiving the text), which is
the next measurement to take, and the reason `voiceime ibus-engine` currently
accepts `--commit-on-enable`.
