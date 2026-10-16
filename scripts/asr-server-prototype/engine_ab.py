#!/usr/bin/env python3
"""Replay the real IBus engine over a WAV and report commit latency.

This is the measurement that matters: not how long a decode takes in isolation,
but how long the user waits between finishing a sentence and seeing its text.
The engine is started by hand (the recipe in docs/AGENT.md) and activated with
`ibus engine voiceime`, which is what a hotkey press does. Every session line is
timestamped as it arrives, so the gap between "utterance" (the endpointer closed
a segment) and "repaired" (that text was committed) is the latency felt.

    python3 engine_ab.py --binary ../../bin/voiceime --clip speech.wav --label "default"
    python3 engine_ab.py --binary ../../bin/voiceime --clip speech.wav --server \
        --server-url http://127.0.0.1:18080 --model refine --label "server"

--server rewrites two config keys for the duration of the run and restores the
file afterwards, so the same clip can be A/B'd without leaving the machine
configured for a server nobody started.
"""
import argparse
import contextlib
import os
import pathlib
import subprocess
import threading
import time

DEFAULT_ENGINE = "xkb:us::eng"


def read_config(path):
    with open(path, "rb") as handle:
        return handle.read()


@contextlib.contextmanager
def config_values(binary, path, values):
    if not values:
        yield
        return
    original = read_config(path)
    try:
        for key, value in values.items():
            subprocess.run([binary, "config", "set", key, value], check=True,
                           stdout=subprocess.DEVNULL)
        yield
    finally:
        pathlib.Path(path).write_bytes(original)


def run_engine(binary, clip, timeout):
    engine = subprocess.Popen([binary, "ibus-engine", "--ibus", "--audio-file", clip],
                              stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                              text=True, bufsize=1)
    started = time.perf_counter()
    events = []

    def reader():
        # readline, not `for line in stdout`: the iterator reads ahead, which
        # holds lines back until a buffer fills and makes every timestamp late.
        while True:
            line = engine.stdout.readline()
            if not line:
                break
            line = line.rstrip()
            if line:
                events.append((time.perf_counter() - started, line))
                print(f"[{time.perf_counter() - started:7.3f}] {line}", flush=True)

    thread = threading.Thread(target=reader, daemon=True)
    thread.start()

    deadline = time.perf_counter() + 30
    while time.perf_counter() < deadline:
        if any("engine started" in line for _, line in events):
            break
        time.sleep(0.05)
    else:
        print("engine never reported that it was listening", flush=True)
    time.sleep(0.3)
    subprocess.run(["ibus", "engine", "voiceime"], check=False)
    try:
        deadline = time.perf_counter() + timeout
        while time.perf_counter() < deadline:
            if any("session finished" in line for _, line in events):
                break
            time.sleep(0.2)
    finally:
        subprocess.run(["ibus", "engine", DEFAULT_ENGINE], check=False)
        time.sleep(0.5)
        engine.terminate()
        try:
            engine.wait(timeout=5)
        except subprocess.TimeoutExpired:
            engine.kill()
    return events


def main():
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--binary", default="bin/voiceime")
    parser.add_argument("--clip", required=True)
    parser.add_argument("--label", default="run")
    parser.add_argument("--server", action="store_true",
                        help="repair through a resident server instead of the CLI route")
    parser.add_argument("--server-url", default="http://127.0.0.1:18080")
    parser.add_argument("--model", default="refine",
                        help="with --server: the server model id; without it: the refine GGUF path")
    parser.add_argument("--config", default=os.path.expanduser("~/.config/voiceime/config.toml"))
    parser.add_argument("--timeout", type=float, default=120)
    args = parser.parse_args()

    if args.server:
        values = {"asr_server_url": args.server_url,
                  "asr_server_model": args.model,
                  "refine_model": ""}
    else:
        values = {"asr_server_url": "", "refine_model": args.model if args.model != "refine" else ""}

    print(f"### {args.label} ({'server' if args.server else 'CLI process per utterance'})")
    subprocess.run(["pkill", "-f", "voiceime ibus[-]engine"], check=False)
    time.sleep(1)
    with config_values(args.binary, args.config, values):
        events = run_engine(args.binary, args.clip, args.timeout)

    for _, line in events:
        if "repair:" in line:
            print(f"  route: {line.split('repair:', 1)[1].strip()}")

    closes = [at for at, line in events if "utterance " in line]
    repairs = [at for at, line in events if "repaired " in line]
    for index, at in enumerate(repairs):
        if index < len(closes):
            print(f"  utterance {index + 1}: {at - closes[index]:.3f}s from the segment "
                  f"closing to its text being committed")


if __name__ == "__main__":
    main()
