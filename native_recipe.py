"""Expand deferred GNU Make shell results before parsing a native recipe."""
from collections.abc import Mapping
from dataclasses import dataclass
import json
import os
from pathlib import Path
import re
import subprocess
import sys


SLOT = re.compile(rb"__KBUILD_SHELL_RESULT_([0-9]+)__")


def interpolate(command: bytes, values: list[bytes]) -> bytes:
    def replace(match: re.Match[bytes]) -> bytes:
        index = int(match[1])
        if index >= len(values):
            raise ValueError(f"Unresolved Make shell result {index}")
        return values[index]
    return SLOT.sub(replace, command)


def fold_newlines(output: bytes) -> bytes:
    if b"\0" in output:
        raise ValueError("Make shell output contains a NUL byte")
    return output.replace(b"\r\n", b"\n").rstrip(b"\n").replace(b"\n", b" ")


def expand(shell: str, commands: list[str], shell_commands: list[str], *,
           cwd: Path | None = None, env: Mapping[str, str] | None = None) -> list[bytes]:
    values: list[bytes] = []
    for capture in shell_commands:
        # Make retains stdout even when the shell command fails. Stderr stays
        # visible; .SHELLSTATUS-dependent expressions are rejected in analysis.
        result = subprocess.run([shell, "-c", interpolate(capture.encode(), values)],
                                stdout=subprocess.PIPE, check=False, cwd=cwd, env=env)
        values.append(fold_newlines(result.stdout))
    return [interpolate(command.encode(), values) for command in commands]


@dataclass(frozen=True)
class Recipe:
    command: str
    strict: bool
    ignore_errors: bool


def main() -> None:
    spec = json.loads(Path(sys.argv[1]).read_text())
    match spec:
        case {"shell": str(shell), "recipes": list(recipes), "shell_commands": list(captures)} if all(isinstance(item, str) for item in captures):
            pass
        case _:
            raise ValueError("Invalid native recipe expansion specification")
    lines: list[Recipe] = []
    for recipe in recipes:
        match recipe:
            case {"command": str(command), "strict": bool(strict), "ignore_errors": bool(ignore_errors)}:
                lines.append(Recipe(command, strict, ignore_errors))
            case _:
                raise ValueError("Invalid native recipe line")
    # Make expands all recipe lines before starting their separate shells.
    expanded = expand(shell, [line.command for line in lines], captures)
    for command, line in zip(expanded, lines, strict=True):
        argv = [shell, *(["-eu"] if line.strict else []), "-c", command]
        if len(expanded) == 1 and not line.ignore_errors:
            os.execv(shell, argv)
        result = subprocess.run(argv, check=False)
        if result.returncode and not line.ignore_errors:
            if result.returncode < 0:
                os.kill(os.getpid(), -result.returncode)
            raise SystemExit(result.returncode)


if __name__ == "__main__":
    main()
