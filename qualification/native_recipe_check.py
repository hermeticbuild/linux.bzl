"""Check actual native size, recipe expansion and LZ4 round-trip outputs."""

import json
from pathlib import Path
import sys


def main() -> None:
    match sys.argv[1:]:
        case ["frame", compressed, frame]:
            data = Path(compressed).read_bytes()
            if data[-4:] != (257).to_bytes(4, "little"):
                raise ValueError(f"Invalid native LZ4 size trailer: {data[-4:]!r}")
            Path(frame).write_bytes(data[:-4])
        case ["check", payload, trailer, expanded, restored, lz4, report]:
            original = Path(payload).read_bytes()
            size = Path(trailer).read_bytes()
            text = Path(expanded).read_text()
            if original != b"x" * 257 or size != (257).to_bytes(4, "little"):
                raise ValueError("Native file-size command did not preserve the 257-byte input")
            if text != "quoted value|no|$literal":
                raise ValueError(f"Make recipe captures were not expanded before execution: {text!r}")
            if Path(restored).read_bytes() != original:
                raise ValueError("Source-selected LZ4 command did not preserve the input")
            Path(report).write_text(json.dumps(dict(payload_size=len(original), trailer_hex=size.hex(),
                                                   expanded=text, roundtrip=True, lz4=lz4), indent=2) + "\n")
        case _:
            raise ValueError("Expected a native recipe frame or check invocation")


if __name__ == "__main__":
    main()
