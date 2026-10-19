"""Check the declared GNU Make export boundary and symbolic source ownership."""

import argparse
import json
import os
import platform
import re
import shutil
import subprocess
import tempfile
from pathlib import Path

from build import assert_self_contained


def assignments(value):
    if isinstance(value, dict):
        if value.get('kind') == 'assignment':
            yield value
        for child in value.values():
            yield from assignments(child)
    elif isinstance(value, list):
        for child in value:
            yield from assignments(child)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--tool', type=Path, required=True)
    parser.add_argument('--source-anchor', type=Path, required=True)
    parser.add_argument('--strace', action='store_true', help='Require Linux exec tracing for the standalone tool')
    args = parser.parse_args()
    tool = args.tool.resolve()
    architecture = platform.machine()
    if architecture == 'arm64':
        architecture = 'aarch64'
    assert_self_contained(tool, architecture)
    # The launcher ignores ambient Python/compiler settings and runs only its
    # embedded native syntax parser; Make evaluation and recipes are forbidden.
    environment = {'PATH': '', 'CC': '/absent/cc', 'MAKEFLAGS': '--invalid',
                   'PYTHONHOME': '/absent/python', 'PYTHONPATH': '/absent/python'}
    def generate(output: Path, trace: Path | None = None) -> subprocess.CompletedProcess[str]:
        command = [str(tool), '--linux', str(linux), '--out', str(output)]
        if trace is not None:
            tracer = shutil.which('strace')
            assert tracer, 'strace is required for --strace'
            command = [tracer, '-f', '-s', '4096', '-e', 'trace=execve,execveat', '-o', str(trace), *command]
        return subprocess.run(command, env=environment, capture_output=True, text=True)

    linux = args.source_anchor.resolve().parent
    with tempfile.TemporaryDirectory() as temporary:
        output = Path(temporary) / 'export'
        diagnostics = Path(os.environ.get('TEST_UNDECLARED_OUTPUTS_DIR', temporary))
        trace = diagnostics / 'exec.trace' if args.strace else None
        result = generate(output, trace)
        assert result.returncode == 0, result.stderr
        if trace is not None:
            text = trace.read_text()
            paths = re.findall(r'\bexecve\("([^"\n]+)"', text)
            assert len(paths) > 1 and set(paths) == {str(tool)}, paths
            assert 'execveat(' not in text, text
            print(f'PASS: {len(paths)} execs, all of the same static exporter')
        data = json.loads((output / 'report.json').read_text())
        print(f'Kbuild payload: {(output / "report.json").stat().st_size} report bytes; '
              f'{sum(1 for _ in output.iterdir())} files', flush=True)
        assert sorted(p.name for p in output.iterdir()) == ['program.bzl', 'report.json']
        assert (output / 'program.bzl').read_text().startswith('KBUILD = json.decode(')
        program = data['program']
        encoded = json.dumps(program)
        assert 'CONFIG_' in encoded and 'SRCARCH' in encoded
        assert '.generation-' not in encoded and str(linux) not in encoded
        assert data['actions'] and '${MAKE}' not in data['link_script']
        host = list(assignments(program['image_host_objtool']))
        pound = [node for node in host if node.get('name') == 'pound']
        assert len(pound) == 1 and pound[0]['value_expression'] == [{'op': 'literal', 'value': '#'}], pound
        elfshdr = [node for node in host if node.get('name') == 'elfshdr']
        assert len(elfshdr) == 1 and 'kbuild_host_shell' in json.dumps(elfshdr), elfshdr
        assert not list(output.parent.glob('.generation-*'))
        sentinel = output / 'keep.txt'
        sentinel.write_text('keep\n')
        result = generate(output)
        assert result.returncode != 0 and 'must be empty' in result.stderr, result.stderr
        assert sentinel.read_text() == 'keep\n'
        result = generate(linux / 'generated')
        assert result.returncode != 0 and 'outside' in result.stderr, result.stderr
        fixture = Path(temporary) / 'Makefile'
        fixture.write_text('VALUE := $(error must not evaluate)\nall:\n\t$(error must not execute)\n')
        events = Path(temporary) / 'events.jsonl'
        result = subprocess.run([str(tool), '_make', '-rR', '-f', str(fixture)],
                                env={'PATH': '', 'LINUX_BZL_MAKE_AST': str(events)},
                                capture_output=True, text=True)
        assert result.returncode == 0, result.stderr
        assert 'must not evaluate' in events.read_text() and 'must not execute' in events.read_text()
        result = subprocess.run([str(tool), '_make', '-f', str(fixture)], env=environment,
                                capture_output=True, text=True)
        assert result.returncode != 0 and 'Only GNU Make syntax export' in result.stderr
    print('PASS: declared Make, symbolic inputs, host helpers, and source/output guards')


if __name__ == '__main__':
    main()
