"""Render the complete workspace module from explicit generation inputs."""
from execution_support import MODULE as EXECUTION_MODULE
from metadata_support import MODULE as METADATA_MODULE
from qemu_support import MODULE as QEMU_MODULE


def write_module(repo, backends=None):
    text = '''module(name="kernel_llvm_actions")
bazel_dep(name="rules_cc", version="0.2.25")
bazel_dep(name="platforms", version="1.1.0")
bazel_dep(name="llvm", version="0.8.24")
bazel_dep(name="elfutils", version="0.195")
bazel_dep(name="bazel_skylib", version="1.9.0")
llvm = use_extension("@llvm//extensions:llvm.bzl", "llvm")
'''
    if backends:
        compilers = dict(backends['build'], **backends.get('compiler_only', {}))
        text += 'llvm.configure(targets=' + repr(sorted({v['backend'] for v in compilers.values()})) + ')\n'
        text += 'use_repo(llvm, "llvm-project")\n'
        text += 'register_toolchains("//kernel_toolchains:all", "@llvm//toolchain:all")\n'
    else:
        text += 'use_repo(llvm, "llvm-project")\nregister_toolchains("@llvm//toolchain:all")\n'
    (repo / 'MODULE.bazel').write_text(text + EXECUTION_MODULE + METADATA_MODULE + QEMU_MODULE)
