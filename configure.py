#!/usr/bin/env python3
"""Generate symbolic kernel inputs and prepare native measurement/reference tools."""
import argparse
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import sys

ROOT = Path(__file__).resolve().parent


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--linux', type=Path, default=ROOT / '_tmp/linux')
    parser.add_argument('--generator', type=Path, default=ROOT / '_tmp/kernel-repository/kernel-repository')
    parser.add_argument('--generated', type=Path, help='Reuse a symbolic input export from the current generator')
    args = parser.parse_args()
    linux = args.linux.resolve()
    provenance = json.loads((ROOT / 'native/provenance.json').read_text())
    for name, expected in provenance['sources'].items():
        if hashlib.sha256((linux / name).read_bytes()).hexdigest() != expected:
            raise ValueError('Native Kconfig source differs from the tested parser: ' + name)
    output = ROOT / '.generated'
    output.mkdir(exist_ok=True)
    generated = args.generated.resolve() if args.generated else output / 'export'
    if generated == output or generated in output.parents:
        parser.error('--generated must be a separate export directory')
    if args.generated is None:
        if generated.exists():
            shutil.rmtree(generated)
        command = [str(args.generator.resolve()), 'generate', '--linux', str(linux), '--out', str(generated)]
        for fragment in sorted((ROOT / 'examples/configs').glob('*.config')):
            command.extend(['--config-fragment', str(fragment)])
        subprocess.run(command, check=True)
    if not (generated / 'baseline.json').is_file():
        parser.error('--generated must contain the current symbolic input format')
    shutil.copytree(generated, output, dirs_exist_ok=True)
    (output / 'setup.MODULE.bazel').write_text('''local_repository = use_repo_rule("@bazel_tools//tools/build_defs/repo:local.bzl", "local_repository")
new_local_repository = use_repo_rule("@bazel_tools//tools/build_defs/repo:local.bzl", "new_local_repository")
local_repository(name = "kernel_inputs", path = %r)
new_local_repository(name = "linux_source", path = %r, build_file_content = 'exports_files(["Makefile"])')
''' % (str(output), str(linux)))
    (output / 'inputs.json').write_text(json.dumps({'linux': str(linux)}))
    subprocess.run([sys.executable, str(ROOT / 'native/build_parser.py'), '--linux', str(linux)], check=True)
    subprocess.run([sys.executable, str(ROOT / 'native/build_reference.py'), '--linux', str(linux)], check=True)
    print('Configured', ROOT)


if __name__ == '__main__':
    main()
