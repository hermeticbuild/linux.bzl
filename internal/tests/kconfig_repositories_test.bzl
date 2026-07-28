"""Tests for kconfig repository platform selection."""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load("//internal:kconfig_tool_filename.bzl", "kconfig_tool_filename")
load("//internal:linux_image_repository.bzl", "repositories_test_helpers")

visibility("private")

def _kconfig_tool_filename_test_impl(ctx):
    env = unittest.begin(ctx)
    asserts.equals(env, "kconfig.exe", kconfig_tool_filename("windows_amd64", "kconfig"))
    asserts.equals(env, "kconfig_parse.exe", kconfig_tool_filename("windows_amd64", "kconfig_parse"))
    asserts.equals(env, "kconfig", kconfig_tool_filename("linux_amd64", "kconfig"))
    return unittest.end(env)

kconfig_tool_filename_test = unittest.make(_kconfig_tool_filename_test_impl)

def _graph_config_args_test_impl(ctx):
    env = unittest.begin(ctx)
    asserts.equals(
        env,
        [
            "-config",
            "aarch64=configs/base.config",
            "-config_mode",
            "allnoconfig",
        ],
        repositories_test_helpers.graph_config_args(
            "aarch64",
            "configs/base.config",
            "allnoconfig",
        ),
    )
    return unittest.end(env)

graph_config_args_test = unittest.make(_graph_config_args_test_impl)

def _graph_arch_tool_args_test_impl(ctx):
    env = unittest.begin(ctx)
    asserts.equals(
        env,
        [
            "-source_objtool",
            "//:_base_x86_objtool",
        ],
        repositories_test_helpers.graph_arch_tool_args("x86_64", "_base"),
    )
    asserts.equals(
        env,
        [
            "-source_relacheck",
            "//:_variant_debug_relacheck_tool",
        ],
        repositories_test_helpers.graph_arch_tool_args("aarch64", "_variant_debug"),
    )
    return unittest.end(env)

graph_arch_tool_args_test = unittest.make(_graph_arch_tool_args_test_impl)

def _without_rust_toolchain_config_test_impl(ctx):
    env = unittest.begin(ctx)
    asserts.equals(
        env,
        {
            "CONFIG_CFI_CLANG": "y",
            "CONFIG_RUST": "y",
        },
        repositories_test_helpers.without_rust_toolchain_config({
            "CONFIG_CFI_CLANG": "y",
            "CONFIG_HAVE_CFI_ICALL_NORMALIZE_INTEGERS_RUSTC": "y",
            "CONFIG_RUST": "y",
            "CONFIG_RUSTC_HAS_COERCE_POINTEE": "y",
            "CONFIG_RUSTC_LLVM_VERSION": "220106",
            "CONFIG_RUSTC_VERSION": "109700",
            "CONFIG_RUSTC_VERSION_TEXT": "rustc 1.97.0-nightly",
            "CONFIG_RUST_IS_AVAILABLE": "y",
        }),
    )
    return unittest.end(env)

without_rust_toolchain_config_test = unittest.make(_without_rust_toolchain_config_test_impl)
