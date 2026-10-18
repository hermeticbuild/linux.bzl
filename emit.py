"""Write bounded text chunks or concatenate declared input artifacts."""
from pathlib import Path
import sys

arguments = sys.argv[1:]
if arguments[0] == "--touch":
    for output in arguments[1:]:
        path = Path(output)
        path.parent.mkdir(parents=True, exist_ok=True)
        path.touch()
    raise SystemExit(0)
if arguments[0] in ("--chunks", "--concat"):
    mode, output, *values = arguments
    content = "".join(Path(p).read_text() for p in values) if mode == "--concat" else "".join(values)
else:
    output, content = arguments
path = Path(output)
path.parent.mkdir(parents=True, exist_ok=True)
path.write_text(content)
