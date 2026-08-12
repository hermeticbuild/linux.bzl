"""Tests for Linux compile flag preservation and family adjustment."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts", "unittest")
load("//internal:linux_objects.bzl", "LinuxCcContextInfo", "linux_module_cc_helpers")

visibility("private")

_REPRODUCIBLE_FLAGS = [
    "-Wno-builtin-macro-redefined",
    "-D__DATE__=\"redacted\"",
    "-D__TIME__=\"redacted\"",
    "-D__TIMESTAMP__=\"redacted\"",
    "-ffile-compilation-dir=.",
]

def _linux_compile_flags_test_impl(ctx):
    env = analysistest.begin(ctx)
    target = analysistest.target_under_test(env)
    flags = target[LinuxCcContextInfo].compile_flags

    for flag in _REPRODUCIBLE_FLAGS:
        asserts.true(
            env,
            flag in flags,
            "expected reproducible compile flag %s in %s" % (flag, flags),
        )

    resource_includes = [
        flag
        for flag in flags
        if "/lib/clang/" in flag.replace("\\", "/") and flag.replace("\\", "/").endswith("/include")
    ]
    asserts.equals(env, 1, len(resource_includes))
    asserts.true(env, "-internal-isystem" in flags)
    asserts.false(env, any([
        "glibc_headers" in flag or "musl_libc" in flag
        for flag in flags
    ]))

    return analysistest.end(env)

linux_compile_flags_test = analysistest.make(_linux_compile_flags_test_impl)

def _compiler_adjusted_flags_test_impl(ctx):
    env = unittest.begin(ctx)
    clang_flags = [
        "-fno-addrsig",
        "-meabi",
        "gnu",
        "-mstack-alignment=4",
        "-mstack-alignment=8",
        "-mretpoline-external-thunk",
        "-mretpoline",
        "-Wno-gnu",
        "-Wno-unused-command-line-argument",
        "-O2",
    ]
    asserts.equals(
        env,
        clang_flags,
        linux_module_cc_helpers.compiler_adjusted_kbuild_flags(
            clang_flags,
            struct(compiler = "clang"),
        ),
    )
    asserts.equals(
        env,
        [
            "-mpreferred-stack-boundary=2",
            "-mpreferred-stack-boundary=3",
            "-mindirect-branch=thunk-extern",
            "-mindirect-branch-register",
            "-fno-jump-tables",
            "-mindirect-branch=thunk-inline",
            "-mindirect-branch-register",
            "-O2",
        ],
        linux_module_cc_helpers.compiler_adjusted_kbuild_flags(
            clang_flags,
            struct(compiler = "gcc"),
        ),
    )
    genksyms_flags = [
        "-mstack-alignment=8",
        "-mretpoline-external-thunk",
        "-D__GENKSYMS__",
    ]
    asserts.equals(
        env,
        [
            "-mpreferred-stack-boundary=3",
            "-mindirect-branch=thunk-extern",
            "-mindirect-branch-register",
            "-fno-jump-tables",
            "-D__GENKSYMS__",
        ],
        linux_module_cc_helpers.compiler_adjusted_kbuild_flags(
            genksyms_flags,
            struct(compiler = "gcc"),
        ),
    )
    return unittest.end(env)

compiler_adjusted_flags_test = unittest.make(_compiler_adjusted_flags_test_impl)
