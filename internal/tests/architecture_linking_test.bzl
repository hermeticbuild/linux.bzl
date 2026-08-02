"""Tests for architecture-specific Linux ELF conventions."""

load("@bazel_skylib//lib:unittest.bzl", "analysistest", "asserts", "unittest")
load("//internal:architecture_linking.bzl", "linux_vmlinux_link_spec")
load("//internal:architectures.bzl", "linux_architectures")

def _architecture_linking_test_impl(ctx):
    env = unittest.begin(ctx)
    expected = {
        "arm": ("armelf_linux_eabi", True),
        "arm64": ("aarch64elf", True),
        "powerpc": ("elf64lppc", True),
        "riscv": ("elf64lriscv", True),
        "x86": ("elf_x86_64", False),
    }
    for arch, values in expected.items():
        spec = linux_vmlinux_link_spec(arch)
        asserts.equals(env, values[0], spec.emulation, arch)
        asserts.equals(env, values[1], spec.direct_lld, arch)
    asserts.true(env, "-Bstatic" in linux_vmlinux_link_spec("powerpc").vmlinux_flags)
    asserts.true(env, "norelro" in linux_vmlinux_link_spec("riscv").vmlinux_flags)
    descriptors = {descriptor.config_name: descriptor for descriptor in linux_architectures()}
    asserts.equals(env, "arm_zimage", descriptors["armv7"].compressed_format)
    asserts.equals(env, "zImage", descriptors["armv7"].extension)
    return unittest.end(env)

architecture_linking_test = unittest.make(_architecture_linking_test_impl)

def _unsupported_architecture_test_impl(ctx):
    env = analysistest.begin(ctx)
    asserts.expect_failure(env, "unsupported Linux vmlinux link architecture")
    return analysistest.end(env)

unsupported_architecture_test = analysistest.make(
    _unsupported_architecture_test_impl,
    expect_failure = True,
)

def _invalid_link_spec_impl(ctx):
    linux_vmlinux_link_spec(ctx.attr.arch)
    return []

invalid_link_spec = rule(
    implementation = _invalid_link_spec_impl,
    attrs = {"arch": attr.string(mandatory = True)},
)

def architecture_linking_test_suite(name):
    specs = name + "_specs"
    unsupported = name + "_unsupported"
    architecture_linking_test(name = specs)
    invalid_link_spec(
        name = name + "_invalid_target",
        arch = "sparc64",
        tags = ["manual"],
    )
    unsupported_architecture_test(
        name = unsupported,
        target_under_test = ":" + name + "_invalid_target",
    )
    native.test_suite(
        name = name,
        tests = [
            ":" + specs,
            ":" + unsupported,
        ],
    )
