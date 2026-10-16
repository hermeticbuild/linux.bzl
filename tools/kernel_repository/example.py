#!/usr/bin/env python3
"""Create a consumer workspace demonstrating the real repository rule.

Bazel repository rules cannot add module dependencies/toolchain registrations.
This relocates the generator's standalone MODULE template into the consumer's
root module, including the two root-only dependency patches.
"""
import argparse
import json
from pathlib import Path
import shutil

HERE = Path(__file__).resolve().parent


def create(linux, tool, generated, out):
    out.mkdir(parents=True, exist_ok=True)
    if any(out.iterdir()):
        raise ValueError('Example output directory must be empty: ' + str(out))
    shutil.copyfile(HERE / 'repository.bzl', out / 'repository.bzl')
    module = (generated / 'MODULE.bazel').read_text().split('\n', 1)[1]
    # All local labels in the maintained MODULE template refer to the generated
    # kernel repository. Module dependency patches must be in the consuming root.
    module = module.replace('"//', '"@kernel//')
    for name in ('rules_bison_cstdlib.patch', 'rules_cc_autoconf_no_host_shell.patch'):
        shutil.copyfile(generated / 'rules' / name, out / name)
        module = module.replace('@kernel//rules:' + name, '//:' + name)
    (out / 'dependencies.MODULE.bazel').write_text(module)
    all_backends = json.loads((generated / 'repository-tool.json').read_text())['all_backends']
    (out / 'MODULE.bazel').write_text('''module(name="kernel_repository_consumer")
new_local_repository = use_repo_rule("@bazel_tools//tools/build_defs/repo:local.bzl", "new_local_repository")
new_local_repository(name="linux_source", path=%r, build_file_content='exports_files(["Makefile"])')
new_local_repository(name="generator", path=%r, build_file_content=%r)
kernel_repository = use_repo_rule("//:repository.bzl", "kernel_repository")
kernel_repository(name="kernel", source="@linux_source//:Makefile", generator="@generator//:%s", all_backends=%s)
include("//:dependencies.MODULE.bazel")
''' % (str(linux), str(tool.parent), 'exports_files([' + repr(tool.name) + '])', tool.name, all_backends))
    (out / 'BUILD.bazel').write_text('exports_files(glob(["*.bzl", "*.patch"]))\n')
    (out / '.bazelversion').write_text('9.1.0\n')
    return out


if __name__ == '__main__':
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--linux', type=Path, default=Path('_tmp/linux'))
    p.add_argument('--tool', type=Path, default=Path('_tmp/kernel-repository/kernel-repository'))
    p.add_argument('--generated', type=Path, default=Path('_tmp/kernel-repository/repo'))
    p.add_argument('--out', type=Path, default=Path('_tmp/kernel-repository/example'))
    args = p.parse_args()
    print(create(args.linux.resolve(), args.tool.resolve(), args.generated.resolve(), args.out.resolve()))
