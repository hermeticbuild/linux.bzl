#!/usr/bin/env python3
"""Generate the kernel prototype with every complete requested LLVM ELF backend."""
import argparse
import json
from pathlib import Path
import shutil
from metadata_support import configure as configure_compiler_metadata
from workspace_support import write_module

HERE = Path(__file__).resolve().parent


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--make', type=Path, required=True)
    parser.add_argument('--ast', type=Path, required=True)
    parser.add_argument('--linux', type=Path, default=Path('_tmp/linux'))
    parser.add_argument('--out', type=Path, default=Path('_tmp/kernel-backends/repo'))
    args = parser.parse_args(argv)
    import generate_image
    generate_image.main(['--make', str(args.make), '--ast', str(args.ast),
                         '--linux', str(args.linux), '--out', str(args.out)])
    repo = args.out.resolve()
    data = json.loads((HERE / 'llvm_backends.json').read_text())
    compilers = dict(data['build'], **data.get('compiler_only', {}))
    (repo / 'rules/llvm_backends.bzl').write_text('LLVM_BACKENDS = ' + repr(compilers) + '\nKERNEL_BACKENDS = ' + repr(sorted(data['build'])) + '\n')
    (repo / 'backend-status.json').write_text(json.dumps(data, indent=2) + '\n')
    for name in ['kernel_toolchains.bzl', 'backend_checks.bzl', 'backend_check.py']:
        shutil.copyfile(HERE / name, repo / 'rules' / name)
    configure_compiler_metadata(repo, "//kernel_toolchains:stage1_linux_x86_64")
    (repo / 'kernel_toolchains').mkdir()
    (repo / 'kernel_toolchains/BUILD.bazel').write_text('load("//rules:kernel_toolchains.bzl", "kernel_toolchains")\npackage(default_visibility=["//visibility:public"])\nkernel_toolchains()\n')
    (repo / 'backend_checks').mkdir()
    (repo / 'backend_checks/BUILD.bazel').write_text('load("//rules:backend_checks.bzl", "backend_checks")\nbackend_checks()\n')
    write_module(repo, data)
    (repo / 'rules/kernel_platform_data.bzl').write_text('SUPPORTED_KERNEL_CPUS = ' + repr(sorted(data['build'])) + '\nKERNEL_CONSTRAINTS = ["//kernel_toolchains:kernel"]\n')
    print(repo)


if __name__ == '__main__':
    main()
