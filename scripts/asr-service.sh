#!/bin/sh
# asr-service.sh - put models from this machine behind an address a client posts to.
#
# One or more GGUF checkpoints and one audio.cpp build become one long-running service: an
# audio.cpp config naming those checkpoints, a systemd user unit that starts it, and the two
# facts a client has to be told - the address a recording is posted to and the id to name in
# the request. Every one of those is answered by looking at this machine once: which audio.cpp
# build is here, what that build supports, how many cores the CPU has, which checkpoints were
# named on the command line.
#
# What a client asks of a model service is small, and it is all this script promises: a POST
# to an address carrying a recording and a model id comes back with text.
#
# This script is in this project and is not part of this program. Nothing the engine runs reads
# it, no setting names it, and no path of the service it writes appears in the settings: the
# engine recognizes a model service by the binary that service's own unit runs, and reads the
# rest from that unit. The separation the engine keeps is the same one the file's location
# does not remove. What is shared is one POST and two lines of the settings file - and that is
# why the script lives here rather than somewhere else: it asks this project's own program for
# the address it is being asked to answer, in the machine-readable form that program publishes.
#
# Usage: scripts/asr-service.sh /path/to/audiocpp_server /path/to/model.gguf [model.gguf …]
#
# The checkpoints are named here rather than in a client's settings because they are deployment
# facts: which files are on this disk. Nothing searches for them and nothing downloads one - a
# disk with several checkpoints in it is exactly the case where guessing picks one quietly and
# the user finds out from the words appearing at their cursor.
#
# The first checkpoint named is the one that starts loaded, because naming it first is saying
# "this is what I use". The rest are registered and load on first use, which is what makes
# switching one line in the client's settings instead of redeploying; what a cold switch costs
# is measured in docs/measurements.md.
#
# It is idempotent: run it again after changing the checkpoints or rebuilding audio.cpp, and it
# rewrites only what differs. The last thing it prints is the proof, which is also what anyone
# can check afterwards without a tool of ours: what the service answers on /v1/models.
#
# The settings belong to whoever keeps them: ~/.config/voiceime/config.toml, unless
# ASR_CLIENT_SETTINGS names another. Nothing here writes that file - it is read for the
# address this service is asked to answer, and the two lines the service needs are printed
# for whoever keeps that file, once the models are actually serving.

set -eu

