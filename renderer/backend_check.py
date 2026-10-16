#!/usr/bin/env python3
"""Compile and partially link with the selected toolchain, retaining diagnostics."""
import argparse
import json
from pathlib import Path
import subprocess
import tempfile


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--check', nargs='+', type=Path)
    p.add_argument('--cc')
    p.add_argument('--ld')
    p.add_argument('--source')
    p.add_argument('--flags')
    p.add_argument('--out', type=Path)
    a = p.parse_args()
    if a.check:
        failed = []
        for path in a.check:
            result = json.loads(path.read_text())
            print(path.name, json.dumps(result, sort_keys=True))
            if not result['passed']:
                failed.append(path.name)
        if failed:
            raise SystemExit('Backend checks failed: ' + ', '.join(failed))
        return
    steps = []
    with tempfile.TemporaryDirectory(prefix='kernel-backend-') as work:
        obj, linked = Path(work) / 'input.o', Path(work) / 'linked.o'
        for command in ([a.cc, *json.loads(a.flags), '-ffreestanding', '-c', a.source, '-o', str(obj)],
                        [a.ld, '-r', str(obj), '-o', str(linked)]):
            result = subprocess.run(command, text=True, capture_output=True)
            steps.append(dict(command=command, code=result.returncode, stdout=result.stdout, stderr=result.stderr))
            if result.returncode:
                break
        passed = len(steps) == 2 and all(x['code'] == 0 for x in steps)
        a.out.write_text(json.dumps(dict(passed=passed, steps=steps), indent=2) + '\n')


if __name__ == '__main__':
    main()
