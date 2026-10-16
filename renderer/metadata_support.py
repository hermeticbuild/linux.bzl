"""Write explicit LLVM metadata adapters alongside the generated build graph."""
from pathlib import Path
import shutil

HERE = Path(__file__).resolve().parent

MODULE = '''
kernel_compiler_versions = use_repo_rule("//rules:compiler_versions_repo.bzl", "llvm_compiler_versions")
kernel_compiler_versions(name="kernel_llvm_versions")
'''


def configure(repo, kernel_prefix=None):
    build = 'load("//rules:llvm_metadata.bzl", "llvm_metadata")\n'
    build += 'llvm_metadata(' + ('kernel_prefix=' + repr(kernel_prefix) + ', kernel_toolchain="//kernel_toolchains:compiler"' if kernel_prefix else '') + ')\n'
    build += 'alias(name="metadata", actual=":llvm", visibility=["//visibility:public"])\n'
    (repo / 'compiler_metadata').mkdir(exist_ok=True)
    (repo / 'compiler_metadata/BUILD.bazel').write_text(build)


def generate(repo):
    for name in ['compiler_metadata.bzl', 'compiler_versions_repo.bzl', 'llvm_metadata.bzl', 'compiler_metadata_checks.bzl', 'check_compiler_metadata.py']:
        shutil.copyfile(HERE / name, repo / 'rules' / name)
    configure(repo)
    with (repo / 'architecture_tests/BUILD.bazel').open('a') as stream:
        stream.write('''load("//rules:compiler_metadata_checks.bzl", "kernel_compiler_metadata_check", "kernel_compiler_metadata_test")
load("//rules:multi_platform.bzl", "kernel_multi_platform")
kernel_compiler_metadata_check(name="compiler_metadata_check", tags=["manual"])
kernel_multi_platform(name="compiler_metadata_checks", target=":compiler_metadata_check")
kernel_compiler_metadata_test(name="compiler_metadata_test", checks=":compiler_metadata_checks")
''')
