"""Single executable entry point; generation never configures a target kernel."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile

from export import native_events, parse_events

HERE = Path(__file__).resolve().parent


def check_parser_sources(linux):
    """Fail closed if the kernel's parser ABI/grammar differs from this build."""
    expected = json.loads((HERE / 'parser-sources.json').read_text())
    for name, digest in expected.items():
        source = linux / name
        if not source.is_file() or hashlib.sha256(source.read_bytes()).hexdigest() != digest:
            raise ValueError(f'Kconfig parser source changed: {name}. Rebuild kernel-repository for this kernel.')


def generate(linux, output, all_backends):
    check_parser_sources(linux)
    if output == linux or linux in output.parents or output in linux.parents:
        raise ValueError('Output must be outside the Linux source tree')
    output.mkdir(parents=True, exist_ok=True)
    # Work inside the repository directory, never remove its root or Bazel's
    # metadata. Publish only into an empty destination (apart from Bazel markers).
    existing = {p.name for p in output.iterdir()} - {'WORKSPACE', 'WORKSPACE.bazel', 'REPO.bazel'}
    if existing:
        raise ValueError('Output directory must be empty: ' + str(output))
    with tempfile.TemporaryDirectory(prefix='.generation-', dir=output) as directory:
        work = Path(directory)
        paths = sorted(p for p in linux.rglob('*') if p.is_file() and '.git' not in p.parts and
                       (p.name in ('Kbuild', 'Makefile') or p.name.startswith('Makefile.') or p.suffix == '.mk'))
        print(f'Exporting {len(paths)} Make files without expanding configuration', file=sys.stderr)
        tree = parse_events(native_events(Path(sys.executable), paths, expressions=True))
        ast = work / 'ast.json'
        ast.write_text(json.dumps(tree))
        repo = work / 'repo'
        import generate_backends, generate_image
        render = generate_backends.main if all_backends else generate_image.main
        render(['--ast', str(ast), '--make', sys.executable,
                '--linux', str(linux), '--out', str(repo)])
        versions = json.loads((HERE / 'tool-versions.json').read_text())
        (repo / 'repository-tool.json').write_text(json.dumps({
            'format': 1, 'tool_versions': versions,
            'parser_sources': json.loads((HERE / 'parser-sources.json').read_text()),
            'resolution': 'Bazel analysis', 'all_backends': all_backends,
        }, indent=2) + '\n')
        for path in repo.iterdir():
            destination = output / path.name
            if destination.exists() or destination.is_symlink():
                raise ValueError('Refusing to overwrite ' + str(destination))
            path.rename(destination)
    print(output)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest='command', required=True)
    sub.add_parser('version')
    generate_parser = sub.add_parser('generate')
    generate_parser.add_argument('--linux', type=Path, required=True)
    generate_parser.add_argument('--out', type=Path, required=True)
    generate_parser.add_argument('--all-backends', action='store_true')
    args = parser.parse_args()
    if args.command == 'version':
        print(json.dumps({'format': 1, **json.loads((HERE / 'tool-versions.json').read_text())}, sort_keys=True))
    else:
        # Internal Make/Kconfig environments are deliberately constructed by
        # their adapters; no compiler, Make variables or Python from the host.
        generate(args.linux.resolve(), args.out.resolve(), args.all_backends)


if __name__ == '__main__':
    main()
