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
