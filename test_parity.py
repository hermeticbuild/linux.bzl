"""Strictly assert every symbol and every written setting against native conf."""
import argparse
import json
from pathlib import Path

if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('report', type=Path)
    parser.add_argument('--compiler-suffix')
    args = parser.parse_args()
    report = args.report
    summary = json.loads((report / 'summary.json').read_text())
    print(json.dumps(summary, indent=2))
    if args.compiler_suffix:
        assert summary["compiler_command"].endswith(args.compiler_suffix), summary["compiler_command"]
    for name in ['symbol-differences.json', 'written-differences.json']:
        differences = json.loads((report / name).read_text())
        if differences:
            raise AssertionError(name + ': ' + json.dumps(differences, indent=2))
