#!/usr/bin/env python3
"""Integration checks for the single-binary generation boundary.

The test runner uses host Python/readelf/strace. The generator under test gets an
empty environment, and every exec it makes must be of that same ELF.
"""
import argparse
import ast
import json
from pathlib import Path
import re
import shutil
import subprocess
import tempfile


def read_program(repo):
    return json.loads(ast.literal_eval((repo / 'rules/flag_program.bzl').read_text()[len('FLAG_PROGRAM = json.decode('):-2]))


def without_provenance(value):
    if isinstance(value, dict):
        return {k: without_provenance(v) for k, v in value.items() if k != 'file'}
    if isinstance(value, list):
        return [without_provenance(v) for v in value]
    return value


def main():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--tool', type=Path, required=True)
    p.add_argument('--linux', type=Path, required=True)
    p.add_argument('--out', type=Path, required=True)
    p.add_argument('--reference', type=Path, help='Existing prototype output for differential checks')
    p.add_argument('--config-dir', type=Path, default=Path(__file__).resolve().parent / 'examples/configs')
    p.add_argument('--strace', action='store_true', help='Require strace and audit every exec')
    args = p.parse_args()
    tool, linux, out = args.tool.resolve(), args.linux.resolve(), args.out.resolve()
    out.mkdir(parents=True, exist_ok=True)
    headers = subprocess.check_output(['readelf', '-l', '-d', tool], text=True)
    assert 'INTERP' not in headers and '(NEEDED)' not in headers, headers
    result = subprocess.run([tool, 'version'], env={'PATH': ''}, capture_output=True, text=True, check=True)
    assert json.loads(result.stdout)['make'] == '4.4.1', result.stdout
    # Only syntax export is allowed; the linked Make must never run recipes.
    result = subprocess.run([tool, '_make', '-f', '/dev/null'], env={'PATH': ''}, capture_output=True, text=True)
    assert result.returncode == 2 and 'Only GNU Make syntax export' in result.stderr
    repo = out / 'repo'
    command = [str(tool), 'generate', '--linux', str(linux), '--out', str(repo), '--all-backends']
    for fragment in sorted(args.config_dir.glob('*.config')):
        command.extend(['--config-fragment', str(fragment.resolve())])
    if args.strace:
        command = [shutil.which('strace'), '-f', '-s', '4096', '-e', 'trace=execve,execveat',
                   '-o', str(out / 'exec.trace'), *command]
        assert command[0], 'strace is required for --strace'
    with (out / 'generation.log').open('w') as log:
        subprocess.run(command, env={'PATH': '', 'CC': '/nonexistent/compiler', 'ARCH': 'invalid',
                                    'MAKEFLAGS': '--definitely-invalid', 'PYTHONHOME': '/nonexistent'},
                       stdout=log, stderr=subprocess.STDOUT, check=True)
    if args.strace:
        trace = (out / 'exec.trace').read_text()
        executable_paths = re.findall(r'\bexecve\("([^"\n]+)"', trace)
        assert len(executable_paths) > 1000, len(executable_paths)
        assert set(executable_paths) == {str(tool)}, set(executable_paths)
        assert 'execveat(' not in trace, trace
        print(f'PASS: {len(executable_paths)} execs, all of the same static ELF')
    assert (repo / 'source').resolve() == linux
    assert not list(repo.glob('.generation-*')), 'Temporary generation files leaked'
    assert not (repo / 'rules/native_make').exists()
    assert '${MAKE}' not in (repo / 'rules/link-vmlinux.sh').read_text()
    requests_text = (repo / 'rules/example_configs.bzl').read_text()
    requests = json.loads(ast.literal_eval(requests_text[len('EXAMPLE_CONFIGS = json.decode('):-2]))
    assert requests['x86_64']['MODULES'] == 'n'
    assert requests['debug']['LOCALVERSION'] == '"-debug"'
    assert requests['lz4']['KERNEL_GZIP'] == 'n'
    program = read_program(repo)
    assert 'CONFIG_' in json.dumps(program) and 'SRCARCH' in json.dumps(program)
    assert '/tmp/tmp' not in json.dumps(program), 'Unstable temporary provenance in Make programs'
    if args.reference:
        reference = args.reference.resolve()
        for name in ('options/BUILD.bazel', 'rules/architecture_data.bzl', 'MODULE.bazel'):
            assert (repo / name).read_bytes() == (reference / name).read_bytes(), name
        assert without_provenance(program) == without_provenance(read_program(reference)), 'Make programs changed'
        print('PASS: Kconfig and Make programs match the existing prototype')
    # Errors must not clobber an existing repository or silently accept a parser
    # compiled against a different kernel revision.
    sentinel = repo / 'keep.txt'
    sentinel.write_text('keep\n')
    result = subprocess.run([tool, 'generate', '--linux', linux, '--out', repo],
                            env={'PATH': ''}, capture_output=True, text=True)
    assert result.returncode and sentinel.read_text() == 'keep\n'
    assert 'must be empty' in result.stderr
    with tempfile.TemporaryDirectory(dir=out) as directory:
        result = subprocess.run([tool, 'generate', '--linux', directory, '--out', out / 'invalid'],
                                env={'PATH': ''}, capture_output=True, text=True)
        assert result.returncode and 'Kconfig parser source changed' in result.stderr
    print('PASS: empty-PATH generation, symbolic inputs, static linkage, and input/output guards')
    print(repo)


if __name__ == '__main__':
    main()
