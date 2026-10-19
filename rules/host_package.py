"""Expose declared host-library flags to native pkg-config queries."""

import json
import os
from pathlib import Path
import shlex
import sys


def package_flags(packages: dict[str, object], arguments: list[str]) -> tuple[int, str]:
    match arguments:
        case [option, name] if option in {"--cflags", "--libs"}:
            pass
        case [name, option] if option in {"--cflags", "--libs"}:
            pass
        case _:
            raise ValueError(f"Unsupported native host package query: {arguments!r}")
    package = packages.get(name)
    if package is None:
        return 1, ""
    if not isinstance(package, dict):
        raise ValueError(f"Invalid declared host package: {name}")
    values = package[option.removeprefix("--")]
    if not isinstance(values, list) or any(not isinstance(value, str) for value in values):
        raise ValueError(f"Invalid {option} for declared host package: {name}")
    return 0, shlex.join(values)


if __name__ == "__main__":
    try:
        packages = json.loads(Path(sys.argv[1]).read_text())
        if not isinstance(packages, dict):
            raise ValueError("Declared host packages must be an object")
        status, flags = package_flags(packages, sys.argv[2:])
    except (OSError, ValueError, KeyError) as error:
        if marker := os.environ.get("KBUILD_PROBE_ERROR"):
            Path(marker).write_text(str(error))
        raise
    if flags:
        print(flags)
    sys.exit(status)
