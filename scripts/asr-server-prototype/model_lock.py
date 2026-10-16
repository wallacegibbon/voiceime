#!/usr/bin/env python3
"""Show what the server's per-model lock costs when both roles share one id.

The server holds a model for the whole length of a request. A live dictation
therefore owns the streaming model until the speaker stops, and a repair sent to
the *same* id waits for it - not for a slot, for the dictation. This holds a live
session open and issues the same short repair against two different ids:

    python3 model_lock.py --live-model stream-sense --other-model refine

Expect the live model's id to wait out the session (seconds) while the other id
answers immediately with identical work. That is why the refine model needs its
own entry, and why two entries cost two resident copies of the weights.
"""
import argparse
import http.client
import json
import threading
import time
import wave


def read_pcm(path):
    with wave.open(path, "rb") as handle:
        if handle.getframerate() != 16000 or handle.getnchannels() != 1:
            raise SystemExit(f"{path}: needs 16 kHz mono PCM to drive the live endpoint")
        return handle.readframes(handle.getnframes())


def live_session(host, port, model, pcm, results, chunk_ms=100, stream=True):
    chunk_bytes = int(16000 * 2 * chunk_ms / 1000)
    connection = http.client.HTTPConnection(host, port, timeout=180)
    query = (f"/v1/audio/transcriptions/live?model={model}&sample_rate=16000"
             "&channels=1&sample_format=s16le")
    if stream:
        query += "&stream=true"
    connection.putrequest("POST", query)
    connection.putheader("Transfer-Encoding", "chunked")
    connection.endheaders()
    started = time.perf_counter()
    try:
        for offset in range(0, len(pcm), chunk_bytes):
            chunk = pcm[offset:offset + chunk_bytes]
            connection.send(b"%x\r\n" % len(chunk) + chunk + b"\r\n")
            time.sleep(chunk_ms / 1000)
        connection.send(b"0\r\n\r\n")
        response = connection.getresponse()
        response.read()
        results["live"] = (time.perf_counter() - started, response.status)
    except Exception as exc:  # noqa: BLE001
        results["live"] = (time.perf_counter() - started, f"error: {exc}")


def repair(host, port, model, clip, label, results, timeout=120):
    connection = http.client.HTTPConnection(host, port, timeout=timeout)
    body = json.dumps({"model": model, "audio": clip}).encode()
    started = time.perf_counter()
    try:
        connection.request("POST", "/v1/audio/transcriptions", body,
                           {"Content-Type": "application/json"})
        response = connection.getresponse()
        payload = json.loads(response.read())
        results[label] = (time.perf_counter() - started, response.status, payload.get("text", ""))
    except Exception as exc:  # noqa: BLE001
        results[label] = (time.perf_counter() - started, f"error: {exc}", "")
    finally:
        connection.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=18080)
    parser.add_argument("--live-model", required=True,
                        help="id the live session holds")
    parser.add_argument("--other-model", required=True,
                        help="id of a second entry, ideally the same weights")
    parser.add_argument("--live-clip", required=True,
                        help="WAV whose duration is the dictation being served")
    parser.add_argument("--repair-clip", required=True,
                        help="the utterance to repair, typically much shorter")
    parser.add_argument("--at", type=float, default=2.0,
                        help="seconds into the live session to send the repair")
    args = parser.parse_args()

    results = {}
    pcm = read_pcm(args.live_clip)

    repair(args.host, args.port, args.other_model, args.repair_clip, "baseline", results)
    print(f"baseline ({args.other_model}, idle): {results['baseline'][0]:.3f}s "
          f"status={results['baseline'][1]} text={results['baseline'][2][:60]!r}")

    thread = threading.Thread(target=live_session,
                              args=(args.host, args.port, args.live_model, pcm, results),
                              daemon=True)
    thread.start()
    time.sleep(args.at)

    repair(args.host, args.port, args.other_model, args.repair_clip, "other", results)
    print(f"during live, other id ({args.other_model}): {results['other'][0]:.3f}s "
          f"status={results['other'][1]}")

    repair(args.host, args.port, args.live_model, args.repair_clip, "same", results)
    print(f"during live, same id ({args.live_model}):  {results['same'][0]:.3f}s "
          f"status={results['same'][1]}")
    thread.join()

    elapsed, status = results["live"]
    print(f"\nlive session ended after {elapsed:.2f}s (status {status})")


if __name__ == "__main__":
    main()
