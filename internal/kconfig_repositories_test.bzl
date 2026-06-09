"""Tests for kconfig repository platform selection."""

load("@bazel_skylib//lib:unittest.bzl", "asserts", "unittest")
load(":kconfig_repositories.bzl", "kconfig_host_platform", "kconfig_tool_filename")

def _kconfig_host_platform_test_impl(ctx):
    env = unittest.begin(ctx)
    asserts.equals(env, "windows_amd64", kconfig_host_platform("windows server 2025", "amd64"))
    asserts.equals(env, "windows_amd64", kconfig_host_platform("windows", "x86_64"))
    asserts.equals(env, "darwin_arm64", kconfig_host_platform("mac os x", "aarch64"))
    asserts.equals(env, "linux_amd64", kconfig_host_platform("linux", "amd64"))
    asserts.equals(env, None, kconfig_host_platform("freebsd", "amd64"))
    return unittest.end(env)

kconfig_host_platform_test = unittest.make(_kconfig_host_platform_test_impl)

def _kconfig_tool_filename_test_impl(ctx):
    env = unittest.begin(ctx)
    asserts.equals(env, "kconfig.exe", kconfig_tool_filename("windows_amd64", "kconfig"))
    asserts.equals(env, "kconfig_parse.exe", kconfig_tool_filename("windows_amd64", "kconfig_parse"))
    asserts.equals(env, "kconfig", kconfig_tool_filename("linux_amd64", "kconfig"))
    return unittest.end(env)

kconfig_tool_filename_test = unittest.make(_kconfig_tool_filename_test_impl)
