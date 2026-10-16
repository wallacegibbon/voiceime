#!/usr/bin/env python3
"""Write an audiocpp_server config for the two ASR roles voiceime needs.

The refine model must be its own entry, not the streaming model's: the server
holds a model for the whole length of a request, so a repair sent to the id a
live dictation owns waits for that dictation to end (see model_lock.py).

Per-utterance request options cannot ride the multipart transcription upload -
only the JSON route takes an `options` object - so the options voiceime's repair
pass would have passed on the command line belong here, in the entry's
default_request_options.
"""
import argparse
import json
import pathlib
import sys


def main():
    parser = argparse.ArgumentParser(description=__doc__,
                                     formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--streaming", required=True,
                        help="GGUF used for live partials (configured mode: streaming)")
    parser.add_argument("--refine", required=True,
                        help="GGUF used for the offline repair decode")
    parser.add_argument("--refine-family", default="sense_asr")
    parser.add_argument("--streaming-family", default="sense_asr")
    parser.add_argument("--port", type=int, default=18080)
    parser.add_argument("--threads", type=int, default=4)
    parser.add_argument("--backend", default="cpu")
    parser.add_argument("--chunk-mode", default="none",
                        help="audio_chunk_mode for the refine entry; 'none' matches "
                             "what the CLI repair route passes")
    parser.add_argument("--out", default="server.json")
    args = parser.parse_args()

    streaming = pathlib.Path(args.streaming).expanduser()
    refine = pathlib.Path(args.refine).expanduser()
    for path in (streaming, refine):
        if not path.is_file():
            sys.exit(f"not a file: {path}")

    config = {
        "host": "127.0.0.1",
        "port": args.port,
        "backend": args.backend,
        "threads": args.threads,
        "lazy_load": True,
        # Both entries stay resident: the point of the server is that the refine
        # model is loaded once instead of once per utterance.
        "max_loaded_models": 0,
        "ui": False,
        "ui_management": False,
        "models": [
            {
                "id": "stream-sense",
                "family": args.streaming_family,
                "path": str(streaming),
                "task": "asr",
                "mode": "streaming",
            },
            {
                "id": "refine",
                "family": args.refine_family,
                "path": str(refine),
                "task": "asr",
                "mode": "offline",
                "default_request_options": {"audio_chunk_mode": args.chunk_mode},
            },
        ],
    }
    pathlib.Path(args.out).write_text(json.dumps(config, indent=2) + "\n")
    print(args.out)
    print(f"  asr_server_url   http://127.0.0.1:{args.port}")
    print("  asr_server_model refine")


if __name__ == "__main__":
    main()
