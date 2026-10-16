#!/usr/bin/env python3
"""Time transcription requests against a resident audiocpp_server.

Reports the first request (which pays the model load) separately from the rest,
because that difference is the entire reason this route exists: a process per
utterance pays the load every time, and a server pays it once.

    python3 latency.py --model refine --clip speech.wav --runs 4
"""
import argparse
import json
import statistics
import sys
import time
import urllib.request


def transcribe(base, model, path, language, timeout):
    body = json.dumps({"model": model, "audio": path, "language": language}).encode()
    request = urllib.request.Request(base + "/v1/audio/transcriptions", data=body,
                                     headers={"Content-Type": "application/json"})
    started = time.perf_counter()
    with urllib.request.urlopen(request, timeout=timeout) as response:
        payload = json.loads(response.read())
    return time.perf_counter() - started, payload


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--base", default="http://127.0.0.1:18080")
    parser.add_argument("--model", required=True)
    parser.add_argument("--clip", required=True)
    parser.add_argument("--language", default="en")
    parser.add_argument("--runs", type=int, default=4)
    parser.add_argument("--timeout", type=int, default=120)
    args = parser.parse_args()
    base = args.base.rstrip("/")

    times = []
    for index in range(args.runs):
        elapsed, payload = transcribe(base, args.model, args.clip, args.language, args.timeout)
        timing = payload.get("timing", {})
        if index == 0:
            wall_ms = timing.get("wall_ms", 0)
            print(f"cold (load + decode) {elapsed:7.3f}s  model_wall={wall_ms:.0f}ms"
                  f"  implied_load={elapsed * 1000 - wall_ms:.0f}ms")
        else:
            times.append(elapsed)
            print(f"warm                 {elapsed:7.3f}s  rtf={timing.get('rtf')}")
    if not times:
        sys.exit("need at least two runs to separate load from decode")
    print(f"\n{args.model}: warm median {statistics.median(times):.3f}s "
          f"(min {min(times):.3f}s, max {max(times):.3f}s) over {len(times)} runs")


if __name__ == "__main__":
    main()