if [ $# -lt 2 ] || [ ! -x "$1" ]; then
	echo "usage: $0 /path/to/audiocpp_server /path/to/model.gguf [model.gguf …]" >&2
	exit 2
fi
server=$1
shift
here=$(dirname "$(readlink -f "$0")")
gguf=${server%audiocpp_server}audiocpp_gguf
# The nested form ${ASR_CLIENT_SETTINGS:-${XDG_CONFIG_HOME:-$HOME/.config}/…} reads differently
# in dash than in bash, and quietly produced no settings path at all. One expansion per line.
conf=${XDG_CONFIG_HOME:-$HOME/.config}
data=${XDG_DATA_HOME:-$HOME/.local/share}
settings=${ASR_CLIENT_SETTINGS:-$conf/voiceime/config.toml}
generated=$data/audiocpp-asr/serve.json
unit_dir=$conf/systemd/user
unit=$unit_dir/audiocpp-asr.service
unit_name=${unit##*/}

voiceime=${VOICEIME_BIN:-$(command -v voiceime || true)}
# What the client's settings say: the one thing they own about recognition is the address a
# recording is posted to, and the id to name in it. The client is asked, in the machine-readable
# form it publishes for exactly this - `capabilities` is where its settings and their effective
# values are reported, and it is the only place they are. That is the difference between reading
# a format and guessing at it: a comment after a value is invisible to a hand-rolled parser, and
# a key defined twice reads as whichever one the parser happens to look at first - while the
# program that owns the file refuses both instead of choosing. Nothing here appends to what it is
# given either: the path belongs to the client.
surface=
if [ -n "$voiceime" ]; then
	if ! surface=$("$voiceime" --config "$settings" capabilities --json 2>/dev/null); then
		echo "$settings cannot be read, and an address is not something to guess at:" >&2
		"$voiceime" --config "$settings" capabilities --json 2>&1 1>/dev/null | sed 's/^/  /' >&2
		exit 3
	fi
elif [ -f "$settings" ]; then
	echo "no voiceime on PATH: $settings is not read, so the loopback address below is an assumption." >&2
fi
# The client's answer is JSON, so it is read with a JSON tool. Without one, nothing is
# substituted by eye for the whole file: the address is not guessed, and saying so is the
# difference between a wrong answer and an unknown one.
ask() { # one setting's value from the client's own report, or nothing
	if [ -z "$surface" ]; then return 0; fi
	if ! command -v jq >/dev/null 2>&1; then
		if [ ! -e "$asked_once" ]; then
			echo "no jq on PATH: $settings is not read, so the address and the model id below are assumptions." >&2
			: >"$asked_once"
		fi
		return 0
	fi
	printf '%s' "$surface" | jq -r --arg key "$1" '
		.data.settings[] | select(.key == $key) | .value | if . == null then "" else . end'
}
asked_once=$(mktemp)
trap 'rm -f "$asked_once"' EXIT
base_url=$(ask asr_server_url)
[ -n "$base_url" ] || base_url=http://127.0.0.1:18080/v1/audio/transcriptions
authority=${base_url#*://}
authority=${authority%%/*}
# The host and the port are separate questions. `open.bigmodel.cn` names no port, and taking
# that as "no host either" would read a provider's endpoint as this machine's - which is exactly
# what this block exists to decide, and what a wrong answer here starts a model for.
host=${authority%%:*}
if [ "$host" = "$authority" ]; then
	port=18080
else
	port=${authority##*:}
fi
[ -n "$host" ] || host=127.0.0.1
[ -n "$port" ] || port=18080
root="http://$host:$port"
request_url="$root/v1/audio/transcriptions"

# The settings can name a server that is not this machine - a provider's endpoint, another
# host. Then nothing belongs here: starting models on this box would contradict the settings,
# not complete them, and saying so and leaving is the whole of the refusal.
case "$host" in
127.0.0.1 | localhost | ::1 | \[::1\] | 127.*) ;;
*)
	echo "$base_url is not this machine: no model is started here."
	echo "a client needs only that this address answer a POST with the recording."
	exit 0
	;;
esac

# What each checkpoint is, and what id it will be served under. audio.cpp wrote the family into
# the file and its own tool reports it; the server's config requires id, family and path and
# defaults everything else. The plan is written once and read three times - into the config,
# into the wait below, into the settings - so "which models are served" is one answer.
[ -x "$gguf" ] || { echo "$gguf is not executable: it travels beside the server" >&2; exit 3; }
plan=$(mktemp)
trap 'rm -f "$plan"' EXIT INT TERM
first=true
for model in "$@"; do
	[ -f "$model" ] || { echo "$model is not a file" >&2; exit 3; }
	family=$("$gguf" --inspect "$model" | sed -n 's/^model_spec_family=//p')
	[ -n "$family" ] || { echo "$model carries no audio.cpp model spec: convert it with audio.cpp first" >&2; exit 3; }
	entry=${model##*/}
	entry=${entry%.*}
	# audio.cpp refuses a duplicate id at start-up, and two checkpoints named the same thing
	# would be an install the user cannot describe: which one did they ask for?
	if cut -f1 "$plan" | grep -qxF "$entry"; then
		echo "two checkpoints would be served under the id $entry: name the files differently" >&2
		exit 3
	fi
	lazy=true
	[ "$first" = true ] && lazy=false
	first=false
	printf '%s\t%s\t%s\t%s\n' "$entry" "$family" "$model" "$lazy" >>"$plan"
done

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

# One entry per checkpoint: the first in offline mode with the request options a multipart
# upload has no field for, and `lazy` saying whether the weights belong to this service's
# start-up or to the first request that asks for it. `max_loaded_models: 0` is audio.cpp's
# "no limit": models that have been used stay resident, so switching back is not another load.
render_models() {
	awk -F'\t' -v chunk_mode='"audio_chunk_mode": "none"' '
	{
		if (NR > 1) printf ",\n";
		printf "    {\n";
		printf "      \"id\": \"%s\",\n", $1;
		printf "      \"family\": \"%s\",\n", $2;
		printf "      \"path\": \"%s\",\n", $3;
		printf "      \"task\": \"asr\",\n";
		printf "      \"mode\": \"offline\",\n";
		printf "      \"lazy\": %s,\n", $4;
		printf "      \"default_request_options\": {\n";
		printf "        %s\n", chunk_mode;
		printf "      }\n";
		printf "    }";
	}
	END { printf "\n" }
	' "$plan"
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
$(render_models)  ]
}
JSON

# The unit names the binary and the config, and links the deployment page's section on a model
# on this machine, so `systemctl status` points at an explanation of what these two files are
# for. The path is the one this script was run from; a checkout that has moved simply leaves
# the field out, because a link to a file that is not there is worse than no link.
documentation=""
model_docs=$here/../docs/DEPLOY.md
if [ -f "$model_docs" ]; then
	documentation="Documentation=file://$(readlink -f "$model_docs")
"
fi

write_if_changed "$unit" <<UNIT
[Unit]
Description=audio.cpp ASR server (models resident)
$documentation
[Service]
ExecStart=$server --config $generated --log
Type=simple
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
UNIT

systemctl --user daemon-reload
systemctl --user enable "$unit_name"
systemctl --user restart "$unit_name"

# "Enabled" is not "starts when the machine boots". A unit in a user's own systemd is started by
# that user's systemd instance, and on a machine without lingering that instance comes up at
# login. On a desktop that is the same moment as logging in and nothing needs changing; on a
# machine expected to serve before anyone logs in, it is a difference worth naming. This says
# which one this is and names the command - changing a person's whole session startup is their
# decision, not something a deployment takes quietly.
if command -v loginctl >/dev/null 2>&1; then
	if [ "$(loginctl show-user "${USER:-$(id -un)}" -p Linger --value 2>/dev/null)" = no ]; then
		echo "note: this starts when you log in, not when the machine boots (Linger=no)."
		echo "      to start it at boot: loginctl enable-linger ${USER:-$(id -un)}"
	fi
fi

# "Started" is not "serving the checkpoints named on the command line": the first one has to
# load, and a server still running the previous config answers /health too. So this waits for
# every id - audio.cpp registers every configured entry at start-up whether or not it loads it,
# which is what makes an un-loaded entry visible here and therefore switchable. This is also
# the assertion anyone can repeat afterwards with curl.
cut -f1 "$plan" | while read -r entry; do
	tries=0
	while [ "$tries" -lt 240 ]; do
		if curl -sf "$root/v1/models" | grep -q "\"id\": *\"$entry\""; then break; fi
		tries=$((tries + 1))
		sleep 0.5
	done
	if [ "$tries" -ge 240 ]; then
		echo "the service is not serving $entry: systemctl --user status $unit_name" >&2
		exit 1
	fi
done

# The settings belong to whoever keeps them, and this script does not write them. What was started
# here is only useful once the client posts to it, so the last thing printed is the two lines
# that do that and the file they belong in - the same two lines a person edits afterwards to
# switch a model, which is exactly why they are printed rather than written for them.
#
# What to recommend for `model` is the client's current choice where that choice names one of
# the ids being served: switching a model is their decision, not something a redeploy
# overwrites. Anything else - a name that is not served here, or nothing at all - gets the
# first id, which is the one that is already loaded.
asked=$(ask model)
chosen=$asked
# An empty name matches every line of a file, so an unset `model` is asked about separately:
# treating it as "already one of these" is how a settings file ends up naming nothing.
if [ -z "$asked" ] || ! cut -f1 "$plan" | grep -qxF "$asked"; then
	chosen=$(cut -f1 "$plan" | head -n 1)
fi

# The switch is one line in that file, and the command that puts a settings file in force is
# named here rather than left to memory: an engine process reads it when it starts.
echo "serving: $(cut -f1 "$plan" | wc -l) model(s) from this machine (backend $backend, $threads threads)"
awk -F'\t' '{ printf "  %-42s %s\n", $1, ($4 == "true" ? "loads on first use <- " : "loaded at start-up     <- ") $3 }' "$plan"
echo "write these two lines in $settings; this script does not edit that file:"
printf '  asr_server_url = "%s"\n' "$request_url"
if [ "$chosen" = "$asked" ]; then
	printf '  model          = "%s"   # already there, and it names one of these\n' "$chosen"
else
	printf '  model          = "%s"\n' "$chosen"
fi
echo "then put either change in force: voiceime apply"
echo "switch a model without redeploying: name another of the ids above in that line, then voiceime apply"
