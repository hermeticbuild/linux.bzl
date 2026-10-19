"""Copy declared sources and bind source scripts to declared interpreters."""

import json
from pathlib import Path
import shlex
import shutil
import sys


def prepare(sources: dict[str, str], commands: dict[str, str], root: Path) -> None:
    root.mkdir(parents=True, exist_ok=True)
    for logical, source in sources.items():
        destination = root / logical
        destination.parent.mkdir(parents=True, exist_ok=True)
        with open(source, "rb") as stream:
            first = stream.readline(512)
        if not first.startswith(b"#!"):
            shutil.copy2(source, destination)
            continue
        words = shlex.split(first[2:].decode().strip())
        if words and Path(words[0]).name == "env":
            words = words[1:]
            if words and words[0] == "-S":
                words = words[1:]
        name = Path(words[0]).name if words else ""
        with open(destination, "wb") as output:
            if name in commands and len(words) <= 2:
                # Linux resolves a relative interpreter against the action
                # working directory, where these declared inputs are staged.
                line = "#!" + commands[name] + (" " + words[1] if len(words) == 2 else "") + "\n"
                if len(line.encode()) > 255:
                    raise ValueError(f"Shebang exceeds Linux limit: {logical}")
                output.write(line.encode())
                with open(source, "rb") as stream:
                    stream.readline()
                    shutil.copyfileobj(stream, output)
            else:
                message = shlex.quote(f"Undeclared interpreter {first.decode().strip()!r} required by {logical}")
                output.write(("#!" + commands["sh"] + "\n"
                              'if test -n "${KBUILD_PROBE_ERROR:-}"; then printf "%s\\n" ' + message + ' > "$KBUILD_PROBE_ERROR"; fi\n'
                              'printf "%s\\n" ' + message + ' >&2\nexit 127\n').encode())
        shutil.copystat(source, destination)


if __name__ == "__main__":
    spec = json.loads(Path(sys.argv[1]).read_text())
    prepare(spec["sources"], spec["commands"], Path(sys.argv[2]))
