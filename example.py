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
    """Configure the maintained examples without replacing their BUILD/config files."""
    if out != (HERE / 'examples').resolve():
        raise ValueError('Configure the maintained examples directory: ' + str(HERE / 'examples'))
    support = out / '.generated'
    support.mkdir(parents=True, exist_ok=True)
    module = (generated / 'MODULE.bazel').read_text().split('\n', 1)[1]
    module = module.replace('"//', '"@kernel//')
    for name in ('rules_bison_cstdlib.patch', 'rules_cc_autoconf_no_host_shell.patch'):
        shutil.copyfile(generated / 'rules' / name, support / name)
        module = module.replace('@kernel//rules:' + name, '//:.generated/' + name)
    (support / 'dependencies.MODULE.bazel').write_text(module)
    all_backends = json.loads((generated / 'repository-tool.json').read_text())['all_backends']
    fragments = ['//configs:' + path.name for path in sorted((out / 'configs').glob('*.config'))]
    (support / 'setup.MODULE.bazel').write_text('''new_local_repository = use_repo_rule("@bazel_tools//tools/build_defs/repo:local.bzl", "new_local_repository")
local_repository = use_repo_rule("@bazel_tools//tools/build_defs/repo:local.bzl", "local_repository")
local_repository(name="prototype", path=%r)
new_local_repository(name="linux_source", path=%r, build_file_content='exports_files(["Makefile"])')
new_local_repository(name="generator", path=%r, build_file_content=%r)
kernel_repository = use_repo_rule("@prototype//:repository.bzl", "kernel_repository")
kernel_repository(name="kernel", source="@linux_source//:Makefile", generator="@generator//:%s", all_backends=%s, config_fragments=%r)
include("//:.generated/dependencies.MODULE.bazel")
''' % (str(HERE), str(linux), str(tool.parent), 'exports_files([' + repr(tool.name) + '])', tool.name, all_backends, fragments))
    return out


if __name__ == '__main__':
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument('--linux', type=Path, default=Path('_tmp/linux'))
    p.add_argument('--tool', type=Path, default=Path('_tmp/kernel-repository/kernel-repository'))
    p.add_argument('--generated', type=Path, default=Path('_tmp/kernel-repository/repo'))
    p.add_argument('--out', type=Path, default=HERE / 'examples')
    args = p.parse_args()
    print(create(args.linux.resolve(), args.tool.resolve(), args.generated.resolve(), args.out.resolve()))
