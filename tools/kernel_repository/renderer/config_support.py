#!/usr/bin/env python3
"""Reuse the grouped Kconfig renderer/solver, without the old Kbuild parser."""
import argparse
import json
from pathlib import Path
import shutil


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--syntax', type=Path, required=True)
    parser.add_argument('--repo', type=Path, required=True)
    parser.add_argument('--linux', type=Path, required=True)
    args = parser.parse_args(argv)
    syntax = json.loads(args.syntax.read_text())
    (args.repo / 'options').mkdir(exist_ok=True)
    (args.repo / 'options/BUILD.bazel').write_text('load("//rules:rules.bzl", "kernel_option_sources")\npackage(default_visibility=["//visibility:public"])\nkernel_option_sources(name="options", files=json.decode(' + repr(json.dumps({p: json.dumps(body) for p, body in syntax.items()})) + '))\n')
    here = Path(__file__).resolve().parent
    shutil.copyfile(here / 'compiler_metadata.bzl', args.repo / 'rules/compiler_metadata.bzl')
    (args.repo / 'compiler_metadata').mkdir(exist_ok=True)
    (args.repo / 'compiler_metadata/BUILD.bazel').write_text('load("//rules:compiler_metadata.bzl", "legacy_compiler_metadata")\nlegacy_compiler_metadata(name="metadata", visibility=["//visibility:public"])\n')
    with (args.repo / 'BUILD.bazel').open('a') as stream:
        stream.write('label_flag(name="compiler_metadata", build_setting_default="//compiler_metadata:metadata")\n')
    (args.repo / 'kernel_modes').mkdir(exist_ok=True)
    (args.repo / 'kernel_modes/BUILD.bazel').write_text('''package(default_visibility=["//visibility:public"])
constraint_setting(name="mode", default_constraint_value=":native")
constraint_value(name="native", constraint_setting=":mode")
constraint_value(name="usermode", constraint_setting=":mode")
''')
    shutil.copyfile(here / 'compiler_capabilities.bzl', args.repo / 'rules/compiler_capabilities.bzl')
    (args.repo / 'rules/compiler_capabilities_data.bzl').write_text('COMPILER_CAPABILITIES = json.decode(' + repr((here / 'compiler_capabilities.json').read_text()) + ')\n')
    with (args.repo / 'BUILD.bazel').open('a') as stream:
        stream.write('load("//rules:compiler_capabilities.bzl", "kernel_compiler_capabilities")\nkernel_compiler_capabilities(name="compiler_capabilities")\n')
    shutil.copyfile(here / 'kconfig_link.bzl', args.repo / 'rules/kconfig_link.bzl')
    shutil.copyfile(here / 'architecture.bzl', args.repo / 'rules/architecture.bzl')
    (args.repo / 'rules/architecture_data.bzl').write_text('CPU_ARCHITECTURES = ' + (here / 'architecture_data.json').read_text() + '\n')
    with (args.repo / 'BUILD.bazel').open('a') as stream:
        stream.write('load("//rules:architecture.bzl", "kernel_architecture")\nkernel_architecture(name="architecture", visibility=["//visibility:public"])\n')
    shutil.copyfile(here / 'solver.bzl', args.repo / 'rules/solver.bzl')
    shutil.copyfile(Path(__file__).resolve().parent / 'config.bzl', args.repo / 'rules/rules.bzl')
    shutil.copyfile(Path(__file__).resolve().parent / 'compiler_inputs.bzl', args.repo / 'rules/compiler_inputs.bzl')


if __name__ == '__main__':
    main()
