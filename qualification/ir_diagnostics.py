"""Capture expected compiler failures without hiding their native diagnostics."""

import json
import os
from pathlib import Path
import subprocess
import sys


def main() -> None:
    spec, output = map(Path, sys.argv[1:])
    cases = json.loads(spec.read_text())
    assert isinstance(cases, list), cases
    results = []
    for case in cases:
        match case:
            case {"name": str(name), "bitcode": str(bitcode), "bitcode_runfile": str(runfile), "commands": list(commands)}:
                pass
            case _:
                raise ValueError(f"Invalid diagnostic case: {case}")
        Path(bitcode).parent.mkdir(parents=True, exist_ok=True)
        outcomes = []
        for command in commands:
            match command:
                case {"phase": str(phase), "argv": list(argv), "environment": dict(environment)} if all(isinstance(arg, str) for arg in argv) and all(isinstance(key, str) and isinstance(value, str) for key, value in environment.items()):
                    pass
                case _:
                    raise ValueError(f"Invalid compiler command: {command}")
            result = subprocess.run(argv, env=os.environ | environment, capture_output=True, text=True, check=False)
            outcomes.append(dict(command, returncode=result.returncode, stdout=result.stdout, stderr=result.stderr))
            if phase == "frontend" and (result.returncode != 0 or not Path(bitcode).is_file() or Path(bitcode).stat().st_size == 0):
                raise RuntimeError(f"{name} frontend did not produce bitcode: {outcomes}")
        results.append({"name": name, "bitcode": runfile, "outcomes": outcomes})
    output.write_text(json.dumps(results, indent=2) + "\n")


if __name__ == "__main__":
    main()
