#!/bin/sh
# asr-service.sh - put a model from this machine where the engine can use it.
#
# Nothing here is a feature of the program. The program is the IBus engine: it reads one
# TOML file and posts one recording to one address. What this script writes are deployment
# records - an audio.cpp config naming one checkpoint, a systemd unit that starts it, and the
# two settings keys that say what this machine's address is and what to name in it - and all
# of them are answered by looking at this machine once: which audio.cpp build is here, what
# that build supports, how many cores the CPU has, which checkpoint was named on the command
# line.
#
# This is the optional half of a voiceime setup, and it is separable on purpose: the engine
# needs one thing, an address that answers a POST with the recording. A local model is how
# this project satisfies that without a network, not a part of what the engine is.
#
# Usage: scripts/asr-service.sh /path/to/audiocpp_server /path/to/model.gguf
#
# The checkpoint is named here rather than in the settings because it is a deployment fact:
# which file is on this disk. What the settings own is the address the engine posts to - and
# the id this script serves it under, because nothing else in this design is in a position to
# know that id.
#
# It is idempotent: run it again after changing the checkpoint or rebuilding audio.cpp, and
# it rewrites only what differs. The last thing it prints is the proof, which is also what
# anyone can check afterwards without a tool of ours: what the service answers on /v1/models.

set -eu

