"""Analysis tests for Linux compile flag preservation."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts")
load("//internal:linux_objects.bzl", "LinuxCcContextInfo")

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
