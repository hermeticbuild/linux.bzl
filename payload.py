"""Publish JSON through filenames with bounded directory fanout."""
import json
from pathlib import Path
import sys
from typing import TypeAlias


JsonValue: TypeAlias = bool | int | float | str | None | list['JsonValue'] | dict[str, 'JsonValue']


def chunk_path(index: int, encoded: str) -> Path:
    """Keep the ordered basename while bounding each directory to 100 children."""
    if not 0 <= index < 100_000_000:
        raise ValueError(f'Payload chunk index exceeds the eight-digit range: {index}')
    number = f'{index:08d}'
    return Path(number[:2], number[2:4], number[4:6], number + '=' + encoded)


def publish(output: Path | str, value: JsonValue, *, report: JsonValue = None) -> None:
    """Encode the consumer payload and optionally retain a separate full report."""
    text = json.dumps(value, ensure_ascii=True, separators=(',', ':'))
    # Match rules/schedule.bzl: escape the marker before filename separators.
    encoded = text.replace('%', '%25').replace('/', '%2F').replace('\\', '%5C')
    count = (len(encoded) + 179) // 180
    if count > 100_000_000:
        raise ValueError(f'Payload exceeds the eight-digit chunk range: {count} chunks')
    output = Path(output)
    output.mkdir(parents=True, exist_ok=True)
    print(f'Payload: {len(text)} JSON bytes; {count} chunks', file=sys.stderr, flush=True)
    for start in range(0, len(encoded), 180):
        path = output / chunk_path(start // 180, encoded[start:start + 180])
        path.parent.mkdir(parents=True, exist_ok=True)
        with path.open('xb'):
            pass
    (output / 'report.json').write_text(json.dumps(value if report is None else report, indent=2))