if [ $# -ne 2 ] || [ ! -x "$1" ]; then
	echo "usage: scripts/asr-service.sh /path/to/audiocpp_server /path/to/model.gguf" >&2
	exit 2
fi
server=$1
model=$2
gguf=${server%audiocpp_server}audiocpp_gguf
settings=${XDG_CONFIG_HOME:-$HOME/.config}/voiceime/config.toml
generated=${XDG_DATA_HOME:-$HOME/.local/share}/voiceime/asr-server.json
unit_dir=${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user
unit=$unit_dir/voiceime-asr-server.service

# What the settings say: the one thing they own about recognition is the address the engine
# posts to. Reading one key out of a file we write ourselves is not parsing a format we do not
# control. Only the authority part is looked at - the path is the engine's business, and this
# program appends nothing to what it is given.
read_setting() {
	sed -n -e "s/^$1[[:space:]]*=[[:space:]]*\"\(.*\)\"[[:space:]]*\$/\1/p" "$settings" | head -n 1
}
# No settings file is what the engine itself sees on a fresh machine: defaults. So the
# address is the default one, and this says so instead of letting sed report the absence.
base_url=
if [ -f "$settings" ]; then
	base_url=$(read_setting asr_server_url)
fi
[ -n "$base_url" ] || base_url=http://127.0.0.1:18080/v1/audio/transcriptions
authority=${base_url#*://}
authority=${authority%%/*}
host=${authority%:*}
if [ "$host" = "$authority" ]; then
	host=127.0.0.1
	port=18080
else
	port=${authority##*:}
fi
root="http://$host:$port"
request_url="$root/v1/audio/transcriptions"

# The settings can name a server that is not this machine - a provider's endpoint, another
# host. Then nothing belongs here: starting a model on this box would contradict the
# settings, not complete them. This is the whole of the "the local model is optional" case.
case "$host" in
127.0.0.1 | localhost | ::1 | \[::1\] | 127.*) ;;
*)
	echo "$base_url is not this machine: no model is started here."
	echo "the engine needs only that this address answer a POST with the recording."
	exit 0
	;;
esac

[ -f "$model" ] || { echo "$model is not a file" >&2; exit 3; }

# What the model is. audio.cpp wrote the family into the checkpoint, and its own tool reports
# it; the server's config requires id, family and path and defaults everything else.
[ -x "$gguf" ] || { echo "$gguf is not executable: it travels beside the server" >&2; exit 3; }
family=$("$gguf" --inspect "$model" | sed -n 's/^model_spec_family=//p')
[ -n "$family" ] || { echo "$model carries no audio.cpp model spec: convert it with audio.cpp first" >&2; exit 3; }
entry=${model##*/}
entry=${entry%.*}

# What this machine is. The build's own banner is the only authority on which backends it
# carries, and the CPU's topology on how many workers fit in it.
backends=$("$server" --version | sed -n 's/^backends: //p')
case " $backends " in
*" cpu "*) backend=cpu ;;
*) echo "this audio.cpp build offers no cpu backend (it offers: $backends)" >&2; exit 3 ;;
esac
threads=$(awk -F': *' '/^physical id/{p=$2} /^core id/{print p"/"$2}' /proc/cpuinfo | sort -u | wc -l)
if [ "$threads" -lt 1 ] 2>/dev/null; then threads=$(nproc); fi

umask 077
mkdir -p "$(dirname "$generated")" "$unit_dir"

# Each file is written whole and compared before it replaces anything, so running this again
# says what changed and leaves a unit that already reads correctly untouched.
write_if_changed() {
	target=$1
	cat >"$target.new"
	if [ -f "$target" ] && cmp -s "$target.new" "$target"; then
		rm -f "$target.new"
		echo "unchanged: $target"
	else
		mv "$target.new" "$target"
		echo "wrote:     $target"
	fi
}

write_if_changed "$generated" <<JSON
{
  "host": "$host",
  "port": $port,
  "backend": "$backend",
  "threads": $threads,
  "lazy_load": false,
  "max_loaded_models": 0,
  "ui": false,
  "ui_management": false,
  "models": [
    {
      "id": "$entry",
      "family": "$family",
      "path": "$model",
      "task": "asr",
      "mode": "offline",
      "default_request_options": {
        "audio_chunk_mode": "none"
      }
    }
  ]
}
JSON

# The unit names the binary and the config, and links the manual that is installed beside the
# engine's binary, so `systemctl status` points at an explanation.
bindir=$(dirname "$(command -v voiceime || echo /nonexistent)")
documentation=""
manual=$(realpath -m "$bindir/../docs/AGENT.md" 2>/dev/null || true)
if [ -n "$manual" ] && [ -f "$manual" ]; then
	documentation="Documentation=file://$manual
"
fi

write_if_changed "$unit" <<UNIT
[Unit]
Description=voiceime recognition server (audio.cpp ASR, model resident)
$documentation
[Service]
Type=simple
ExecStart=$server --config $generated --log
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
UNIT

systemctl --user daemon-reload
systemctl --user enable voiceime-asr-server.service
systemctl --user restart voiceime-asr-server.service

# "Started" is not "serving the checkpoint named on the command line": the model has to load,
# and a server still running the previous config answers /health too. So this waits for the
# entry, which is the assertion anyone can repeat afterwards with curl.
tries=0
while [ "$tries" -lt 240 ]; do
	if curl -sf "$root/v1/models" | grep -q "\"id\": *\"$entry\""; then break; fi
	tries=$((tries + 1))
	sleep 0.5
done
if [ "$tries" -ge 240 ]; then
	echo "the service is not serving $entry: systemctl --user status voiceime-asr-server.service" >&2
	exit 1
fi

# The last record is the settings, and it is written last because it is only true once the
# model is actually serving. A program that never asks an address what it serves has to be
# told, and the program that chose this id is this one. Two keys, both about this machine: the
# whole address a recording is posted to, and the id to name in the request. The id goes first
# so that the settings are never left holding an address with nothing to ask it for.
voiceime=${VOICEIME_BIN:-$(command -v voiceime || true)}
if [ -z "$voiceime" ]; then
	echo "no voiceime on PATH: write these two keys in $settings yourself:" >&2
	echo "  asr_server_url = \"$request_url\"" >&2
	echo "  model = \"$entry\"" >&2
else
	record() {
		if ! out=$("$voiceime" --config "$settings" config set "$1" "$2" 2>&1 >/dev/null); then
			echo "the model is serving, but $1 could not be recorded in $settings: $out" >&2
			exit 1
		fi
	}
	record model "$entry"
	record asr_server_url "$request_url"
fi

echo "serving: $entry ($backend, $threads threads) from $model"
echo "settings: asr_server_url = $request_url, model = $entry"
