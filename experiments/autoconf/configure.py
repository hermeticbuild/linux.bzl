#!/usr/bin/env python3
"""Configure the separate autoconf module and regenerate kernel input fixtures."""
import argparse
import ast
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import sys

ROOT = Path(__file__).resolve().parent
REPOSITORY = ROOT.parents[1]


def exported_call(path, name):
    """Read literal generator output without executing BUILD or Starlark code."""
    tree = ast.parse(path.read_text())
    calls = [node for node in ast.walk(tree) if isinstance(node, ast.Call)
             and isinstance(node.func, ast.Name) and node.func.id == name]
    if len(calls) != 1:
        raise ValueError(f'Expected one {name} in {path}')
    return {keyword.arg: keyword.value for keyword in calls[0].keywords}


def decode_literal(node):
    if isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute) and (
            isinstance(node.func.value, ast.Name) and node.func.value.id == 'json'
            and node.func.attr == 'decode' and len(node.args) == 1 and not node.keywords):
        return json.loads(ast.literal_eval(node.args[0]))
    return ast.literal_eval(node)


def fixtures(generated, output):
    """Keep symbolic syntax and programs; never resolve a compiler capability."""
    source = generated / 'options/BUILD.bazel'
    encoded = decode_literal(exported_call(source, 'kernel_option_sources')['files'])
    syntax = {name: json.loads(body) for name, body in encoded.items()}
    catalog = decode_literal(exported_call(generated / 'BUILD.bazel', 'kbuild_catalog')['actions'])
    catalog = {name: json.loads(body) if isinstance(body, str) else body for name, body in catalog.items()}
    profile = exported_call(generated / 'configs/BUILD.bazel', 'kernel_config')
    profile = {name: decode_literal(profile[name]) for name in ['base', 'requested']}
    profile['requested'].update(OBJTOOL='n', UNWINDER_ORC='n', STACK_VALIDATION='n', DEBUG_INFO_NONE='y')
    (output / 'rules').mkdir(parents=True, exist_ok=True)
    program = generated / 'rules/flag_program.bzl'
    shutil.copyfile(program, output / 'rules/flag_program.bzl')
    # Literal exported data is loaded by the callback and also passed independently
    # to the measuring runner. Both are regenerated from the same syntax export.
    (output / 'rules/sources.bzl').write_text('SOURCES = ' + repr(encoded) + '\n')
    (output / 'baseline.json').write_text(json.dumps(encoded))
    (output / 'profile.json').write_text(json.dumps(profile))
    (output / 'rules/data.bzl').write_text('PROFILE = json.decode(' + repr(json.dumps(profile)) + ')\n')
    (output / 'rules/catalog.bzl').write_text('ACTIONS = json.decode(' + repr(json.dumps(catalog)) + ')\n')
    programs = set()

    def walk(value):
        if isinstance(value, dict):
            if value.get('op') == 'function' and value.get('name') == 'kbuild_host_fallback':
                terms = value['arguments'][0]
                if any(term.get('op') != 'literal' for term in terms):
                    raise ValueError('Non-literal host capability program')
                programs.add(''.join(term['value'] for term in terms))
            for child in value.values():
                walk(child)
        elif isinstance(value, list):
            for child in value:
                walk(child)

    tree = ast.parse(program.read_text())
    walk(decode_literal(tree.body[0].value))
    (output / 'host-programs.json').write_text(json.dumps(sorted(programs)))
    for directory in [output, output / 'rules', output / 'native']:
        directory.mkdir(exist_ok=True)
        (directory / 'BUILD.bazel').write_text('exports_files(glob(["*"]))\n')
    (output / 'MODULE.bazel').write_text('module(name = "kernel_inputs")\n')
    (output / 'provenance.json').write_text(json.dumps({
        'inputs': {name: hashlib.sha256((generated / name).read_bytes()).hexdigest()
                   for name in ['options/BUILD.bazel', 'configs/BUILD.bazel', 'BUILD.bazel', 'rules/flag_program.bzl']},
        'kconfig_files': len(syntax), 'actions': len(catalog),
    }, indent=2) + '\n')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--linux', type=Path, default=REPOSITORY / '_tmp/linux')
    parser.add_argument('--autoconf', type=Path, default=REPOSITORY / '_tmp/autoconf-prototype')
    parser.add_argument('--generator', type=Path, default=REPOSITORY / '_tmp/kernel-repository/kernel-repository')
    parser.add_argument('--generated', type=Path, help='Existing static generator output; otherwise regenerate it')
    args = parser.parse_args()
    linux, autoconf = args.linux.resolve(), args.autoconf.resolve()
    if not (autoconf / 'autoconf.bzl').is_file():
        parser.error('--autoconf must point to the separate autoconf.bzl module checkout')
    provenance = json.loads((ROOT / 'native/provenance.json').read_text())
    for name, expected in provenance['sources'].items():
        if hashlib.sha256((linux / name).read_bytes()).hexdigest() != expected:
            raise ValueError('Native Kconfig source differs from the tested parser: ' + name)
    output = ROOT / '.generated'
    output.mkdir(exist_ok=True)
    generated = args.generated.resolve() if args.generated else output / 'export'
    if args.generated is None:
        # Generator only publishes into an empty directory. Keep its source tree
        # and module dependency outside that directory.
        if generated.exists():
            shutil.rmtree(generated)
        subprocess.run([str(args.generator.resolve()), 'generate', '--linux', str(linux), '--out', str(generated)], check=True)
    fixtures(generated, output)
    (output / 'setup.MODULE.bazel').write_text('''local_path_override(module_name = "autoconf_prototype", path = %r)
local_repository = use_repo_rule("@bazel_tools//tools/build_defs/repo:local.bzl", "local_repository")
new_local_repository = use_repo_rule("@bazel_tools//tools/build_defs/repo:local.bzl", "new_local_repository")
local_repository(name = "kernel_inputs", path = %r)
new_local_repository(name = "linux_source", path = %r, build_file_content = 'exports_files(["Makefile"])')
''' % (str(autoconf), str(output), str(linux)))
    (output / 'inputs.json').write_text(json.dumps({'linux': str(linux)}))
    subprocess.run([sys.executable, str(ROOT / 'native/build_parser.py'), '--linux', str(linux)], check=True)
    subprocess.run([sys.executable, str(ROOT / 'native/build_reference.py'), '--linux', str(linux)], check=True)
    print('Configured', ROOT)


if __name__ == '__main__':
    main()
