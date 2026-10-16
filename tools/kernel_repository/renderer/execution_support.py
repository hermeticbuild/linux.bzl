"""Generate explicit utility registrations; no host utility discovery."""
from pathlib import Path
import shutil

HERE = Path(__file__).resolve().parent

MODULE = '''
bazel_dep(name="bazel_lib", version="3.7.2")
bazel_dep(name="rules_python", version="1.8.0")
bazel_dep(name="rules_perl", version="1.2.0")
bazel_dep(name="rules_bison", version="0.4.bcr.1")
bazel_dep(name="rules_flex", version="0.4.1")
bazel_dep(name="rules_m4", version="0.3.bcr.1")
bazel_dep(name="rules_gzip", version="1.0.0")
bazel_dep(name="rules_xz", version="1.0.1")
bazel_dep(name="rules_bzip2", version="1.0.0")
bazel_dep(name="gawk", version="5.4.0.bcr.3")
bazel_dep(name="sed", version="4.10.bcr.2")
bazel_dep(name="grep", version="3.12.bcr.1")
single_version_override(
    module_name="rules_bison",
    version="0.4.bcr.1",
    patches=["//rules:rules_bison_cstdlib.patch"],
    patch_strip=1,
)
single_version_override(
    module_name="rules_cc_autoconf",
    version="0.27.0",
    patches=["//rules:rules_cc_autoconf_no_host_shell.patch"],
    patch_strip=1,
)

kernel_http_file = use_repo_rule("@bazel_tools//tools/build_defs/repo:http.bzl", "http_file")
kernel_http_file(
    name="kernel_exec_bash_linux_x86_64",
    urls=["https://github.com/robxu9/bash-static/releases/download/5.2.015-1.2.3-2/bash-linux-x86_64"],
    sha256="64469a9512a00199c85622ec56f870f97d50457a4e06e0cfa39bae7adf0cc8f2",
    executable=True,
)
kernel_http_file(
    name="kernel_exec_busybox_linux_x86_64",
    urls=["https://busybox.net/downloads/binaries/1.35.0-x86_64-linux-musl/busybox"],
    sha256="6e123e7f3202a8c1e9b1f94d8941580a25135382b99e8d3e34fb858bba311348",
    executable=True,
)
register_toolchains("//execution_tools:linux_x86_64")
'''

BUILD = '''load("//rules:execution_tools.bzl", "kernel_execution_tools", "kernel_execution_tools_check")
package(default_visibility=["//visibility:public"])
toolchain_type(name="toolchain_type")
kernel_execution_tools_check(name="check")
kernel_execution_tools(
    name="linux_x86_64_tools",
    commands={
        "@kernel_exec_bash_linux_x86_64//file": "sh bash",
        "@kernel_exec_busybox_linux_x86_64//file": "bc hexdump find xargs diff cmp lzop cpio which",
        "@gawk//:gawk": "awk",
        "@sed//:sed": "sed",
        "@grep//:grep": "grep",
    },
    coreutils='[ arch b2sum base32 base64 basename basenc cat chgrp chmod chown chroot cksum comm cp csplit cut date dd df dir dircolors dirname du echo env expand expr factor false fmt fold groups hashsum head hostid hostname id install join kill link ln logname ls md5sum mkdir mkfifo mknod mktemp more mv nice nl nohup nproc numfmt od paste pathchk pinky pr printenv printf ptx pwd readlink realpath rm rmdir seq sha1sum sha224sum sha256sum sha384sum sha512sum shred shuf sleep sort split stat stty sum sync tac tail tee test timeout touch tr true truncate tsort tty uname unexpand uniq unlink uptime users vdir wc who whoami yes'.split(" "),
)
toolchain(
    name="linux_x86_64",
    toolchain=":linux_x86_64_tools",
    toolchain_type=":toolchain_type",
    exec_compatible_with=["@platforms//os:linux", "@platforms//cpu:x86_64"],
)
'''

def generate(repo):
    shutil.copyfile(HERE / 'execution_tools.bzl', repo / 'rules/execution_tools.bzl')
    for name in ['source_tree.bzl', 'prepare_source.py', 'host_libraries.bzl', 'rules_cc_autoconf_no_host_shell.patch', 'rules_bison_cstdlib.patch']:
        shutil.copyfile(HERE / name, repo / 'rules' / name)
    (repo / 'host_libraries').mkdir(exist_ok=True)
    (repo / 'host_libraries/BUILD.bazel').write_text('''load("//rules:host_libraries.bzl", "kernel_host_library")
kernel_host_library(name="libelf", library="@elfutils//:libelf", apis=["elf_getshdr"], visibility=["//visibility:public"])
''')
    with (repo / 'BUILD.bazel').open('a') as stream:
        stream.write('load("//rules:source_tree.bzl", "kernel_source_tree")\nkernel_source_tree(name="prepared_source", sources=":source_tree", anchor="source/Makefile")\n')
    (repo / 'execution_tools').mkdir(exist_ok=True)
    (repo / 'execution_tools/BUILD.bazel').write_text(BUILD)
